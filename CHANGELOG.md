# Changelog

Each release lists what a program that uses datumujo must know: new
functions, changes to the API or to the file format, and fixes. Before v1,
the API and the file format can change in any release.

## Unreleased

Stage 2, a core subset of SQL, is in progress. See
[docs/roadmap.md](docs/roadmap.md), steps 14 to 25.

### Added

- `Tx.DropTable` and `Tx.DropIndex` free the pages of the dropped trees.
- `Tx.Savepoint` and `Tx.RollbackTo` set a write transaction back to an
  earlier state. The transaction goes on after the rollback.
- `DB.Exec` and `Tx.Exec` run SQL that writes: CREATE TABLE, CREATE
  INDEX, DROP, ALTER TABLE ADD COLUMN, INSERT, UPDATE and DELETE, with
  parameters. A failed statement changes nothing. Errors are `*SQLError`
  with line and column. The SQL and its differences to SQLite are in
  [docs/requirements.md](docs/requirements.md).
- `DB.Query`, `View.Query` and `Tx.Query` run a SELECT over one table:
  expressions, WHERE, ORDER BY, DISTINCT, LIMIT and OFFSET. The rows are
  `*SQLRows`; close them to end the view of a query of a DB.
- A statement reads a part of the key or of an index where WHERE allows
  it. It uses the order of a scan for ORDER BY. `Options.QueryMemory`
  bounds the rows a statement holds in memory (ErrQueryMemory).
- `ScanOptions.FromExclusive` and `ScanOptions.ToInclusive` for scans.
- GROUP BY, HAVING and the aggregates count, sum, avg, min and max, also
  with DISTINCT.
- INNER JOIN and LEFT JOIN, with a lookup by key or index where ON or
  WHERE allows one.

### File format

No change. A file of v0.1.0 opens without a conversion.

## v0.1.0, 2026-09-27

The first release: stage 1 of the roadmap, steps 1 to 13.

- One database file and one log file, in the process of the program.
- Transactions with one writer and readers that never wait. A commit is
  on stable storage when it returns.
- A checksum on every page. A damaged page gives an error that names it.
- Typed tables with a primary key, secondary and unique indexes, scans in
  both directions with a cursor, and counters.
- `datumujo check FILE` reports each damaged page. Exit 0 when intact, 1
  on a finding, 2 when it could not check.
- Backup while writes go on, and restore.
- File format version 1, with a page size of 4096 bytes for new files.
