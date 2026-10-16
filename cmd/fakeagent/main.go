// Command fakeagent stands in for an agent CLI such as claude or grok so
// tests can drive the real harness process code. It ignores its arguments
// and stdin and performs the script in the FAKEAGENT_SCRIPT environment
// variable, one step per line:
//
//	edit <path> <text>  write text to path, relative to the working directory
//	stderr <text>       print text to stderr
//	exit <n>            exit with code n
//	sleep <seconds>     sleep
//	hang                sleep until killed
//
// Text is expanded like a shell word: $NAME is the environment variable and
// $$ is the process ID. Blank lines are skipped. An unknown or malformed step
// exits 2 and names the step. The script finishing exits 0.
package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Getenv("FAKEAGENT_SCRIPT"), os.Stderr))
}

// expand replaces $NAME with the environment variable and $$ with the PID.
func expand(s string) string {
	return os.Expand(s, func(name string) string {
		if name == "$" {
			return strconv.Itoa(os.Getpid())
		}
		return os.Getenv(name)
	})
}

// run performs script and returns the exit code.
func run(script string, stderr io.Writer) int {
	for line := range strings.Lines(script) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		step, rest, _ := strings.Cut(line, " ")
		bad := func(err error) int {
			fmt.Fprintf(stderr, "fakeagent: step %q: %v\n", line, err)
			return 2
		}
		switch step {
		case "edit":
			path, text, ok := strings.Cut(rest, " ")
			if !ok || path == "" {
				return bad(errors.New("want edit <path> <text>"))
			}
			if err := os.WriteFile(path, []byte(expand(text)+"\n"), 0o644); err != nil {
				return bad(err)
			}
		case "stderr":
			fmt.Fprintln(stderr, expand(rest))
		case "exit":
			n, err := strconv.Atoi(rest)
			if err != nil {
				return bad(err)
			}
			return n
		case "sleep":
			s, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return bad(err)
			}
			time.Sleep(time.Duration(s * float64(time.Second)))
		case "hang":
			time.Sleep(math.MaxInt64) // select {} would die as a deadlock
		default:
			return bad(fmt.Errorf("unknown step %q; want edit, stderr, exit, sleep, or hang", step))
		}
	}
	return 0
}
