#!/usr/bin/env bash
# Tests for scripts/test-summary.sh.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/test-summary.sh"
d="$(mktemp -d)"; trap 'rm -rf "$d"' EXIT
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

cat >"$d/log" <<'L'
=== RUN   TestFine
--- PASS: TestFine (0.01s)
--- FAIL: TestRestackDoesNotSkipRerereResolvedPick (2.16s)
    stack_test.go:290: restack merged [t1], repairing []; want t1 sent to a repair
--- FAIL: TestParent (0.10s)
    --- FAIL: TestParent/child (0.05s)
        parent_test.go:12: child broke
            with a second line
ok  	example.com/m/a	1.0s
FAIL	example.com/m/app	139.068s
L
out="$("$script" "$d/log")"
grep -q 'FAIL	example.com/m/app' <<<"$out" && ok "package FAIL line" || fail "package" "$out"
grep -q -- '--- FAIL: TestRestackDoesNotSkipRerereResolvedPick' <<<"$out" && ok "failed test" || fail "test" "$out"
grep -q 'stack_test.go:290: restack merged \[t1\]' <<<"$out" && ok "keeps the failure message" || fail "message" "$out"
grep -q 'parent_test.go:12: child broke' <<<"$out" && grep -q 'with a second line' <<<"$out" &&
    ok "keeps a subtest's message" || fail "subtest" "$out"
grep -q 'TestFine\|ok  	example' <<<"$out" && fail "passing" "$out" || ok "leaves passing tests out"

printf 'some build error\n' >"$d/build"
grep -q 'no FAIL line' <<<"$("$script" "$d/build")" && ok "no FAIL line says so" || fail "build" "$("$script" "$d/build")"

{ for i in $(seq 200); do printf -- '--- FAIL: TestN%d (0.01s)\n    n_test.go:1: x\n' "$i"; done; printf 'FAIL\tm\t1s\n'; } >"$d/many"
n="$("$script" "$d/many" | wc -l)"
[ "$n" -le 60 ] && ok "caps the summary" || fail "cap" "$n lines"

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
