package control

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultGitHubAPIURL is the GitHub REST API base URL.
const DefaultGitHubAPIURL = "https://api.github.com"

// Token is a short-lived GitHub App installation token for one job.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TokenMinter mints GitHub App installation tokens.
type TokenMinter struct {
	// AppID is the GitHub App's ID (or client ID), used as the JWT issuer.
	AppID string
	// Key is the GitHub App's private key.
	Key *rsa.PrivateKey
	// APIURL is the GitHub REST API base URL; empty means DefaultGitHubAPIURL.
	APIURL string
	// Client is the HTTP client to use; nil means http.DefaultClient.
	Client *http.Client
}

// ParsePrivateKey parses a GitHub App private key in PEM form.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an RSA key")
	}
	return key, nil
}

// appJWT signs the RS256 JWT that authenticates as the App itself.
func (m TokenMinter) appJWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	claims, err := json.Marshal(map[string]any{
		"iss": m.AppID,
		"iat": now.Add(-time.Minute).Unix(), // allow for clock drift, as GitHub advises
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Mint returns an installation token for the job, limited to the job's
// repository and to what a runner needs to push a branch, open a PR, and
// list check runs. GitHub expires installation tokens after one hour.
func (m TokenMinter) Mint(ctx context.Context, job Job) (Token, error) {
	jwt, err := m.appJWT(time.Now())
	if err != nil {
		return Token{}, err
	}
	reqBody, err := json.Marshal(map[string]any{
		"repository_ids": []int64{job.RepoID},
		"permissions": map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"checks":        "read",
		},
	})
	if err != nil {
		return Token{}, err
	}
	apiURL := m.APIURL
	if apiURL == "" {
		apiURL = DefaultGitHubAPIURL
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiURL, job.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("request installation token: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("read installation token response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return Token{}, fmt.Errorf("github refused an installation token for installation %d: %s: %s",
			job.InstallationID, resp.Status, respBody)
	}
	var token Token
	if err := json.Unmarshal(respBody, &token); err != nil {
		return Token{}, fmt.Errorf("decode installation token: %w", err)
	}
	return token, nil
}
