package runner

import (
	"bufio"
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
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/repoconfig"
)

// DefaultNATSURL is where a control node on the same machine listens.
const DefaultNATSURL = "nats://127.0.0.1:4222"

func envName(flagName string) string {
	return "SCHMEIDE_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// ParseFlags parses args into fs, then fills every flag that was not given
// from its SCHMEIDE_<NAME> environment variable. A variable whose value does
// not parse for its flag's type is an error that names the variable and shows
// a valid value. It returns flag.ErrHelp when help was asked for.
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
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		name := envName(f.Name)
		if v := os.Getenv(name); v != "" && !given[f.Name] {
			if f.Value.Set(v) != nil {
				errs = append(errs, fmt.Errorf("%s is %q, which is not %s, unset it, or pass --%s", name, v, envExample(f.Value), f.Name))
			}
		}
	})
	return errors.Join(errs...)
}

// envExample describes the kind of value a flag takes and shows a valid one,
// for the error ParseFlags returns.
func envExample(v flag.Value) string {
	if g, ok := v.(flag.Getter); ok {
		switch g.Get().(type) {
		case time.Duration:
			return "a duration; set it to one such as 5m"
		case int:
			return "an integer; set it to one such as 3"
		case bool:
			return "a boolean; set it to true or false"
		}
	}
	return "valid for this flag; set it to a valid value"
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

// harnessFlags are the runner flags that shape each harness session.
type harnessFlags struct {
	claudeArgs, grokArgs string
	allowDangerous       bool
}

// check refuses session flags that widen the permission floor, unless
// --allow-dangerous-harness-args was given.
func (h *harnessFlags) check() error {
	if h.allowDangerous {
		return nil
	}
	for _, f := range []struct{ name, value string }{{"claude-args", h.claudeArgs}, {"grok-args", h.grokArgs}} {
		if arg := harness.WideningArg(strings.Fields(f.value)); arg != "" {
			return fmt.Errorf("--%s contains %q, which lets a session skip its permission checks; remove it, or pass --allow-dangerous-harness-args to accept that (see docs/permissions.md)", f.name, arg)
		}
	}
	return nil
}

func runnerFlags() (fs *flag.FlagSet, o *Options, passEnv *string, h *harnessFlags) {
	o = &Options{}
	fs = flag.NewFlagSet("schmeide runner", flag.ContinueOnError)
	fs.StringVar(&o.Steward, "steward", "", "Your GitHub login. The runner only takes jobs for issues assigned to this login. Required.")
	fs.BoolVar(&o.Pool, "pool", false, "Take a job whose steward has no registered runner. The runner still works one job at a time and takes its own job first. The default is off.")
	fs.StringVar(&o.NATSURL, "nats-url", DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr. Use tls://<host>:<port> when the control node serves NATS over TLS; the system roots verify it. For a private CA on Linux, point SSL_CERT_FILE at a bundle that includes it.")
	fs.StringVar(&o.NATSToken, "nats-token", "", "This steward's NATS token: the one the control node's --runner-tokens lists for --steward. Prefer the environment variable, so the token stays out of the process list.")
	fs.StringVar(&o.WorkDir, "work-dir", "schmeide-work", "Directory for repository clones and per-job git worktrees.")
	fs.StringVar(&o.GitHubAPIURL, "github-api-url", control.DefaultGitHubAPIURL, "Base URL of the GitHub REST API.")
	fs.StringVar(&o.GitHubURL, "github-url", "https://github.com", "Base URL repositories are cloned from and pushed to.")
	fs.StringVar(&o.GitName, "git-name", "schmeide[bot]", "Author name for the commits a job makes: the GitHub App's bot user.")
	fs.StringVar(&o.GitEmail, "git-email", "schmeide[bot]@users.noreply.github.com", "Author email for the commits a job makes.")
	fs.IntVar(&o.MaxAttempts, "max-attempts", defaultMaxAttempts, "How many times a job's work may fail before that failure is final and reported on the issue. Handing the job back because a token could not be minted, or because the runner is shutting down, does not count. A gate failure is never retried.")
	fs.DurationVar(&o.AckWait, "ack-wait", defaultAckWait, "How long JetStream waits without an InProgress heartbeat before redelivering a job. The runner sends a heartbeat every third of this duration. A Go duration, for example 5m.")
	fs.DurationVar(&o.StuckAfter, "stuck-after", defaultStuckAfter, "How long a job may go without a state change before the runner cancels its work and Naks it for another runner. A Go duration, for example 3h.")
	fs.IntVar(&o.MaxReviewRounds, "max-review-rounds", defaultMaxReviewRounds, "How many reviewer sessions to run after the pull request is open. Each round is a fresh reviewer session. When the reviewer requests changes, the repo config sets automate.review_fix = \"on\", and a round remains, a fresh worker session applies the findings, the gate runs again, and the same branch is pushed. With review_fix off, the first request for changes leaves the pull request open for a person. Exhausting the rounds fails the job, comments on the issue, and leaves the pull request open. The factory merges the pull request only when the repo config sets automate.merge = \"on\". The default is a person.")
	fs.DurationVar(&o.SessionTimeout, "session-timeout", repoconfig.DefaultSessionTimeout, "How long one harness session may run before its process group is canceled and the job fails. A Go duration, for example 30m. The repo config session_timeout overrides this when it is set. An empty config value keeps this flag. The default is 2h.")
	fs.DurationVar(&o.GateTimeout, "gate-timeout", repoconfig.DefaultGateTimeout, "How long the gate command may run before its process group is canceled and the job fails. A Go duration, for example 30m. The repo config gate_timeout overrides this when it is set. An empty config value keeps this flag. A gate timeout is not retried. The default is 30m.")
	passEnv = fs.String("pass-env", "", "Comma-separated names to add for the harness session only, on top of the base list and ANTHROPIC_, CLAUDE_, and XAI_. The gate does not use this list. Never list a secret.")
	h = &harnessFlags{}
	fs.StringVar(&h.claudeArgs, "claude-args", "--permission-mode acceptEdits", "Flags for each Claude Code session. The default only lets a session edit files in its worktree. Add to it to let sessions run commands such as the repo's tests. Flags that skip permission checks are refused without --allow-dangerous-harness-args.")
	fs.StringVar(&h.grokArgs, "grok-args", "--permission-mode acceptEdits", "Flags for each Grok session. The default only lets a session edit files in its worktree. Flags that skip permission checks are refused without --allow-dangerous-harness-args.")
	fs.BoolVar(&h.allowDangerous, "allow-dangerous-harness-args", false, "Accept --claude-args and --grok-args that skip permission checks: --dangerously-skip-permissions, or a --permission-mode other than acceptEdits. The default is off.")
	return fs, o, passEnv, h
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	return slices.DeleteFunc(strings.Split(value, ","), func(name string) bool { return name == "" })
}

// harnesses returns harness.All in that order, with claudeArgs on the Claude
// Code harness and grokArgs on the Grok harness. Any other harness keeps the
// defaults All gave it. An empty value stores nil Args, which is the
// harness's own default.
func harnesses(claudeArgs, grokArgs string) []harness.Harness {
	list := harness.All()
	for _, h := range list {
		switch h := h.(type) {
		case *harness.ClaudeCode:
			h.Args = strings.Fields(claudeArgs)
		case *harness.Grok:
			h.Args = strings.Fields(grokArgs)
		}
	}
	return list
}

// Usage is the help text of `schmeide runner`.
func Usage() string {
	fs, _, _, _ := runnerFlags()
	return `schmeide runner takes jobs from the control node and turns each one into a pull
request. It connects out to the control node's NATS, registers under its
steward with the harnesses that are installed and logged in on this machine,
and pulls only that steward's jobs. With --pool, it also takes a job whose
steward has no registered runner. The default is off. It still works one job
at a time and takes its own steward's job first. For each job it creates a
git worktree on a branch named <issue number>_<short-description>, runs a
fresh harness session
with a brief built from the issue as it stood when it became a job, runs the
gate command from the repo's .schmeide/config.toml, and then pushes the branch
and opens a PR as the schmeide GitHub App. Once that gate has passed and the
PR is open, a fresh reviewer session checks the change in the same worktree.
If the reviewer requests changes, the repo config sets
automate.review_fix = "on", and --max-review-rounds (default 3) has rounds
left, a fresh worker session applies the findings, the gate runs again, and
the same branch is pushed so the open PR updates. With review_fix off, the
first request for changes leaves the PR open for a person. A passing review
finishes the job. The factory merges the pull request only when the repo
config sets automate.merge = "on". The default is a person. Factory merge
waits for every check run on the head commit to succeed for check_wait
(default 2m), a top-level key, e.g. check_wait = "10m", reading them with
backoff. A
stacked pull request merges only after the one below it. Exhausting the
rounds fails the job, comments on the issue, and leaves the pull request
open. The outcome is reported to the control node and commented on the issue.
A job the runner does not finish is handed to another runner, and a job that
fails before the pull request is open is tried again up to --max-attempts
times. That count is stored with job status and survives runner restarts.
Handing a job back because a token could not be minted, because the runner is
shutting down, or because --stuck-after (default 3h) found no state change does
not count as one of those tries. A gate failure is never retried.

JetStream redelivers a job when it receives no InProgress heartbeat for
--ack-wait (default 5m). While it works, the runner must stay connected so it
can send a heartbeat every third of that duration. The separate --stuck-after
watchdog prevents those heartbeats from keeping a job with no state change
forever: it cancels the work and Naks the delivery for another runner.

A harness session is canceled after --session-timeout (default 2h). The gate
command is canceled after --gate-timeout (default 30m). Canceling either one
kills its process group, fails the job, and comments on the issue that the
session or the gate timed out. The comment does not include the command's
output. The repo config may override either flag with session_timeout and
gate_timeout, which are Go durations such as 30m. An empty value keeps the
flag. A gate timeout is not retried. A session timeout before the pull
request is open counts toward --max-attempts.

The permission floor is the same for Claude Code and Grok: a session may edit
files in its worktree, and it is not handed flags that skip permission
checks. The runner refuses to start when --claude-args or --grok-args
contains --dangerously-skip-permissions or a --permission-mode other than
acceptEdits, unless --allow-dangerous-harness-args is given. See
docs/permissions.md.

A harness session sees the basic environment plus ANTHROPIC_, CLAUDE_, XAI_,
and --pass-env, and does not see the runner's SCHMEIDE_* settings unless a
name is passed that way. The gate inherits the runner process environment.
Git sees only the installation-token variables. They still run as this
machine's user, so run the runner as a user that holds nothing else worth
taking.

Before the first job in a new pair of work directory and origin URL, the
runner asks on its terminal whether to trust that pair, and records a yes
under the user config directory (schmeide/trust). A different origin asks
again, even for the same directory. Without a terminal, a pair with no
recorded yes fails the job. Set SCHMEIDE_TRUST=1 to trust every pair for
that run without asking or recording anything, for example for an unattended
runner.

Usage:
  schmeide runner --steward <github login> [flags]

Run 'schmeide doctor' first to check that this machine is ready.

Every flag can also be set through its environment variable. A flag wins over
the environment.

` + FlagUsage(fs)
}

// confirm asks a yes-or-no question on out and reads the answer from in.
// Anything but y or yes is a no.
func confirm(in *bufio.Reader, out io.Writer) func(string) (bool, error) {
	return func(question string) (bool, error) {
		fmt.Fprintf(out, "%s [y/N] ", question)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

// Main runs `schmeide runner` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs, o, passEnv, h := runnerFlags()
	err := ParseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	}
	if err == nil && o.Steward == "" {
		err = errors.New("missing --steward: pass your GitHub login with --steward or set SCHMEIDE_STEWARD")
	}
	if err == nil {
		err = h.check()
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide runner: %v\nRun 'schmeide runner --help' for usage.\n", err)
		return 2
	}
	o.PassEnv = splitList(strings.ReplaceAll(*passEnv, " ", ""))
	o.Harnesses = harnesses(h.claudeArgs, h.grokArgs)
	o.Output = stderr
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		o.Confirm = confirm(bufio.NewReader(os.Stdin), stderr)
	}

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
