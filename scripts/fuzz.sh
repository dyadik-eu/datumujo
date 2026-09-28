#!/bin/sh
# Runs every fuzz target of the module for a time each (requirement P-2).
#
# Usage: scripts/fuzz.sh [seconds]
#
# A run of `go test -fuzz` can fail with only "context deadline exceeded"
# when its time ends. That is a race in the fuzzing of Go, not a finding.
# The file internal/fuzz/fuzz.go makes a context with the deadline and a
# child fuzzCtx. It treats the end as normal only if the error equals
# fuzzCtx.Err(). The package context closes the Done channel of the
# parent before it cancels the child. So the check can see a nil error
# in the child.
#
# Such a failure writes no failing input. This script runs that target
# once more. Any other failure, and a second failure, fails the script.
#
# Exit 0: all targets ran. Exit 1: a target failed. Exit 2: no target
# found, so nothing was checked.

set -u
time="${1:-10}s"
count=0
for pkg in $(go list ./...); do
    for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
        count=$((count + 1))
        for try in 1 2; do
            out=$(go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "$time" 2>&1)
            status=$?
            printf '%s\n' "$out"
            [ "$status" -eq 0 ] && break
            if [ "$try" -eq 1 ] &&
                printf '%s\n' "$out" | grep -q '^    context deadline exceeded$' &&
                ! printf '%s\n' "$out" | grep -q 'Failing input written'; then
                echo "fuzz.sh: $pkg $target ended with the deadline race of go test -fuzz; running it again" >&2
                continue
            fi
            echo "fuzz.sh: $pkg $target failed" >&2
            exit 1
        done
    done
done
echo "fuzz targets run: $count"
[ "$count" -gt 0 ] || exit 2
