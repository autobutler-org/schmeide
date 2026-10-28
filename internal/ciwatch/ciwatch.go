// Package ciwatch decides when a failed check on an open pull request needs
// its log for a fix round, and caps that log. It makes no network calls.
package ciwatch

import "unicode/utf8"

// TailBytes is the most of a check's log a fix brief carries.
const TailBytes = 4000

// Run is one check run on a commit. Status is completed once GitHub has a
// conclusion. ID is also the Actions job ID for a check that Actions ran.
type Run struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// NeedsTail returns the first completed run that failed, when no fix round
// has run yet. A conclusion of skipped is not a failure: GitHub reports a
// path-filtered job that way. A pull request gets one fix round, so after it
// (fixed) a failure needs no log.
func NeedsTail(runs []Run, fixed bool) (Run, bool) {
	if fixed {
		return Run{}, false
	}
	for _, run := range runs {
		if run.Status == "completed" && run.Conclusion != "success" && run.Conclusion != "skipped" {
			return run, true
		}
	}
	return Run{}, false
}

// Tail is the last TailBytes of log, starting on a whole UTF-8 character.
func Tail(log []byte) string {
	if len(log) <= TailBytes {
		return string(log)
	}
	tail := log[len(log)-TailBytes:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return string(tail)
}
