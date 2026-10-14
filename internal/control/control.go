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
	"time"
)

// maxWebhookBytes is GitHub's cap on webhook payload size.
const maxWebhookBytes = 25 << 20

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
}

// Handler is the webhook endpoint. It only extracts the delivery from the
// request, hands it to Intake, and maps the outcome to a status code.
func Handler(in Intake, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "webhook deliveries must be POSTed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
		if err != nil {
			http.Error(w, "could not read the payload", http.StatusBadRequest)
			return
		}
		delivery := r.Header.Get("X-Github-Delivery")
		outcome, err := in.Handle(r.Context(), r.Header.Get("X-Github-Event"), r.Header.Get("X-Hub-Signature-256"), body)
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
			log.Info("webhook handled", "delivery", delivery, "outcome", string(outcome))
			if outcome == OutcomeCreated {
				w.WriteHeader(http.StatusAccepted)
			}
			fmt.Fprintln(w, outcome)
		}
	})
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
	pemBytes, err := os.ReadFile(cfg.GitHubAppPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read the GitHub App private key: %w", err)
	}
	key, err := ParsePrivateKey(pemBytes)
	if err != nil {
		return fmt.Errorf("GitHub App private key %s: %w", cfg.GitHubAppPrivateKeyFile, err)
	}
	minter := TokenMinter{AppID: cfg.GitHubAppID, Key: key, APIURL: cfg.GitHubAPIURL}

	queue, err := StartQueue(ctx, QueueOptions{Addr: cfg.NATSAddr, RunnerTokens: runnerTokens, DataDir: cfg.DataDir})
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

	mux := http.NewServeMux()
	mux.Handle("/webhook", Handler(Intake{
		Secret:     []byte(cfg.WebhookSecret),
		ReadyLabel: cfg.ReadyLabel,
		Allow:      allow,
		Publish:    queue.Publish,
	}, log))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen for webhooks on %s: %w", cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("control node started",
		"webhook", "http://"+listener.Addr().String()+"/webhook", "nats", cfg.NATSAddr, "ready_label", cfg.ReadyLabel)

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
