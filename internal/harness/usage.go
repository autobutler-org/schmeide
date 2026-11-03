package harness

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrUsageLimit means the session stopped because the harness's account ran
// out of usage: a usage, rate, spend, or credit limit.
var ErrUsageLimit = errors.New("ran out of usage")

// UsageLimitError is a session that ended at a usage limit. It wraps
// [ErrUsageLimit]. Tail is the end of the session's output, for a harness
// that continues the work. Error leaves Tail out, so the text never carries
// session output.
type UsageLimitError struct {
	Harness string
	Tail    string
}

func (e *UsageLimitError) Error() string { return e.Harness + " session " + ErrUsageLimit.Error() }

func (e *UsageLimitError) Unwrap() error { return ErrUsageLimit }

// usageTailBytes is the most of a session's output a UsageLimitError keeps.
const usageTailBytes = 4000

// usageLimitMarkers are the lowercase texts that mark a usage limit in the
// output of claude (--print --output-format stream-json) or grok (-p -).
//
// ponytail: matching depends on each CLI's wording, taken from claude 2.1
// and grok 1.0.70. A CLI that rewords its limit message falls through to an
// ordinary failure; add the new text here.
var usageLimitMarkers = []string{
	// claude: "Claude AI usage limit reached|<epoch>", "Usage limit reached",
	// "You've hit your limit · resets ...", and the stream-json
	// "error":"rate_limit" on an assistant event.
	"usage limit reached",
	"you've hit your",
	"out of usage credits",
	"usage credit limit reached",
	"spend limit reached",
	`"error":"rate_limit"`,
	"rate_limit_error",
	// grok: "You've hit the rate limit for your plan.", "Rate limited",
	// "You hit your weekly limit.", "You hit your free usage limit.",
	// "out of credits or over your spending limit".
	"rate limited",
	"you hit your",
	"out of credits",
	"spending limit",
	"usage balance exhausted",
	// Either CLI passing an HTTP 429 through.
	"too many requests",
}

// usageLimited reports whether output shows a usage limit. Run calls it only
// for a session that failed, so a session that wrote about rate limits and
// finished is not mistaken for one.
func usageLimited(output string) bool {
	output = strings.ToLower(output)
	for _, marker := range usageLimitMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// tailWriter keeps the last usageTailBytes written to it.
type tailWriter struct{ buf []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > 2*usageTailBytes {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-usageTailBytes:]...)
	}
	return len(p), nil
}

// String is the kept tail, starting on a whole UTF-8 character.
func (w *tailWriter) String() string {
	tail := w.buf[max(0, len(w.buf)-usageTailBytes):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return string(tail)
}

// failure is the error Run returns for a session that exited with err: a
// UsageLimitError when its output shows a usage limit, and otherwise err
// wrapped as "<cli> session failed".
func failure(name, cli string, err error, out *tailWriter) error {
	if usageLimited(out.String()) {
		return &UsageLimitError{Harness: name, Tail: out.String()}
	}
	return fmt.Errorf("%s session failed: %w", cli, err)
}
