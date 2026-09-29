#!/bin/sh
# Tests requirement L-19 against the release v0.2.0 itself.
#
# Usage: tests/format.test.sh
#
# It builds the tag v0.2.0 in a git worktree and checks:
#   1. testdata/format/write-v0.2.0 run on the tag writes a file that is
#      the same, byte for byte, as testdata/format/v0.2.0.db.
#   2. v0.2.0 checks testdata/format/v0.2.0.db as intact. This is the
#      positive control for 3: the build of the tag works.
#   3. v0.2.0 refuses testdata/format/v2.db with the error for an unknown
#      format version (I-4), and reads no page of it.
#   4. This tree checks both files as intact, with the format version of
#      each.
# The tag must be in the repository; CI fetches it. Each file is copied
# before a program opens it, so the files in the tree stay as they are.
#
# Exit 0: all checks pass. Exit 1: a check failed. Exit 2: the test could
# not run, for example without the tag.

root=$(cd "$(dirname "$0")/.." && pwd)
tag=v0.2.0
if ! git -C "$root" rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
    echo "format.test.sh: the tag $tag is not in the repository" >&2
    exit 2
fi
work=$(mktemp -d)
cleanup() {
    git -C "$root" worktree remove --force "$work/tag" >/dev/null 2>&1
    rm -rf "$work"
}
trap cleanup EXIT
if ! git -C "$root" worktree add -q --detach "$work/tag" "$tag"; then
    echo "format.test.sh: no worktree of $tag" >&2
    exit 2
fi
mkdir -p "$work/tag/cmd/write" "$work/bin" "$work/files"
cp "$root/testdata/format/write-v0.2.0/main.go" "$work/tag/cmd/write/main.go"
if ! (cd "$work/tag" && go build -o "$work/bin/write" ./cmd/write &&
    go build -o "$work/bin/datumujo-$tag" ./cmd/datumujo) ||
    ! (cd "$root" && go build -o "$work/bin/datumujo" ./cmd/datumujo); then
    echo "format.test.sh: a build failed" >&2
    exit 2
fi

failures=0
fail() {
    echo "FAIL: $*"
    failures=$((failures + 1))
}

# run <program> <file> <name>: checks a copy of the file and keeps the
# exit code in $code and the output in $work/<name>.out.
run() {
    cp "$root/testdata/format/$2" "$work/files/$3"
    "$work/bin/$1" check "$work/files/$3" >"$work/$3.out" 2>&1
    code=$?
}

"$work/bin/write" "$work/files/written.db" || fail "the writer of $tag"
cmp -s "$work/files/written.db" "$root/testdata/format/v0.2.0.db" ||
    fail "$tag writes another file than testdata/format/v0.2.0.db"

run "datumujo-$tag" v0.2.0.db old-v1
if [ "$code" -ne 0 ] || ! grep -q '^intact$' "$work/old-v1.out"; then
    fail "$tag on its own file: exit $code"
    cat "$work/old-v1.out"
fi

run "datumujo-$tag" v2.db old-v2
if [ "$code" -ne 2 ] || ! grep -q 'format version 2 is not supported; this code reads version 1' "$work/old-v2.out"; then
    fail "$tag on a file of format version 2: exit $code"
    cat "$work/old-v2.out"
fi

for f in v0.2.0.db:1 v2.db:2; do
    run datumujo "${f%:*}" "new-${f%:*}"
    if [ "$code" -ne 0 ] || ! grep -q '^intact$' "$work/new-${f%:*}.out" ||
        ! grep -q "^format version: ${f#*:}$" "$work/new-${f%:*}.out"; then
        fail "this tree on ${f%:*}: exit $code"
        cat "$work/new-${f%:*}.out"
    fi
done

if [ "$failures" -gt 0 ]; then
    echo "$failures checks failed"
    exit 1
fi
echo "all checks passed"
