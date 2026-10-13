package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlUsageDocumentsEverySetting(t *testing.T) {
	for _, s := range controlSettings {
		if got := Usage(); !strings.Contains(got, "--"+s.name+"\n") || !strings.Contains(got, s.env()) || s.usage == "" {
			t.Errorf("usage does not document --%s and %s", s.name, s.env())
		}
	}
}

func TestParseControl(t *testing.T) {
	for _, s := range controlSettings {
		t.Setenv(s.env(), "")
	}
	file := filepath.Join(t.TempDir(), "conf")
	conf := "# control node\nwebhook-secret = from-file\ngithub-app-id = \"111\"\nready-label = file:ready\ndata-dir = /file\n"
	if err := os.WriteFile(file, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHMEIDE_READY_LABEL", "env:ready")
	t.Setenv("SCHMEIDE_DATA_DIR", "/env")
	t.Setenv("SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE", "/env/key.pem")

	got, err := parseControl([]string{"--config", file, "--data-dir", "/flag"})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		HTTPAddr:                ":8080",
		NATSAddr:                "127.0.0.1:4222",
		DataDir:                 "/flag",     // flag beats env and file
		ReadyLabel:              "env:ready", // env beats file
		WebhookSecret:           "from-file", // file beats default
		GitHubAppID:             "111",       // quotes are optional
		GitHubAppPrivateKeyFile: "/env/key.pem",
		GitHubAPIURL:            DefaultGitHubAPIURL,
	}
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}

	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"exposed nats needs a token": {args: []string{"--config", file, "--nats-addr", "0.0.0.0:4222"}, want: "SCHMEIDE_NATS_TOKEN"},
		"missing config file":        {args: []string{"--config", file + ".nope"}, want: "--config"},
		"positional argument":        {args: []string{"--config", file, "extra"}, want: `unexpected argument "extra"`},
	} {
		if _, err := parseControl(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tt.want)
		}
	}
	if _, err := parseControl([]string{"--config", file, "--nats-addr", "0.0.0.0:4222", "--nats-token", "t"}); err != nil {
		t.Errorf("exposed nats with a token: %v", err)
	}

	if err := os.WriteFile(file, []byte("webhook-secrte = typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseControl([]string{"--config", file}); err == nil || !strings.Contains(err.Error(), `unknown setting "webhook-secrte"`) {
		t.Errorf("err = %v, want the unknown setting named", err)
	}
}
