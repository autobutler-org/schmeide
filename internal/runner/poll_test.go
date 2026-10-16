package runner

import "time"

// A short poll keeps the "nothing else arrives" waits, which sleep a few
// polls, from costing seconds per test.
func init() { pollWait = 10 * time.Millisecond }
