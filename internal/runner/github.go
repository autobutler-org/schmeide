package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/autobutler-org/schmeide/internal/forge"
)

// baseBranch is the branch a new pull request for issue is opened against,
// and the branch its worktree is created from. It is the repository default
// branch when no open blocker has an open schmeide pull request: there is no
// blocker, every blocker is already merged, or a blocker has no open pull
// request whose branch starts with that issue's "<number>_". Otherwise the
// open blockers' pull requests have to be one linear stack, and the new pull
// request stacks on its top: the one whose chain of bases down to the
// default branch holds every other one. Blockers that do not form one chain,
// such as two stacked side by side, are refused with errNotOneStack.
func baseBranch(ctx context.Context, gh forge.Forge, issue int) (string, error) {
	base, err := gh.DefaultBranch(ctx)
	if err != nil {
		return "", err
	}
	blockers, err := gh.BlockedBy(ctx, issue)
	if err != nil {
		return "", err
	}
	var heads []string
	for _, blocker := range blockers {
		if blocker.State != "open" {
			continue
		}
		pr, ok, err := gh.IssuePull(ctx, blocker.Number, "open")
		if err != nil {
			return "", err
		}
		if ok {
			heads = append(heads, pr.Head.Ref)
		}
	}
	if len(heads) == 0 {
		return base, nil
	}
	for _, top := range heads {
		below, err := stackBelow(ctx, gh, top, base)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(heads, func(h string) bool { return h != top && !below[h] }) {
			return top, nil
		}
	}
	return "", fmt.Errorf("the open dependencies' pull requests (%s) are not one linear stack, and %w", strings.Join(heads, ", "), errNotOneStack)
}

// errNotOneStack is open blockers whose pull requests do not form one chain.
// A retry does not change them, so the job fails finally.
var errNotOneStack = errors.New("schmeide stacks only on a single chain of blockers")

// stackBelow is the set of schmeide branches under head: its pull request's
// base, that branch's base, and so on down to trunk.
func stackBelow(ctx context.Context, gh forge.Forge, head, trunk string) (map[string]bool, error) {
	below := map[string]bool{}
	for {
		issue, err := stackedOn(head)
		if err != nil {
			return below, nil
		}
		pr, ok, err := gh.IssuePull(ctx, issue, "open")
		if err != nil {
			return nil, err
		}
		if !ok || pr.Base.Ref == trunk || below[pr.Base.Ref] {
			return below, nil
		}
		head = pr.Base.Ref
		below[head] = true
	}
}

// github is the GitHub forge plus what the runner needs that forge.GitHub
// does not have yet: retargeting a pull request and revoking the job's
// installation token.
type github struct{ forge.GitHub }

// RevokeToken revokes token with DELETE /installation/token, so it stops
// working before its hour is up. GitHub answers 204 No Content.
func (g github) RevokeToken(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const path = "/installation/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(g.API, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("github DELETE %s: %s: %s", path, resp.Status, data)
	}
	return nil
}

// RetargetPull implements [retargeter].
func (g github) RetargetPull(ctx context.Context, number int, base string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	body, err := json.Marshal(map[string]string{"base": base})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d", g.Repo, number)
	auth, err := g.Bearer(ctx)
	if err != nil {
		return fmt.Errorf("github PATCH %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, strings.TrimSuffix(g.API, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github PATCH %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("github PATCH %s: %s: %s", path, resp.Status, data)
	}
	return nil
}

// outcomeMarker tags the issue comment that carries a job's outcome, followed
// by the outcome's state and " -->". A redelivered job reads it back to tell
// that its outcome was already posted.
const outcomeMarker = "<!-- schmeide:outcome "

// markOutcome appends the outcome marker for state to an outcome comment.
func markOutcome(body string, state State) string {
	return body + "\n\n" + outcomeMarker + string(state) + " -->"
}

// postedOutcome returns the state in the last outcome comment a bot posted on
// the issue since the RFC 3339 time since, or false when there is none. Empty
// since reads every comment.
func postedOutcome(ctx context.Context, gh forge.Forge, issue int, since string) (State, bool, error) {
	comments, err := gh.Comments(ctx, issue, since)
	if err != nil {
		return "", false, err
	}
	var state State
	found := false
	for _, c := range comments {
		if c.User.Type != "Bot" {
			continue
		}
		_, rest, ok := strings.Cut(c.Body, outcomeMarker)
		if !ok {
			continue
		}
		if s, _, ok := strings.Cut(rest, " -->"); ok {
			state, found = State(s), true
		}
	}
	return state, found, nil
}

// pullNumber is the pull request number at the end of a GitHub html URL.
func pullNumber(prURL string) (int, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return 0, fmt.Errorf("pull request URL %q: %w", prURL, err)
	}
	n, err := strconv.Atoi(path.Base(u.Path))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("pull request URL %q has no pull number", prURL)
	}
	return n, nil
}

// commentsLead starts the section of issue comments added to a job's issue
// body.
const commentsLead = "Issue comments, oldest first."

// commentsCap bounds the comment text added to a job's issue body, so a long
// thread cannot crowd out the rest of a prompt.
const commentsCap = 32 << 10

// writeAssociations are the author associations of people who can write to
// the repository. Only their comments reach a prompt: the repositories are
// public, and anyone else's comment is untrusted input.
var writeAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// withComments returns body followed by the issue's comments from people who
// can write to the repository, read fresh so a comment added after the ready
// label landed is included. Every prompt that quotes the issue body then
// carries them.
func withComments(ctx context.Context, gh forge.Forge, issue int, body string) (string, error) {
	comments, err := gh.Comments(ctx, issue, "")
	if err != nil {
		return "", fmt.Errorf("read the issue's comments: %w", err)
	}
	return body + commentSection(comments), nil
}

// commentSection renders the comments by people who can write to the
// repository, leaving out every bot's, oldest first. Over commentsCap the
// oldest are left out and the section says how many; a newest comment that is
// over the cap alone is cut to it. No such comment gives an empty section.
func commentSection(comments []forge.Comment) string {
	kept := make([]string, 0, len(comments))
	for _, c := range comments {
		if c.User.Type == "Bot" || !slices.Contains(writeAssociations, c.AuthorAssociation) {
			continue
		}
		kept = append(kept, fmt.Sprintf("%s on %s:\n%s", c.User.Login, c.CreatedAt, strings.TrimSpace(c.Body)))
	}
	if len(kept) == 0 {
		return ""
	}
	if last := len(kept) - 1; len(kept[last]) > commentsCap {
		kept[last] = kept[last][:commentsCap]
	}
	left, size := 0, 0
	for i := len(kept) - 1; i >= 0; i-- {
		size += len(kept[i])
		if size > commentsCap {
			left = i + 1
			break
		}
	}
	var b strings.Builder
	b.WriteString("\n\n" + commentsLead + " Where a comment disagrees with the body above, the later comment wins.\n")
	switch {
	case left == 1:
		b.WriteString("1 older comment was left out to fit.\n")
	case left > 1:
		fmt.Fprintf(&b, "%d older comments were left out to fit.\n", left)
	}
	for _, k := range kept[left:] {
		b.WriteString("\n---\n" + k + "\n")
	}
	return b.String()
}
