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
	"slices"
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

func runnerFlags() (fs *flag.FlagSet, o *Options, passEnv, claudeArgs *string) {
	o = &Options{}
	fs = flag.NewFlagSet("schmeide runner", flag.ContinueOnError)
	fs.StringVar(&o.Steward, "steward", "", "Your GitHub login. The runner only takes jobs for issues assigned to this login. Required.")
	fs.StringVar(&o.NATSURL, "nats-url", DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr.")
	fs.StringVar(&o.NATSToken, "nats-token", "", "This steward's NATS token: the one the control node's --runner-tokens lists for --steward. Prefer the environment variable, so the token stays out of the process list.")
	fs.StringVar(&o.WorkDir, "work-dir", "schmeide-work", "Directory for repository clones and per-job git worktrees.")
	fs.StringVar(&o.GitHubAPIURL, "github-api-url", control.DefaultGitHubAPIURL, "Base URL of the GitHub REST API.")
	fs.StringVar(&o.GitHubURL, "github-url", "https://github.com", "Base URL repositories are cloned from and pushed to.")
	fs.StringVar(&o.GitName, "git-name", "schmeide[bot]", "Author name for the commits a job makes: the GitHub App's bot user.")
	fs.StringVar(&o.GitEmail, "git-email", "schmeide[bot]@users.noreply.github.com", "Author email for the commits a job makes.")
	fs.IntVar(&o.MaxAttempts, "max-attempts", defaultMaxAttempts, "How many times a job's work may fail before that failure is final and reported on the issue. Handing the job back because a token could not be minted, or because the runner is shutting down, does not count. A gate failure is never retried.")
	fs.IntVar(&o.MaxReviewRounds, "max-review-rounds", defaultMaxReviewRounds, "How many reviewer sessions to run after the pull request is open. Each round is a fresh reviewer session. When the reviewer requests changes and a round remains, a fresh worker session applies the findings, the gate runs again, and the same branch is pushed. Exhausting the rounds fails the job, comments on the issue, and leaves the pull request open. The factory merges the pull request only when the repo config sets merge = \"factory\". The default is a person.")
	passEnv = fs.String("pass-env", "", "Comma-separated names of environment variables that harness sessions and gate commands may see, on top of PATH, HOME, locale, and proxy settings. Every other variable, this runner's own settings included, is withheld from them. Never list a secret.")
	claudeArgs = fs.String("claude-args", "--permission-mode acceptEdits", "Flags for each Claude Code session. The default only lets a session edit files in its worktree. Add to it to let sessions run commands such as the repo's tests.")
	return fs, o, passEnv, claudeArgs
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	return slices.DeleteFunc(strings.Split(value, ","), func(name string) bool { return name == "" })
}

// harnesses returns harness.All in that order. claudeArgs applies only to the
// Claude Code harness; every other harness keeps the defaults All gave it.
// An empty claudeArgs means no extra Claude flags, the same as --claude-args
// set to empty: Fields returns nil, which is what the runner stores today.
func harnesses(claudeArgs string) []harness.Harness {
	list := harness.All()
	args := strings.Fields(claudeArgs)
	for _, h := range list {
		cc, ok := h.(*harness.ClaudeCode)
		if !ok {
			continue
		}
		cc.Args = args
	}
	return list
}

// Usage is the help text of `schmeide runner`.
func Usage() string {
	fs, _, _, _ := runnerFlags()
	return `schmeide runner takes jobs from the control node and turns each one into a pull
request. It connects out to the control node's NATS, registers under its
steward with the harnesses that are installed and logged in on this machine,
and pulls only that steward's jobs. For each job it creates a git worktree on a
branch named <issue number>_<short-description>, runs a fresh harness session
with a brief built from the issue as it stood when it became a job, runs the
gate command from the repo's .schmeide/config.toml, and then pushes the branch
and opens a PR as the schmeide GitHub App. Once that gate has passed and the
PR is open, a fresh reviewer session checks the change in the same worktree.
If the reviewer requests changes and --max-review-rounds (default 3) has
rounds left, a fresh worker session applies the findings, the gate runs
again, and the same branch is pushed so the open PR updates. A passing review
finishes the job. The factory merges the pull request only when the repo
config sets merge = "factory". The default is a person. Exhausting the
rounds fails the job, comments on the issue, and leaves the pull request
open. The outcome is reported to the control node and commented on the issue.
A job the runner does not finish is handed to another runner, and a job that
fails before the pull request is open is tried again up to --max-attempts
times. Handing a job back because a token could not be minted, or because the
runner is shutting down, does not count as one of those tries. A gate failure
is never retried.

Harness sessions and gate commands see only a basic environment (see
--pass-env), never the runner's own settings such as its NATS token. They still
run as this machine's user, so run the runner as a user that holds nothing else
worth taking.

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
	fs, o, passEnv, claudeArgs := runnerFlags()
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
	o.PassEnv = splitList(strings.ReplaceAll(*passEnv, " ", ""))
	o.Harnesses = harnesses(*claudeArgs)
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
