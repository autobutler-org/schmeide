package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// maxIssuesWebhookBytes caps an issues event's body. GitHub caps an issue
	// body at 65,536 characters, so a real issues payload is far smaller. The
	// body is read before its signature can be checked, so the cap bounds
	// what an unsigned request can make the control node hold in memory.
	maxIssuesWebhookBytes = 1 << 20
	// maxOtherWebhookBytes caps every other event's body. Only issues events
	// make jobs; the rest are verified and ignored.
	maxOtherWebhookBytes = 64 << 10
	// webhookReadHeaderTimeout bounds how long a client may take to send headers.
	webhookReadHeaderTimeout = 10 * time.Second
	// webhookReadTimeout bounds how long a client may take to send the whole
	// request, so a slow sender cannot hold the connection open.
	webhookReadTimeout = 30 * time.Second
)

// WebhookPolicy is the in-memory limit on the webhook handler.
type WebhookPolicy struct {
	// RequestsPerMinute is how many requests are accepted per minute.
	// Zero or less disables the limit. The command rejects a non-positive value.
	RequestsPerMinute int
	// DeliveryTTL is how long an X-Github-Delivery id is remembered. A repeat
	// inside the window is acknowledged and not published again. Zero or less
	// disables replay detection. An empty delivery id is never a replay.
	DeliveryTTL time.Duration
	// now is the clock. Nil means time.Now. Tests set it.
	now func() time.Time
}

// Config is everything the control node needs to run.
type Config struct {
	// HTTPAddr is the host:port the webhook endpoint listens on.
	HTTPAddr string
	// NATSAddr is the host:port runners connect to.
	NATSAddr string
	// RunnerTokens is a comma-separated list of "<steward>:<token>" entries:
	// the token each steward's runners present to NATS.
	RunnerTokens string
	// DataDir is where JetStream keeps the job queue.
	DataDir string
	// WebhookSecret is the GitHub App's webhook secret.
	WebhookSecret string
	// ReadyLabel is the label that marks an issue ready.
	ReadyLabel string
	// AllowedOrgs is a comma-separated list of repository owners that may
	// create jobs.
	AllowedOrgs string
	// AllowedInstallations is a comma-separated list of GitHub App installation
	// IDs that may create jobs.
	AllowedInstallations string
	// GitHubAppID is the GitHub App's ID.
	GitHubAppID string
	// GitHubAppPrivateKeyFile is the path of the GitHub App's PEM private key.
	GitHubAppPrivateKeyFile string
	// GitHubAPIURL is the GitHub REST API base URL.
	GitHubAPIURL string
	// WebhookRate is the maximum number of webhook requests accepted per minute.
	WebhookRate string
	// DeliveryTTL is how long an X-Github-Delivery id is remembered.
	DeliveryTTL string
	// NATSTLSCertFile and NATSTLSKeyFile are the PEM certificate and key the
	// NATS listener serves. Both or neither are set.
	NATSTLSCertFile string
	NATSTLSKeyFile  string
	// AllowPlaintextNATS is "true" when NATS may listen beyond loopback
	// without TLS. Empty means false.
	AllowPlaintextNATS string
}

// Handler is the webhook endpoint. It caps the body by event, rate-limits requests,
// drops replays of an X-Github-Delivery id, hands the delivery to Intake, and
// maps the outcome to a status code.
func Handler(in Intake, log *slog.Logger, policy WebhookPolicy) http.Handler {
	gate := newWebhookGate(policy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "webhook deliveries must be POSTed", http.StatusMethodNotAllowed)
			return
		}
		now := gate.now()
		delivery := r.Header.Get("X-Github-Delivery")
		if gate.replay(delivery, now) {
			log.Info("webhook handled", "delivery", delivery, "outcome", string(OutcomeReplay))
			fmt.Fprintln(w, OutcomeReplay)
			return
		}
		if !gate.allow(now) {
			log.Warn("webhook rejected", "delivery", delivery, "err", errRateLimited)
			http.Error(w, errRateLimited.Error(), http.StatusTooManyRequests)
			return
		}
		event := r.Header.Get("X-Github-Event")
		limit := int64(maxOtherWebhookBytes)
		if event == "issues" {
			limit = maxIssuesWebhookBytes
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Warn("webhook rejected", "delivery", delivery, "event", event, "err", fmt.Errorf("payload is larger than %d bytes", limit))
			http.Error(w, fmt.Sprintf("a %s payload may be at most %d bytes", event, limit), http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "could not read the payload", http.StatusBadRequest)
			return
		}
		outcome, err := in.Handle(r.Context(), event, r.Header.Get("X-Hub-Signature-256"), body)
		switch {
		case errors.Is(err, ErrBadSignature):
			log.Warn("webhook rejected", "delivery", delivery, "err", err)
			http.Error(w, err.Error(), http.StatusUnauthorized)
		case errors.Is(err, ErrNotAllowed):
			log.Warn("webhook rejected", "delivery", delivery, "err", err)
			http.Error(w, err.Error(), http.StatusForbidden)
		case err != nil:
			log.Error("webhook failed", "delivery", delivery, "err", err)
			http.Error(w, "could not process the delivery", http.StatusInternalServerError)
		default:
			gate.remember(delivery, now)
			log.Info("webhook handled", "delivery", delivery, "outcome", string(outcome))
			if outcome == OutcomeCreated {
				w.WriteHeader(http.StatusAccepted)
			}
			fmt.Fprintln(w, outcome)
		}
	})
}

// parseWebhookRate reads the --webhook-rate value, a positive count per minute.
func parseWebhookRate(value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("webhook rate %q is not a positive number of requests per minute; pass --webhook-rate or set SCHMEIDE_WEBHOOK_RATE", value)
	}
	return n, nil
}

// parseDeliveryTTL reads the --delivery-ttl value, a positive duration.
func parseDeliveryTTL(value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("delivery ttl %q is not a positive duration (for example 10m); pass --delivery-ttl or set SCHMEIDE_DELIVERY_TTL", value)
	}
	return d, nil
}

// parseAllowPlaintextNATS reads the --allow-plaintext-nats value. Empty
// means false.
func parseAllowPlaintextNATS(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	allow, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("allow plaintext nats %q is not true or false; pass --allow-plaintext-nats or set SCHMEIDE_ALLOW_PLAINTEXT_NATS", value)
	}
	return allow, nil
}

// Run starts the embedded NATS server and the webhook listener and blocks
// until ctx is canceled or either one fails.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	allow, err := ParseAllowlist(cfg.AllowedOrgs, cfg.AllowedInstallations)
	if err != nil {
		return err
	}
	runnerTokens, err := ParseRunnerTokens(cfg.RunnerTokens)
	if err != nil {
		return err
	}
	rate, err := parseWebhookRate(cfg.WebhookRate)
	if err != nil {
		return err
	}
	ttl, err := parseDeliveryTTL(cfg.DeliveryTTL)
	if err != nil {
		return err
	}
	allowPlaintext, err := parseAllowPlaintextNATS(cfg.AllowPlaintextNATS)
	if err != nil {
		return err
	}
	natsTLS := cfg.NATSTLSCertFile != ""
	if err := checkNATSTransport(cfg.NATSAddr, natsTLS, allowPlaintext); err != nil {
		return err
	}
	pemBytes, err := os.ReadFile(cfg.GitHubAppPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read the GitHub App private key: %w", err)
	}
	key, err := ParsePrivateKey(pemBytes)
	if err != nil {
		return fmt.Errorf("GitHub App private key %s: %w", cfg.GitHubAppPrivateKeyFile, err)
	}
	minter := TokenMinter{AppID: cfg.GitHubAppID, Key: key, APIURL: cfg.GitHubAPIURL}

	if !natsTLS && listensBeyondLoopback(cfg.NATSAddr) {
		log.Warn("NATS listens beyond loopback without TLS because --allow-plaintext-nats is set; steward tokens and job tokens cross the network in plain text. Set --nats-tls-cert-file and --nats-tls-key-file", "nats", cfg.NATSAddr)
	}
	queue, err := StartQueue(ctx, QueueOptions{
		Addr:         cfg.NATSAddr,
		RunnerTokens: runnerTokens,
		DataDir:      cfg.DataDir,
		TLSCertFile:  cfg.NATSTLSCertFile,
		TLSKeyFile:   cfg.NATSTLSKeyFile,
	})
	if err != nil {
		return err
	}
	defer queue.Close()
	if err := queue.ServeTokens(minter.Mint, log); err != nil {
		return fmt.Errorf("serve job tokens: %w", err)
	}
	if _, err := queue.Track(log); err != nil {
		return fmt.Errorf("track runners and job states: %w", err)
	}
	if err := queue.ServeStatus(log); err != nil {
		return fmt.Errorf("serve status queries: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", Handler(Intake{
		Secret:     []byte(cfg.WebhookSecret),
		ReadyLabel: cfg.ReadyLabel,
		Allow:      allow,
		Publish:    queue.Publish,
	}, log, WebhookPolicy{RequestsPerMinute: rate, DeliveryTTL: ttl}))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for webhooks on %s: %w", cfg.HTTPAddr, err)
	}
	srv := newWebhookServer(mux)
	log.Info("control node started",
		"webhook", "http://"+listener.Addr().String()+"/webhook",
		"listener", "plain HTTP; must sit behind a TLS-terminating proxy",
		"nats", cfg.NATSAddr, "nats_tls", natsTLS, "ready_label", cfg.ReadyLabel)

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(listener) }()
	select {
	case err := <-failed:
		return fmt.Errorf("webhook listener stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newWebhookServer builds the plain HTTP webhook listener.
// ReadTimeout bounds the whole request so a slow client cannot hold the
// connection open. IdleTimeout is left unset, so it follows ReadTimeout:
// a keep-alive connection that sends nothing is closed too. Shutdown is
// unchanged.
func newWebhookServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: webhookReadHeaderTimeout,
		ReadTimeout:       webhookReadTimeout,
	}
}
