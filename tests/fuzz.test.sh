#!/bin/sh
# Tests for scripts/fuzz.sh.
#
# Usage: tests/fuzz.test.sh [script]
#
# A fake go on PATH plays each case: the targets it lists, and the output
# and exit code of each fuzz run. CI also runs the suite against a script
# that always exits 0, which must make the suite fail.
#
# Exit 0: all cases pass. Exit 1: at least one case failed.

script=${1:-"$(dirname "$0")/../scripts/fuzz.sh"}
script=$(cd "$(dirname "$script")" && pwd)/$(basename "$script")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
cases=0

race='--- FAIL: FuzzX (10.09s)
    context deadline exceeded
FAIL'
crash='--- FAIL: FuzzX (1.00s)
    fuzz_test.go:10: bad
    context deadline exceeded
Failing input written to testdata/fuzz/FuzzX/abc
FAIL'
other='--- FAIL: FuzzX (1.00s)
    fuzzing process hung or terminated unexpectedly: exit status 2
FAIL'

# expect <exit> <runs> <description> <targets> <result>...
# The fake go lists the targets and gives the results in order, one per
# fuzz run: "ok", "race", "crash" or "other". runs is the number of fuzz
# runs the script must make.
expect() {
    want=$1 runs=$2 desc=$3 targets=$4
    shift 4
    cases=$((cases + 1))
    rm -rf "$work/bin" "$work/state" && mkdir -p "$work/bin" "$work/state"
    i=0
    for r in "$@"; do
        i=$((i + 1))
        case $r in
        ok) printf 'ok  \tpkg\t10.0s\n' >"$work/state/out$i"; echo 0 >"$work/state/rc$i" ;;
        race) printf '%s\n' "$race" >"$work/state/out$i"; echo 1 >"$work/state/rc$i" ;;
        crash) printf '%s\n' "$crash" >"$work/state/out$i"; echo 1 >"$work/state/rc$i" ;;
        other) printf '%s\n' "$other" >"$work/state/out$i"; echo 1 >"$work/state/rc$i" ;;
        esac
    done
    printf '%s\n' "$targets" | tr ' ' '\n' | grep . >"$work/state/targets"
    cat >"$work/bin/go" <<FAKE
#!/bin/sh
s="$work/state"
case "\$*" in
"list ./...") echo example.invalid/pkg ;;
*"-list"*) cat "\$s/targets" ;;
*"-fuzz"*)
    n=\$(( \$(cat "\$s/runs" 2>/dev/null || echo 0) + 1 ))
    echo \$n >"\$s/runs"
    cat "\$s/out\$n" 2>/dev/null
    exit \$(cat "\$s/rc\$n" 2>/dev/null || echo 1)
    ;;
esac
FAKE
    chmod +x "$work/bin/go"
    PATH="$work/bin:$PATH" "$script" 1 >/dev/null 2>&1
    got=$?
    made=$(cat "$work/state/runs" 2>/dev/null || echo 0)
    if [ "$got" -eq "$want" ] && [ "$made" -eq "$runs" ]; then
        echo "ok   $desc"
    else
        echo "FAIL $desc (expected exit $want after $runs runs, got exit $got after $made)"
        failures=$((failures + 1))
    fi
}

expect 0 2 "two targets pass" "FuzzA FuzzB" ok ok
expect 0 3 "the deadline race runs the target again" "FuzzA FuzzB" race ok ok
expect 1 2 "the race twice fails" "FuzzA" race race
expect 1 1 "a crash with a failing input fails at once" "FuzzA FuzzB" crash
expect 1 1 "another failure fails at once" "FuzzA FuzzB" other
expect 1 2 "a failure of the second target fails" "FuzzA FuzzB" ok other
expect 2 0 "no target is not a pass" ""

echo "$cases cases, $failures failed"
[ "$failures" -eq 0 ]
