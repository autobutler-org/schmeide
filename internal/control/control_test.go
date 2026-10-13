package control

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSecret = "it's a secret to everybody"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func payload(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func startQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := StartQueue(t.Context(), QueueOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q
}

// jobs returns every job in the stream.
func jobs(t *testing.T, q *Queue) []Job {
	t.Helper()
	info, err := q.stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []Job
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		msg, err := q.stream.GetMsg(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		var job Job
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			t.Fatal(err)
		}
		if msg.Subject != job.Subject() {
			t.Errorf("job stored on %q, want %q", msg.Subject, job.Subject())
		}
		got = append(got, job)
	}
	return got
}

type delivery struct {
	event     string // defaults to "issues"
	body      []byte
	signature string
}

// deliver posts one webhook to the handler and returns the status code.
func deliver(t *testing.T, h http.Handler, d delivery) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(d.body))
	if d.event == "" {
		d.event = "issues"
	}
	req.Header.Set("X-Github-Event", d.event)
	if d.signature != "" {
		req.Header.Set("X-Hub-Signature-256", d.signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhookIntake(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	want := Job{Repo: "autobutler-org/quark", RepoID: 901234567, Issue: 42, Steward: "octocat", InstallationID: 61234567}
	// The same issue after it was reassigned: still the same job.
	reassigned := bytes.ReplaceAll(ready, []byte(`"octocat"`), []byte(`"hubot"`))
	unassigned := payload(t, "issues_labeled_unassigned.json")
	unlabeled := payload(t, "issues_assigned_unlabeled.json")
	closed := bytes.Replace(ready, []byte(`"state": "open"`), []byte(`"state": "closed"`), 1)

	tests := []struct {
		name       string
		deliveries []delivery
		wantCodes  []int
		wantJobs   []Job
	}{
		{
			name:       "assigned and labeled creates one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusAccepted},
			wantJobs:   []Job{want},
		},
		{
			name:       "duplicate delivery creates one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}, {body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusAccepted, http.StatusOK},
			wantJobs:   []Job{want},
		},
		{
			name:       "reassigned issue keeps its one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}, {body: reassigned, signature: sign(reassigned)}},
			wantCodes:  []int{http.StatusAccepted, http.StatusOK},
			wantJobs:   []Job{want},
		},
		{
			name:       "labeled but unassigned creates no job",
			deliveries: []delivery{{body: unassigned, signature: sign(unassigned)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "assigned but unlabeled creates no job",
			deliveries: []delivery{{body: unlabeled, signature: sign(unlabeled)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "closed issue creates no job",
			deliveries: []delivery{{body: closed, signature: sign(closed)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "other events are ignored",
			deliveries: []delivery{{event: "issue_comment", body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "missing signature is rejected",
			deliveries: []delivery{{body: ready}},
			wantCodes:  []int{http.StatusUnauthorized},
		},
		{
			name: "bad signature is rejected",
			deliveries: []delivery{
				{body: ready, signature: sign(unlabeled)},
				{body: ready, signature: "sha256=nothex"},
				{body: ready, signature: strings.TrimPrefix(sign(ready), "sha256=")},
			},
			wantCodes: []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusUnauthorized},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := startQueue(t)
			h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Publish: q.Publish}, slog.New(slog.DiscardHandler))
			for i, d := range tt.deliveries {
				if got := deliver(t, h, d); got != tt.wantCodes[i] {
					t.Errorf("delivery %d: status = %d, want %d", i, got, tt.wantCodes[i])
				}
			}
			got := jobs(t, q)
			if len(got) != len(tt.wantJobs) {
				t.Fatalf("jobs = %+v, want %+v", got, tt.wantJobs)
			}
			for i := range got {
				if got[i] != tt.wantJobs[i] {
					t.Errorf("job %d = %+v, want %+v", i, got[i], tt.wantJobs[i])
				}
			}
		})
	}
}

func TestReadyLabelIsConfigurable(t *testing.T) {
	body := payload(t, "issues_assigned_labeled.json")
	job, err := JobFromIssuesEvent(body, "enhancement")
	if err != nil || job == nil {
		t.Fatalf("job = %v, err = %v, want a job for the configured label", job, err)
	}
	if job, err = JobFromIssuesEvent(body, "other:ready"); err != nil || job != nil {
		t.Fatalf("job = %v, err = %v, want no job without the configured label", job, err)
	}
}

// fakeGitHub is a GitHub API that issues one installation token after checking
// the App JWT against the App's public key.
func fakeGitHub(t *testing.T, key *rsa.PublicKey, expires time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/61234567/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatalf("Authorization = %q, want a bearer JWT", r.Header.Get("Authorization"))
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("app JWT signature: %v", err)
		}
		var claims struct {
			Iss      string `json:"iss"`
			Iat, Exp int64
		}
		claimBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if err := json.Unmarshal(claimBytes, &claims); err != nil {
			t.Errorf("app JWT claims: %v", err)
		}
		if now := time.Now().Unix(); claims.Iss != "12345" || claims.Iat > now || claims.Exp <= now || claims.Exp > now+600 {
			t.Errorf("app JWT claims = %+v, want issuer 12345 and a lifetime within GitHub's 10 minute cap", claims)
		}
		var body struct {
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		reqBody, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(reqBody, &body); err != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 901234567 {
			t.Errorf("token request body = %s, want it limited to the job's repository", reqBody)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Token{Token: "ghs_fake", ExpiresAt: expires})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenPerJob(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	gh := fakeGitHub(t, &key.PublicKey, expires)
	minter := TokenMinter{AppID: "12345", Key: parsed, APIURL: gh.URL}

	q := startQueue(t)
	job := Job{Repo: "autobutler-org/quark", RepoID: 901234567, Issue: 42, Steward: "octocat", InstallationID: 61234567}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	if err := q.ServeTokens(minter.Mint, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	request := func(subject string) TokenReply {
		t.Helper()
		msg, err := q.nc.Request(TokenSubject, []byte(subject), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var reply TokenReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if got := request(job.Subject()); got.Error != "" || got.Token.Token != "ghs_fake" || !got.ExpiresAt.Equal(expires) {
		t.Errorf("token reply = %+v, want ghs_fake expiring at %s", got, expires)
	}
	for _, subject := range []string{"schmeide.jobs.octocat.901234567.43", "schmeide.jobs.>", "elsewhere"} {
		if got := request(subject); got.Error == "" || got.Token.Token != "" {
			t.Errorf("token reply for %q = %+v, want an error and no token", subject, got)
		}
	}
}

func TestMintReportsGitHubRefusal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
	}))
	defer gh.Close()
	_, err = TokenMinter{AppID: "1", Key: key, APIURL: gh.URL}.Mint(context.Background(), Job{InstallationID: 7})
	if err == nil || !strings.Contains(err.Error(), "Integration not found") {
		t.Errorf("err = %v, want GitHub's message", err)
	}
}
