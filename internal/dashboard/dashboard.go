// Package dashboard implements `schmeide dashboard`: a local web page that
// shows one steward's runners and jobs, refreshed by polling the control
// node's status query, and an ask panel that hands questions about them to
// the claude CLI.
//
// The dashboard connects to the control node's NATS as the steward, the same
// way `schmeide status` does, and sees exactly what that command sees, plus
// the jobs that finished in the last day. It keeps an activity log in memory,
// built from the states it has seen, which is lost when it stops. It changes
// nothing: it publishes no job, status, or registration, and the claude
// session it runs for a question has no tools, no MCP servers, and no saved
// session. It does not read runner stages or GitHub.
package dashboard

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/nats-io/nats.go"
)

// Config is what `schmeide dashboard` runs with.
type Config struct {
	Steward   string
	NATSURL   string
	NATSToken string
	// Addr is the address the page is served on.
	Addr string
	// Interval is how often the status query is asked.
	Interval time.Duration
	// Model is the claude --model the ask panel uses.
	Model string
	// Claude is the claude executable.
	Claude string
}

// Usage is the help text of `schmeide dashboard`.
func Usage() string {
	return `schmeide dashboard serves a web page that shows your runners and jobs as the
control node reports them, refreshed every --interval, with the jobs that
finished in the last 24 hours and a log of the states it has seen. Its "Ask the
factory" panel answers questions about what the page shows with the claude CLI,
which runs with no tools: it cannot run commands, read files, or change
anything. The dashboard connects to the control node's NATS as a steward, the
same way 'schmeide status' does, and changes nothing.

Usage:
  schmeide dashboard --steward <github login> [flags]

Flags:
  --steward
        Your GitHub login. Required.
        Env: SCHMEIDE_STEWARD
  --nats-url
        NATS URL of the control node, which is its --nats-addr.
        Env: SCHMEIDE_NATS_URL (default "nats://127.0.0.1:4222")
  --nats-token
        This steward's NATS token: the one the control node's --runner-tokens lists for --steward.
        Env: SCHMEIDE_NATS_TOKEN
  --addr
        Address to serve the page on. Anything other than a loopback address
        exposes your jobs and the ask panel, with no login, to whoever can
        reach it. (default "127.0.0.1:7878")
  --interval
        How often to ask the control node for status. (default 5s)
  --model
        The claude --model the ask panel uses. (default "sonnet")
  --claude
        The claude executable the ask panel runs. (default "claude")
  -h, --help
        Show this help.
`
}

// Main runs `schmeide dashboard` and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schmeide dashboard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	var cfg Config
	fs.StringVar(&cfg.Steward, "steward", env("SCHMEIDE_STEWARD", ""), "")
	fs.StringVar(&cfg.NATSURL, "nats-url", env("SCHMEIDE_NATS_URL", "nats://127.0.0.1:4222"), "")
	fs.StringVar(&cfg.NATSToken, "nats-token", env("SCHMEIDE_NATS_TOKEN", ""), "")
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1:7878", "")
	fs.DurationVar(&cfg.Interval, "interval", 5*time.Second, "")
	fs.StringVar(&cfg.Model, "model", "sonnet", "")
	fs.StringVar(&cfg.Claude, "claude", "claude", "")
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	}
	switch {
	case err != nil:
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case cfg.Steward == "":
		err = errors.New("missing --steward: pass your GitHub login with --steward or set SCHMEIDE_STEWARD")
	case cfg.Interval <= 0:
		err = fmt.Errorf("--interval %s is not positive; pass a duration such as 5s", cfg.Interval)
	case cfg.Model == "":
		err = errors.New("--model is empty; pass a claude model such as sonnet")
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide dashboard: %v\nRun 'schmeide dashboard --help' for usage.\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg, slog.New(slog.NewTextHandler(stderr, nil))); err != nil {
		fmt.Fprintf(stderr, "schmeide dashboard: %v\n", err)
		return 1
	}
	return 0
}

// Run serves the dashboard until ctx ends.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	nc, err := nats.Connect(cfg.NATSURL,
		nats.UserInfo(cfg.Steward, cfg.NATSToken),
		nats.CustomInboxPrefix(control.InboxPrefix(cfg.Steward)),
		nats.Name("schmeide dashboard"),
		nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("cannot reach the control node's NATS at %s as %s: %w\n"+
			"Check that `schmeide control` is running, that --nats-url points at its --nats-addr, "+
			"and that --nats-token is the token its --runner-tokens lists for %s", cfg.NATSURL, cfg.Steward, err, cfg.Steward)
	}
	defer nc.Close()

	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w; pass a free address with --addr", cfg.Addr, err)
	}
	host, _, _ := net.SplitHostPort(cfg.Addr)
	loopback := host == "localhost" || isLoopback(host)
	if !loopback {
		log.Warn("the dashboard is reachable beyond this machine and has no login; anyone who can reach it sees your jobs and can ask questions on your claude account", "addr", cfg.Addr)
	}

	b := newBoard(cfg.Steward, cfg.Model)
	poll := func() {
		snap, err := control.QueryStatus(nc, cfg.Steward, control.StatusQuery{Finished: true})
		if err != nil {
			log.Warn("status query failed; the page shows the last good state", "err", err)
		}
		b.update(snap, err, time.Now())
	}
	poll()
	go func() {
		tick := time.NewTicker(cfg.Interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				poll()
			}
		}
	}()

	claude := Claude{Bin: cfg.Claude, Model: cfg.Model}
	a := newAsker(b, claude.Ask)
	srv := &http.Server{
		Handler:           Handler(b.View, a.Ask, loopback),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("dashboard serving", "url", "http://"+l.Addr().String(), "steward", cfg.Steward, "model", cfg.Model)
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve the dashboard: %w", err)
	}
	return nil
}
