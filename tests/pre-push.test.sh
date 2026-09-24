#!/bin/sh
# Tests for .githooks/pre-push.
#
# Usage: tests/pre-push.test.sh [hook]
#
# The optional argument replaces the hook under test. CI uses it to run
# the suite against a hook that allows everything, which must make the
# suite fail. A test suite that cannot fail proves nothing.
#
# Exit 0: all cases pass. Exit 1: at least one case failed.

hook=${1:-"$(dirname "$0")/../.githooks/pre-push"}
failures=0
cases=0

zeros40=0000000000000000000000000000000000000000
zeros64=0000000000000000000000000000000000000000000000000000000000000000
sha=1111111111111111111111111111111111111111

# expect <exit code> <description> <stdin line>
expect() {
    cases=$((cases + 1))
    printf '%s\n' "$3" | "$hook" origin git@example.invalid:x.git >/dev/null 2>&1
    got=$?
    if [ "$got" -eq "$1" ]; then
        echo "ok   $2"
    else
        echo "FAIL $2 (expected exit $1, got $got)"
        failures=$((failures + 1))
    fi
}

# Git ignores a hook without the executable bit, silently. Checked first,
# because every case below would pass by accident if git never runs it.
cases=$((cases + 1))
if [ -x "$hook" ]; then
    echo "ok   hook is executable"
else
    echo "FAIL hook is not executable, git would skip it without a message"
    failures=$((failures + 1))
fi

expect 1 "push to existing main is rejected" \
    "refs/heads/x $sha refs/heads/main $sha"
expect 0 "first push creating main is allowed (SHA-1)" \
    "refs/heads/main $sha refs/heads/main $zeros40"
expect 0 "first push creating main is allowed (SHA-256)" \
    "refs/heads/main $sha refs/heads/main $zeros64"
expect 0 "push to a feature branch is allowed" \
    "refs/heads/docs/x $sha refs/heads/docs/x $sha"
expect 0 "branch named like main but not main is allowed" \
    "refs/heads/x $sha refs/heads/main-old $sha"
expect 0 "nothing to push is allowed" ""

echo
echo "$cases cases, $failures failed"
[ "$failures" -eq 0 ]
