# datumujo

An embedded database for Go, written with the Go standard library only.
datumujo is Esperanto for "data container".

**Status: the first stage is built and tagged as v0.1.0.** It is a
storage engine with a typed Go API. It has tables with a primary key,
secondary and unique indexes, scans and counters. One writer runs while
readers never wait, and there are backup and a check command. A subset of SQL is the second stage.
The API and the file format can still change before v1.

## Goals

- One database file, used in the process of the program. There is no server.
- Transactions that stay correct after a crash or a power loss.
- A checksum on every page. The database returns an error on damaged data.
  It never returns wrong data.
- No dependency outside the Go standard library, and no cgo.
- A documented, versioned file format.

The first user is a code forge, and its needs shaped the first stage. The
requirements are in
[docs/requirements.md](docs/requirements.md), the design in
[docs/design.md](docs/design.md), and the steps in
[docs/roadmap.md](docs/roadmap.md).

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) describes how a change gets in.
Report a security problem or wrong data in private, as
[SECURITY.md](SECURITY.md) describes.

## Licence

[EUPL-1.2](LICENSE).
