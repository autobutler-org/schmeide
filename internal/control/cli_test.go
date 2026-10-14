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
	conf := "# control node\nwebhook-secret = from-file\ngithub-app-id = \"111\"\nready-label = file:ready\nallowed-orgs = autobutler-org, exokomodo\nrunner-tokens = octocat:t1\ndata-dir = /file\n"
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
		AllowedOrgs:             "autobutler-org, exokomodo",
		RunnerTokens:            "octocat:t1",
		GitHubAppID:             "111", // quotes are optional
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
		"empty allowlist":     {args: []string{"--config", file, "--allowed-orgs", " "}, want: "no allowed orgs or installations"},
		"bad runner token":    {args: []string{"--config", file, "--runner-tokens", "octocat"}, want: "SCHMEIDE_RUNNER_TOKENS"},
		"bad installation id": {args: []string{"--config", file, "--allowed-installations", "61234567,acme"}, want: `allowed installation "acme"`},
		"missing config file": {args: []string{"--config", file + ".nope"}, want: "--config"},
		"positional argument": {args: []string{"--config", file, "extra"}, want: `unexpected argument "extra"`},
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
