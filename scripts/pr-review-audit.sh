#!/usr/bin/env bash
# pr-review-audit.sh — list open PRs that have no review comment, and branches
# with no PR at all.
#
# WHY THIS EXISTS. AGENTS.md requires every PR to get a complete review posted as
# a comment. In the session that wrote that rule, two PRs were opened without one
# and a third branch never got a PR — because "remember to review it" is not a
# mechanism. This is the mechanism: it is checkable, so it can be run before
# claiming the work is done.
#
#   ./scripts/pr-review-audit.sh          # report; exits non-zero if anything is missing
#
# A "review" is a PR comment whose body starts with "## Review". That convention
# is what makes reviews findable later, which is the whole point of putting them
# on the PR rather than in a chat log.
set -uo pipefail

command -v gh >/dev/null 2>&1 || { echo "gh not found on PATH" >&2; exit 2; }

missing=0

echo "==> open PRs"
prs="$(gh pr list --state open --json number --jq '.[].number' | sort -n)"
if [ -z "$prs" ]; then
    echo "    none"
else
    for p in $prs; do
        title="$(gh pr view "$p" --json title --jq '.title[0:60]')"
        n="$(gh pr view "$p" --json comments \
             --jq '[.comments[] | select(.body | startswith("## Review"))] | length')"
        if [ "$n" -eq 0 ]; then
            printf '    ✗ PR %-5s NO REVIEW   %s\n' "$p" "$title"
            missing=$((missing + 1))
        else
            printf '    ✓ PR %-5s %s review(s)  %s\n' "$p" "$n" "$title"
        fi
    done
fi

echo "==> local branches with no PR"
found_branchless=0
while read -r b; do
    [ -z "$b" ] && continue
    case "$b" in main|gh-pages) continue ;; esac
    # An integration branch legitimately has no PR until the work under it is done.
    case "$b" in integration/*) continue ;; esac
    if ! gh pr list --head "$b" --state all --json number --jq '.[0].number' 2>/dev/null | grep -q .; then
        printf '    ✗ %s\n' "$b"
        missing=$((missing + 1))
        found_branchless=1
    fi
done <<< "$(git branch --format='%(refname:short)')"
[ "$found_branchless" -eq 0 ] && echo "    none"

echo ""
if [ "$missing" -gt 0 ]; then
    echo "✗ $missing item(s) need attention before this work can be called done."
    exit 1
fi
echo "✓ every open PR has a review and every branch has a PR"
