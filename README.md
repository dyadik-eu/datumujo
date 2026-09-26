# datumujo

An embedded database for Go, written with the Go standard library only.
datumujo is Esperanto for "data container".

**Status: no code yet.** This repository holds the requirements. The first
stage is a storage engine with a typed Go API. A subset of SQL is the second
stage.

## Goals

- One database file, used in the process of the program. There is no server.
- Transactions that stay correct after a crash or a power loss.
- A checksum on every page. The database returns an error on damaged data.
  It never returns wrong data.
- No dependency outside the Go standard library, and no cgo.
- A documented, versioned file format.

The first user is [abelejo](https://github.com/dyadik-eu/abelejo), a code
forge. Its needs define the first stage. The requirements are in
[docs/requirements.md](docs/requirements.md), the design in
[docs/design.md](docs/design.md), and the steps in
[docs/roadmap.md](docs/roadmap.md).

## Licence

[EUPL-1.2](LICENSE).
