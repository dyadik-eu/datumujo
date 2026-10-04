# Changelog

Each release lists what a program that uses datumujo must know: new
functions, changes to the API or to the file format, and fixes. Before v1,
the API and the file format can change in any release.

## Unreleased

### Added

- DEFAULT on a column in SQL: a constant, or CURRENT_TIMESTAMP for a
  TIMESTAMP column. An INSERT that does not name a column takes it.
  ALTER TABLE ADD COLUMN with a DEFAULT gives it to the rows that exist,
  so the column can be NOT NULL. See "Defaults" in
  [docs/sql.md](docs/sql.md).
- `Column.Default` and `Def.Checks`, with `Table.Checks`: the schema
  holds the text of a default for each column and the texts of the
  checks of a table. The Go API stores them and does not apply them; an
  SQL INSERT applies the default, and an SQL INSERT or UPDATE tests the
  checks.
- `Column.Fill`: the value that rows written before `AddColumn` read for
  the column. With a Fill, an added column can be NOT NULL.
- CHECK on a column and on a table in SQL. An INSERT or UPDATE that makes
  a check FALSE fails with `ErrCheck`; NULL passes. ALTER TABLE ADD
  COLUMN with a CHECK tests the rows that exist. See "Checks" in
  [docs/sql.md](docs/sql.md).
- `AddColumn` takes checks for the table after the column, as a variadic
  argument.
- UNIQUE on a column and on a table in SQL. Each is a unique index
  called `<table>_unique_<n>`; an INSERT or UPDATE that breaks it fails
  with `ErrUnique`. See "Unique" in [docs/sql.md](docs/sql.md).
- `datumujo check` and `Stats.FormatVersion` report the format version.

### File format

Format version 2. The file becomes version 2 with the commit of its
first default, fill or check, and stays at version 2. A file without them
stays at version 1, which v0.2.0 reads. A file of version 2 gets the
error "format version 2 is not supported" from v0.2.0. This release reads
files of versions 1 and 2. See "Format versions" in
[docs/design.md](docs/design.md).

## v0.2.0, 2026-09-29

Stage 2: a core subset of SQL on the tables of stage 1, steps 14 to 25
of [docs/roadmap.md](docs/roadmap.md). The SQL and its limits are in
[docs/sql.md](docs/sql.md).

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
- `DB.Session`: BEGIN, COMMIT and ROLLBACK as SQL text.
- The package `sqldriver`: the driver for `database/sql`, as
  "datumujo".
- `datumujo sql FILE [SQL]` runs SQL from its argument or its input and
  prints the rows. `Session.Script` runs a text of several statements,
  SELECT included.
- `docs/sql.md` describes the SQL of stage 2 and its limits, each limit
  with a test at its edge.

### File format

No change. A file of v0.1.0 opens without a conversion. Measured on
2026-09-29 with a file that v0.1.0 wrote, with a table and an index. It
takes SELECT, GROUP BY, INSERT and UPDATE through `datumujo sql`, and the
check finds it intact.

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
