#!/usr/bin/env bash
# Tests for scripts/cover-report.sh.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/cover-report.sh"
d="$(mktemp -d)"; trap 'rm -rf "$d"' EXIT
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

cat >"$d/a.out" <<P
mode: atomic
m/x/a.go:1.1,2.2 8 1
m/x/a.go:3.1,4.2 2 0
P
cat >"$d/b.out" <<P
mode: atomic
m/y/b.go:1.1,2.2 10 0
m/x/a.go:3.1,4.2 2 5
P
# merged: x = 10/10 covered (the 0 block is covered by b), y = 0/10 -> total 50.0
out="$(TOTAL_FILE="$d/t" "$script" "$d/a.out" "$d/b.out")" || fail "no baseline" "exit $?"
[ "$(cat "$d/t")" = "50.0" ] && ok "merged total, duplicate blocks counted once" || fail "total" "$(cat "$d/t")"
grep -q '| m/x | 100.0% | 10 |' <<<"$out" && ok "per-package row" || fail "row" "$out"
grep -q 'No baseline' <<<"$out" && ok "no baseline is not a failure" || fail "no baseline msg" "$out"

run() { BASELINE_FILE="$1" "$script" "$d/a.out" "$d/b.out" >/dev/null 2>&1; }
echo 52 >"$d/base";   run "$d/base" && fail "drop 2" "passed" || ok "fails on a drop over 1 point"
echo 51 >"$d/base";   run "$d/base" && ok "allows a drop of exactly 1" || fail "drop 1" "failed"
echo 40 >"$d/base";   run "$d/base" && ok "passes on an improvement" || fail "improve" "failed"
: >"$d/base";         run "$d/base" && ok "empty baseline skips the gate" || fail "empty" "failed"
echo oops >"$d/base"; run "$d/base" && ok "garbage baseline skips the gate" || fail "garbage" "failed"
run "$d/missing" && ok "missing baseline skips the gate" || fail "missing" "failed"
"$script" >/dev/null 2>&1 && fail "usage" "accepted no args" || ok "requires a profile"

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
