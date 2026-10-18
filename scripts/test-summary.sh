#!/usr/bin/env bash
# Prints what failed in a `go test` log, for the end of make test's output:
# the FAIL lines, a timeout, and each failed test with the first lines of
# its message, so a gate that keeps only the last lines still says why.
# usage: test-summary.sh LOG
set -euo pipefail
(($# == 1)) || { echo "usage: $0 LOG" >&2; exit 2; }
{
    grep -E '^FAIL[[:space:]]' "$1" || echo "(no FAIL line: see the errors above)"
    grep -E '^panic: test timed out' "$1" || true
    awk '
        /--- FAIL/                  { print; n = 0; msg = 1; next }
        /=== (RUN|PAUSE|CONT|NAME)/ { msg = 0; next }
        msg && /^[[:space:]]/       { if (n++ < 6) print; next }
                                    { msg = 0 }
    ' "$1"
} | head -n 60
