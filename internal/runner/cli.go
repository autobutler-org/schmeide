package runner

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

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

// DefaultNATSURL is where a control node on the same machine listens.
const DefaultNATSURL = "nats://127.0.0.1:4222"

func envName(flagName string) string {
	return "SCHMEIDE_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// ParseFlags parses args into fs, then fills every flag that was not given
// from its SCHMEIDE_<NAME> environment variable. It returns flag.ErrHelp when
// help was asked for.
func ParseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	fs.VisitAll(func(f *flag.Flag) {
		if v := os.Getenv(envName(f.Name)); v != "" && !given[f.Name] {
			_ = f.Value.Set(v)
		}
	})
	return nil
}

// FlagUsage lists the flags of fs with their environment variables and
// defaults, in the layout every schmeide command uses.
func FlagUsage(fs *flag.FlagSet) string {
	var b strings.Builder
	b.WriteString("Flags:\n")
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(&b, "  --%s\n        %s\n        Env: %s", f.Name, f.Usage, envName(f.Name))
		if f.DefValue != "" {
			fmt.Fprintf(&b, " (default %q)", f.DefValue)
		}
		b.WriteString("\n")
	})
	b.WriteString("  -h, --help\n        Show this help.\n")
	return b.String()
}

func runnerFlags() (*flag.FlagSet, *Options, *string) {
	o := &Options{}
	fs := flag.NewFlagSet("schmeide runner", flag.ContinueOnError)
	fs.StringVar(&o.Steward, "steward", "", "Your GitHub login. The runner only takes jobs for issues assigned to this login. Required.")
	fs.StringVar(&o.NATSURL, "nats-url", DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr.")
	fs.StringVar(&o.NATSToken, "nats-token", "", "Token the control node's NATS expects, which is its --nats-token. Prefer the environment variable, so the token stays out of the process list.")
	fs.StringVar(&o.WorkDir, "work-dir", "schmeide-work", "Directory for repository clones and per-job git worktrees.")
	fs.StringVar(&o.GitHubAPIURL, "github-api-url", control.DefaultGitHubAPIURL, "Base URL of the GitHub REST API.")
	fs.StringVar(&o.GitHubURL, "github-url", "https://github.com", "Base URL repositories are cloned from and pushed to.")
	fs.StringVar(&o.GitName, "git-name", "schmeide[bot]", "Author name for the commits a job makes: the GitHub App's bot user.")
	fs.StringVar(&o.GitEmail, "git-email", "schmeide[bot]@users.noreply.github.com", "Author email for the commits a job makes.")
	claudeArgs := fs.String("claude-args", "--permission-mode acceptEdits", "Flags for each Claude Code session. The default only lets a session edit files in its worktree. Add to it to let sessions run commands such as the repo's tests.")
	return fs, o, claudeArgs
}

// Usage is the help text of `schmeide runner`.
func Usage() string {
	fs, _, _ := runnerFlags()
	return `schmeide runner takes jobs from the control node and turns each one into a pull
request. It connects out to the control node's NATS, registers under its
steward with the harnesses that are installed and logged in on this machine,
and pulls only that steward's jobs. For each job it creates a git worktree on a
branch named <issue number>_<short-description>, runs a fresh harness session
with a brief built from the issue, runs the gate command from the repo's
.schmeide/config.toml, and then pushes the branch and opens a PR as the schmeide
GitHub App. The outcome is reported to the control node and commented on the
issue. A job the runner does not finish is handed to another runner.

Usage:
  schmeide runner --steward <github login> [flags]

Run 'schmeide doctor' first to check that this machine is ready.

Every flag can also be set through its environment variable. A flag wins over
the environment.

` + FlagUsage(fs)
}

// Main runs `schmeide runner` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs, o, claudeArgs := runnerFlags()
	err := ParseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	}
	if err == nil && o.Steward == "" {
		err = errors.New("missing --steward: pass your GitHub login with --steward or set SCHMEIDE_STEWARD")
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide runner: %v\nRun 'schmeide runner --help' for usage.\n", err)
		return 2
	}
	o.Harnesses = []harness.Harness{&harness.ClaudeCode{Args: strings.Fields(*claudeArgs)}}
	o.Output = stderr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := New(ctx, *o, slog.New(slog.NewTextHandler(stderr, nil)))
	if err == nil {
		defer r.Close()
		err = r.Run(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide runner: %v\n", err)
		return 1
	}
	return 0
}
