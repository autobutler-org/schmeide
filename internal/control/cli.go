package control

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// setting is one control node setting. Each can come from its flag, its
// environment variable, or the config file, in that order of precedence.
type setting struct {
	name     string
	def      string
	usage    string
	required bool
}

func (s setting) env() string {
	return "SCHMEIDE_" + strings.ToUpper(strings.ReplaceAll(s.name, "-", "_"))
}

var controlSettings = []setting{
	{name: "config", usage: "Path of a config file with one 'name = value' line per setting, named as the flags are."},
	{name: "http-addr", def: ":8080", usage: "Address the plain HTTP webhook endpoint listens on. The listener is HTTP and must sit behind a TLS-terminating proxy. GitHub must be able to reach /webhook there."},
	{name: "webhook-rate", def: "1000", usage: "Maximum webhook requests accepted per minute. Further requests are rejected and publish no job. The default leaves room for GitHub's retries and redeliveries."},
	{name: "delivery-ttl", def: "10m", usage: "How long an X-Github-Delivery id is remembered. A repeat within this time is acknowledged with success and is not published again."},
	{name: "nats-addr", def: "127.0.0.1:4222", usage: "Address the embedded NATS server listens on. Runners connect here. Beyond loopback it needs --nats-tls-cert-file and --nats-tls-key-file, or --allow-plaintext-nats."},
	{name: "nats-tls-cert-file", usage: "Path of the PEM certificate (with its chain) the NATS listener serves. Set it with --nats-tls-key-file, and runners must connect with a tls:// URL. The file is read again on each connection, so a renewed certificate is served without a restart."},
	{name: "nats-tls-key-file", usage: "Path of the PEM private key for --nats-tls-cert-file."},
	{name: "allow-plaintext-nats", def: "false", usage: "Set to true to let NATS listen beyond loopback without TLS. Without it, control refuses to start in that configuration, because steward tokens and every job's GitHub token would cross the network in plain text. Only use it on a network you trust."},
	{name: "runner-tokens", required: true, usage: "Comma-separated '<steward login>:<token>' entries, one per steward. A runner connects to NATS as its steward with that steward's token, and can only reach that steward's jobs and job tokens. Prefer the environment variable or config file, so the tokens stay out of the process list."},
	{name: "data-dir", def: "schmeide-data", usage: "Directory where the job queue is stored."},
	{name: "webhook-secret", required: true, usage: "Webhook secret of the schmeide GitHub App. Prefer the environment variable or config file, so the secret stays out of the process list."},
	{name: "ready-label", def: DefaultReadyLabel, usage: "Label that, together with an assignee, turns an issue into a job."},
	{name: "allowed-orgs", usage: "Comma-separated organization or user logins whose repositories may create jobs. Webhooks for anyone else are rejected with 403. This or --allowed-installations is required."},
	{name: "allowed-installations", usage: "Comma-separated GitHub App installation IDs that may create jobs. A webhook is accepted when it matches this list or --allowed-orgs. This or --allowed-orgs is required."},
	{name: "github-app-id", required: true, usage: "App ID of the schmeide GitHub App."},
	{name: "github-app-private-key-file", required: true, usage: "Path of the GitHub App's private key (.pem). The file must not be readable by group or other: run 'chmod 0600' on it, or control refuses to start."},
	{name: "github-api-url", def: DefaultGitHubAPIURL, usage: "Base URL of the GitHub REST API."},
}

// Usage is the help text of `schmeide control`.
func Usage() string {
	var b strings.Builder
	b.WriteString(`schmeide control runs the control node: an embedded NATS server with JetStream
that holds the job queue, and an HTTP endpoint (/webhook) for the schmeide
GitHub App. The listener is HTTP and must sit behind a TLS-terminating proxy.
An open issue that is assigned and carries the ready label becomes
one job, with the assignee as its steward.

Usage:
  schmeide control [flags]

Every setting can be given as a flag, an environment variable, or a line in the
--config file. A flag wins over the environment, which wins over the file.

Flags:
`)
	for _, s := range controlSettings {
		fmt.Fprintf(&b, "  --%s\n        %s\n        Env: %s", s.name, s.usage, s.env())
		switch {
		case s.required:
			b.WriteString(" (required)")
		case s.def != "":
			fmt.Fprintf(&b, " (default %q)", s.def)
		}
		b.WriteString("\n")
	}
	b.WriteString("  -h, --help\n        Show this help.\n")
	return b.String()
}

// readConfigFile reads 'name = value' lines. Blank lines and lines starting
// with '#' are skipped.
func readConfigFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w; check the path given with --config or SCHMEIDE_CONFIG", err)
	}
	values := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected 'name = value', got %q", path, i+1, line)
		}
		values[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return values, nil
}

// parseControl resolves the control node's settings from flags, environment,
// and config file. It returns flag.ErrHelp when help was asked for.
func parseControl(args []string) (Config, error) {
	fs := flag.NewFlagSet("schmeide control", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, s := range controlSettings {
		fs.String(s.name, s.def, s.usage)
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	values := map[string]string{}
	fromFlag := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { values[f.Name] = f.Value.String() })
	fs.Visit(func(f *flag.Flag) { fromFlag[f.Name] = true })
	fromEnv := func(s setting) bool {
		v := os.Getenv(s.env())
		if !fromFlag[s.name] && v != "" {
			values[s.name] = v
		}
		return fromFlag[s.name] || v != ""
	}

	file := map[string]string{}
	if fromEnv(controlSettings[0]); values["config"] != "" {
		var err error
		if file, err = readConfigFile(values["config"]); err != nil {
			return Config{}, err
		}
	}
	var missing []string
	for _, s := range controlSettings[1:] {
		if v, ok := file[s.name]; !fromEnv(s) && ok {
			values[s.name] = v
		}
		delete(file, s.name)
		if s.required && values[s.name] == "" {
			missing = append(missing, fmt.Sprintf("  %s: pass --%s, set %s, or add '%s = ...' to the --config file",
				s.name, s.name, s.env(), s.name))
		}
	}
	for name := range file {
		return Config{}, fmt.Errorf("config file %s: unknown setting %q; 'schmeide control --help' lists the settings", values["config"], name)
	}
	if len(missing) > 0 {
		return Config{}, errors.New("missing required settings:\n" + strings.Join(missing, "\n"))
	}
	if _, err := ParseRunnerTokens(values["runner-tokens"]); err != nil {
		return Config{}, fmt.Errorf("%w; fix --runner-tokens or SCHMEIDE_RUNNER_TOKENS", err)
	}
	if _, err := ParseAllowlist(values["allowed-orgs"], values["allowed-installations"]); err != nil {
		return Config{}, fmt.Errorf("%w; fix --allowed-orgs or --allowed-installations", err)
	}
	if _, err := parseWebhookRate(values["webhook-rate"]); err != nil {
		return Config{}, err
	}
	if _, err := parseDeliveryTTL(values["delivery-ttl"]); err != nil {
		return Config{}, err
	}
	if _, err := parseAllowPlaintextNATS(values["allow-plaintext-nats"]); err != nil {
		return Config{}, err
	}
	if (values["nats-tls-cert-file"] == "") != (values["nats-tls-key-file"] == "") {
		return Config{}, errors.New("NATS TLS needs both a certificate and a key; set both --nats-tls-cert-file and --nats-tls-key-file (SCHMEIDE_NATS_TLS_CERT_FILE and SCHMEIDE_NATS_TLS_KEY_FILE), or neither")
	}
	return Config{
		HTTPAddr:                values["http-addr"],
		NATSAddr:                values["nats-addr"],
		RunnerTokens:            values["runner-tokens"],
		DataDir:                 values["data-dir"],
		WebhookSecret:           values["webhook-secret"],
		ReadyLabel:              values["ready-label"],
		AllowedOrgs:             values["allowed-orgs"],
		AllowedInstallations:    values["allowed-installations"],
		GitHubAppID:             values["github-app-id"],
		GitHubAppPrivateKeyFile: values["github-app-private-key-file"],
		GitHubAPIURL:            values["github-api-url"],
		WebhookRate:             values["webhook-rate"],
		DeliveryTTL:             values["delivery-ttl"],
		NATSTLSCertFile:         values["nats-tls-cert-file"],
		NATSTLSKeyFile:          values["nats-tls-key-file"],
		AllowPlaintextNATS:      values["allow-plaintext-nats"],
	}, nil
}

// Main runs `schmeide control` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseControl(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide control: %v\nRun 'schmeide control --help' for usage.\n", err)
		return 2
	}
	if err := CheckPrivateKeyFileMode(cfg.GitHubAppPrivateKeyFile); err != nil {
		fmt.Fprintf(stderr, "schmeide control: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg, slog.New(slog.NewTextHandler(stderr, nil))); err != nil {
		fmt.Fprintf(stderr, "schmeide control: %v\n", err)
		return 1
	}
	return 0
}

// CheckPrivateKeyFileMode refuses a GitHub App private key file that group
// or other may read, and says how to fix it.
func CheckPrivateKeyFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("GitHub App private key: %w; fix --github-app-private-key-file", err)
	}
	if mode := info.Mode().Perm(); mode&0o044 != 0 {
		return fmt.Errorf("GitHub App private key %s has mode %04o, readable by group or other; run 'chmod 0600 %s' and start again", path, mode, path)
	}
	return nil
}

// InitConfig is the control config that `schmeide init` writes. Every
// required setting holds a placeholder to replace.
const InitConfig = `# schmeide control node config. This is an operator config, not a repo file:
# replace every "replace-me" with a real secret and do not commit the result.
# Pass it with 'schmeide control --config .schmeide/control.conf'.
http-addr = :8080
# NATS listens on loopback by default. For runners on other machines, listen
# on every interface and serve TLS; runners then use --nats-url tls://<host>:4222.
# Without the TLS pair, control refuses a non-loopback address.
# nats-addr = 0.0.0.0:4222
# nats-tls-cert-file = /path/to/fullchain.pem
# nats-tls-key-file = /path/to/privkey.pem
webhook-secret = replace-me
runner-tokens = replace-me:replace-me
github-app-id = replace-me
github-app-private-key-file = replace-me
allowed-orgs = replace-me
`
