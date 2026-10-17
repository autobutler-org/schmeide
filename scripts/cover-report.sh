#!/usr/bin/env bash
# Merges Go coverage profiles, prints total and per-package coverage as
# markdown, and gates on a drop versus a baseline.
# usage: cover-report.sh PROFILE...
# env:   BASELINE_FILE  file holding the baseline total percent (e.g. 71.3).
#                       Missing, empty or non-numeric: no gate, never a failure.
#        MAX_DROP       allowed drop in points (default 1)
#        TOTAL_FILE     if set, the merged total percent is written there
#        MERGED_FILE    if set, the merged profile is written there
# Profiles must come from `go test -coverprofile`. A block seen in several
# profiles counts once, covered if any profile covered it.
set -euo pipefail
(($# > 0)) || { echo "usage: $0 PROFILE..." >&2; exit 2; }
max_drop="${MAX_DROP:-1}"
merged="$(mktemp)"
trap 'rm -f "$merged"' EXIT

awk '
    FNR == 1 { next }               # "mode: ..." header of each profile
    NF == 3 {
        if (!($1 in stmts)) order[n++] = $1
        stmts[$1] = $2
        if ($3 > cnt[$1]) cnt[$1] = $3
    }
    END {
        print "mode: atomic"
        for (i = 0; i < n; i++) print order[i], stmts[order[i]], cnt[order[i]]
    }' "$@" >"$merged"
[ -z "${MERGED_FILE:-}" ] || cp "$merged" "$MERGED_FILE"

report="$(awk '
    NR == 1 { next }
    {
        file = $1; sub(/:.*/, "", file); sub(/\/[^\/]*$/, "", file)
        tot[file] += $2; all += $2
        if ($3 > 0) { cov[file] += $2; allcov += $2 }
    }
    END {
        for (p in tot) printf "%s\t%.1f\t%d\n", p, 100 * cov[p] / tot[p], tot[p]
        printf "TOTAL\t%.1f\t%d\n", all ? 100 * allcov / all : 0, all
    }' "$merged" | sort)"
total="$(awk -F'\t' '$1 == "TOTAL" { print $2 }' <<<"$report")"
[ -z "${TOTAL_FILE:-}" ] || echo "$total" >"$TOTAL_FILE"

echo "### Coverage: ${total}% of statements"
echo
if [ -n "${BASELINE_FILE:-}" ] && [ -s "$BASELINE_FILE" ]; then
    base="$(tr -d '[:space:]' <"$BASELINE_FILE")"
else
    base=""
fi
gate=""
if [[ "$base" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    delta="$(awk -v a="$total" -v b="$base" 'BEGIN { printf "%+.1f", a - b }')"
    echo "Baseline (main): ${base}%, change: ${delta} points (gate: -${max_drop})"
    if awk -v a="$total" -v b="$base" -v m="$max_drop" 'BEGIN { exit !(b - a > m) }'; then gate=fail; fi
else
    echo "No baseline to compare against; the regression gate is skipped."
fi
echo
echo "| Package | Coverage | Statements |"
echo "|---|---|---|"
grep -v '^TOTAL' <<<"$report" | awk -F'\t' '{ printf "| %s | %s%% | %s |\n", $1, $2, $3 }'
if [ -n "$gate" ]; then
    echo
    echo "**Coverage dropped more than ${max_drop} point(s) versus main.**"
    echo "coverage regression: ${base}% -> ${total}%" >&2
    exit 1
fi
