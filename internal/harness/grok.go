package harness

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Grok drives the locally installed grok CLI.
type Grok struct {
	// Bin is the grok executable. Empty means "grok" on PATH.
	Bin string
	// Args are the session flags. Nil means "--permission-mode acceptEdits",
	// which lets the session edit files in its working directory and nothing
	// more. A runner that wants the session to run commands sets its own.
	Args []string
}

// Name implements [Harness].
func (*Grok) Name() string { return "grok" }

// lookPath finds the grok executable.
func (g *Grok) lookPath() (string, error) {
	name := g.Bin
	if name == "" {
		name = "grok"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %q was not found; install the Grok CLI (https://x.ai/cli/install.sh) and make sure it is on PATH", ErrNotInstalled, name)
	}
	return bin, nil
}

// grokModelsAuth reads the first line of `grok models`. grok 1.0.52 prints
// that line and exits 0 both when it is logged in ("You are logged in
// with ...", "You are using XAI_API_KEY.", "You are authenticated via ...")
// and when it is not ("You are not authenticated."), so the exit status is
// not the login.
func grokModelsAuth(out string) (loggedIn, recognized bool) {
	var line string
	for _, candidate := range strings.Split(out, "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			line = candidate
			break
		}
	}
	switch {
	case strings.HasPrefix(line, "You are not authenticated"):
		return false, true
	case strings.HasPrefix(line, "You are logged in"),
		strings.HasPrefix(line, "You are using XAI_API_KEY"),
		strings.HasPrefix(line, "You are authenticated via"):
		return true, true
	default:
		return false, false
	}
}

// Check implements [Harness].
func (g *Grok) Check(ctx context.Context) error {
	bin, err := g.lookPath()
	if err != nil {
		return err
	}
	// models prints the login and returns without starting a session.
	// A logged-out CLI still exits 0, so the exit status is ignored in
	// favor of the output.
	out, runErr := exec.CommandContext(ctx, bin, "models").Output()
	loggedIn, recognized := grokModelsAuth(string(out))
	switch {
	case !recognized:
		return fmt.Errorf("could not read the output of `%s models` (%w); run it by hand to see what is wrong, and upgrade the Grok CLI if the command is unknown", bin, errors.Join(runErr, errors.New("unrecognized login status")))
	case !loggedIn:
		return fmt.Errorf("%w: run `%s login` on this machine, then try again", ErrNotLoggedIn, bin)
	default:
		return nil
	}
}

// Run implements [Harness].
func (g *Grok) Run(ctx context.Context, req Request) error {
	bin, err := g.lookPath()
	if err != nil {
		return err
	}
	args := g.Args
	if args == nil {
		args = []string{"--permission-mode", "acceptEdits"}
	}
	// -p - is grok 1.0.52's headless single-turn mode. The "-" reads the
	// prompt from stdin ("pipe the prompt into grok") and the result is
	// written to stdout. The default output format is plain text, so a
	// caller is not required to parse it. Headless mode starts a fresh session
	// unless --resume or --continue is set, and neither is set here.
	// The prompt goes in on stdin so it is never parsed as a flag.
	cmd := exec.CommandContext(ctx, bin, append([]string{"-p", "-"}, args...)...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Stdout = req.Output
	cmd.Stderr = req.Output
	// The session leads its own process session, detached from the runner's
	// terminal, so SIGTTOU and SIGTTIN cannot stop it when the runner runs in
	// a terminal. As session leader it also leads its own process group, so
	// everything it spawns can be killed together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	killGroup := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Cancel = killGroup
	// Stops Wait from hanging when a leftover child holds the output open.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start grok: %w", err)
	}
	err = cmd.Wait()
	_ = killGroup() // sweep up anything the session left running
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("grok session canceled: %w", ctx.Err())
	case errors.Is(err, exec.ErrWaitDelay):
		return nil // grok itself exited 0; only a leftover child lingered
	case err != nil:
		return fmt.Errorf("grok session failed: %w", err)
	}
	return nil
}
