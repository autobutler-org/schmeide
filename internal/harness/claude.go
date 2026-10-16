package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ClaudeCode drives the locally installed claude CLI.
type ClaudeCode struct {
	// Bin is the claude executable. Empty means "claude" on PATH.
	Bin string
	// Args are the session flags. Nil means "--permission-mode acceptEdits",
	// which lets the session edit files in its working directory and nothing
	// more. A runner that wants the session to run commands sets its own.
	Args []string
}

// WideningArg returns the first flag in args that would widen a session past
// the permission floor, or "" when there is none. The floor is the same for
// Claude Code and Grok: a session may edit its worktree, and it is not handed
// --dangerously-skip-permissions or a --permission-mode other than
// acceptEdits. See docs/permissions.md.
func WideningArg(args []string) string {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions":
			return arg
		case "--permission-mode":
			if !hasValue && i+1 < len(args) {
				value = args[i+1]
				arg += " " + value
			}
			if value != "acceptEdits" {
				return arg
			}
		}
	}
	return ""
}

// Name implements [Harness].
func (*ClaudeCode) Name() string { return "claude-code" }

// lookPath finds the claude executable.
func (c *ClaudeCode) lookPath() (string, error) {
	name := c.Bin
	if name == "" {
		name = "claude"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %q was not found; install Claude Code (https://claude.com/claude-code) and make sure it is on PATH", ErrNotInstalled, name)
	}
	return bin, nil
}

// Check implements [Harness].
func (c *ClaudeCode) Check(ctx context.Context) error {
	bin, err := c.lookPath()
	if err != nil {
		return err
	}
	// A logged-out CLI exits non-zero but still prints the JSON, so the exit
	// status is ignored in favor of the output.
	out, runErr := exec.CommandContext(ctx, bin, "auth", "status", "--json").Output()
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return fmt.Errorf("could not read the output of `%s auth status --json` (%w); run it by hand to see what is wrong, and upgrade Claude Code if the command is unknown", bin, errors.Join(runErr, err))
	}
	if !status.LoggedIn {
		return fmt.Errorf("%w: run `%s auth login` on this machine, then try again", ErrNotLoggedIn, bin)
	}
	return nil
}

// Run implements [Harness].
func (c *ClaudeCode) Run(ctx context.Context, req Request) error {
	bin, err := c.lookPath()
	if err != nil {
		return err
	}
	args := c.Args
	if args == nil {
		args = []string{"--permission-mode", "acceptEdits"}
	}
	// The prompt goes in on stdin so it is never parsed as a flag.
	cmd := exec.CommandContext(ctx, bin, append([]string{"--print"}, args...)...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Stdout = req.Output
	cmd.Stderr = req.Output
	// The session gets its own process group so everything it spawns can be
	// killed together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killGroup := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Cancel = killGroup
	// Stops Wait from hanging when a leftover child holds the output open.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	err = cmd.Wait()
	_ = killGroup() // sweep up anything the session left running
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("claude session canceled: %w", ctx.Err())
	case errors.Is(err, exec.ErrWaitDelay):
		return nil // claude itself exited 0; only a leftover child lingered
	case err != nil:
		return fmt.Errorf("claude session failed: %w", err)
	}
	return nil
}
