package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlUsageDocumentsEverySetting(t *testing.T) {
	usage := Usage()
	for _, s := range controlSettings {
		if !strings.Contains(usage, "--"+s.name+"\n") || !strings.Contains(usage, s.env()) || s.usage == "" {
			t.Errorf("usage does not document --%s and %s", s.name, s.env())
		}
	}
	if !strings.Contains(usage, "HTTP") || !strings.Contains(usage, "TLS-terminating proxy") {
		t.Error("usage does not say the listener is HTTP and must sit behind a TLS-terminating proxy")
	}
}

func TestParseControl(t *testing.T) {
	for _, s := range controlSettings {
		t.Setenv(s.env(), "")
	}
	file := filepath.Join(t.TempDir(), "conf")
	conf := "# control node\nwebhook-secret = from-file\ngithub-app-id = \"111\"\nready-label = file:ready\nallowed-orgs = autobutler-org, exokomodo\nrunner-tokens = octocat:t1\ndata-dir = /file\ndelivery-ttl = 5m\nwebhook-rate = 40\n"
	if err := os.WriteFile(file, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHMEIDE_READY_LABEL", "env:ready")
	t.Setenv("SCHMEIDE_DATA_DIR", "/env")
	t.Setenv("SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE", "/env/key.pem")
	t.Setenv("SCHMEIDE_WEBHOOK_RATE", "80")

	got, err := parseControl([]string{"--config", file, "--data-dir", "/flag", "--delivery-ttl", "15m"})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		HTTPAddr:                ":8080",
		NATSAddr:                "127.0.0.1:4222",
		DataDir:                 "/flag",     // flag beats env and file
		ReadyLabel:              "env:ready", // env beats file
		WebhookSecret:           "from-file", // file beats default
		AllowedOrgs:             "autobutler-org, exokomodo",
		RunnerTokens:            "octocat:t1",
		GitHubAppID:             "111", // quotes are optional
		GitHubAppPrivateKeyFile: "/env/key.pem",
		GitHubAPIURL:            DefaultGitHubAPIURL,
		WebhookRate:             "80",  // env beats the file
		DeliveryTTL:             "15m", // flag beats the file
	}
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}
	t.Setenv("SCHMEIDE_WEBHOOK_RATE", "")
	t.Setenv("SCHMEIDE_DELIVERY_TTL", "")
	bare := filepath.Join(t.TempDir(), "bare")
	bareConf := "webhook-secret = s\ngithub-app-id = 1\nallowed-orgs = acme\nrunner-tokens = octocat:t\ngithub-app-private-key-file = /k.pem\n"
	if err := os.WriteFile(bare, []byte(bareConf), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = parseControl([]string{"--config", bare})
	if err != nil {
		t.Fatal(err)
	}
	if got.WebhookRate != "1000" || got.DeliveryTTL != "10m" {
		t.Errorf("defaults = rate %q ttl %q, want 1000 and 10m", got.WebhookRate, got.DeliveryTTL)
	}

	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"empty allowlist":     {args: []string{"--config", file, "--allowed-orgs", " "}, want: "no allowed orgs or installations"},
		"bad runner token":    {args: []string{"--config", file, "--runner-tokens", "octocat"}, want: "SCHMEIDE_RUNNER_TOKENS"},
		"bad installation id": {args: []string{"--config", file, "--allowed-installations", "61234567,acme"}, want: `allowed installation "acme"`},
		"missing config file": {args: []string{"--config", file + ".nope"}, want: "--config"},
		"positional argument": {args: []string{"--config", file, "extra"}, want: `unexpected argument "extra"`},
		"bad webhook rate":    {args: []string{"--config", file, "--webhook-rate", "0"}, want: "positive"},
		"bad delivery ttl":    {args: []string{"--config", file, "--delivery-ttl", "soon"}, want: "duration"},
	} {
		if _, err := parseControl(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tt.want)
		}
	}
	if _, err := parseControl([]string{"--config", file, "--allowed-orgs", " ", "--allowed-installations", "61234567"}); err != nil {
		t.Errorf("installations alone are an allowlist: %v", err)
	}

	if err := os.WriteFile(file, []byte("webhook-secrte = typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseControl([]string{"--config", file}); err == nil || !strings.Contains(err.Error(), `unknown setting "webhook-secrte"`) {
		t.Errorf("err = %v, want the unknown setting named", err)
	}
}

func TestControlRefusesReadableKeyFile(t *testing.T) {
	for _, s := range controlSettings {
		t.Setenv(s.env(), "")
	}
	key := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(key, []byte("pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	args := []string{"--webhook-secret", "s", "--github-app-id", "1", "--allowed-orgs", "acme", "--runner-tokens", "octocat:t", "--github-app-private-key-file", key}
	if code := Main(args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "chmod 0600 "+key) {
		t.Errorf("code = %d, stderr = %q, want a failure with the chmod hint", code, stderr.String())
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateKeyFileMode(key); err != nil {
		t.Errorf("0600 key: %v", err)
	}
	if !strings.Contains(Usage(), "chmod 0600") {
		t.Error("usage does not document the key file mode")
	}
}
