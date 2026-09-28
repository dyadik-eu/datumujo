# Contributing

The test for every change: could someone who was not there follow it,
rebuild it and prove it wrong, using only the pull request? If not, it is
not done.

## Setup

You need Go at the version in `go.mod`. Nothing else: datumujo uses the Go
standard library only (requirement S-2).

Once per clone:

```sh
git config core.hooksPath .githooks
```

This enables `.githooks/pre-push`, which rejects a push to `main`. Without
this line the hook exists and never runs.

## Checks

CI runs `.github/workflows/checks.yml` on every pull request. The workflow
is the list of checks, not this file. Before a push, run at least:

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

`gofmt -l .` must print nothing.

## Where work comes from

Work follows [docs/roadmap.md](docs/roadmap.md). The requirements, each
with an ID, are in [docs/requirements.md](docs/requirements.md), and the
design is in [docs/design.md](docs/design.md). One roadmap step is about
one pull request.

A bug report does not need a roadmap step. A new feature does: open an
issue first, so the step can be added before the work starts.

## A change

1. Reproduce the problem with a test or a command. Without a
   reproduction, nothing changes.
2. Write the test first. Run it before the fix and after it: red, then
   green.
3. Change only what the problem needs. A cleanup on the side is a pull
   request of its own.
4. Change `docs/design.md` in the same pull request as the code it
   describes.
5. A change of the file format needs a way to read the files of the
   release before (I-3).

Mutation tests are welcome proof: change the new code on purpose and show
that a test fails. A mutation that does not build proves nothing, so build
it first.

## Branches and commits

Branch names are `<type>/<short-description>`, with a Conventional Commits
type: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `ci`, `chore`.

```sh
git fetch origin
git switch -c fix/short-description origin/main
```

A commit message is in English:

```text
<type>: <what changes, lowercase, no period>

<why the change is needed>

<what was considered and rejected, and why>

<how it was verified: the command and its result>
```

The body is the part a reviewer reads in five years. Name the command and
its result; "tests pass" is not enough. If something was not checked, say
so.

## Pull requests

Fill in the template. It asks for the roadmap step, the problem, the
change, the verification and what was not checked.

`main` accepts changes through a pull request only, after the check
`checks` is green. The merge is a merge commit. A squash folds the commit
bodies into one, and a rebase changes the commits that CI checked.

## Writing

Code comments, documentation, commits and pull requests are in English.

- Lists, tables and code blocks before paragraphs.
- Short sentences, active voice.
- No marketing words. Say what a thing does.
- Every command in the documentation has run against the current tree.

## Licence

datumujo is under the [EUPL-1.2](LICENSE). A contribution is under the same
licence. There is no contributor licence agreement.
