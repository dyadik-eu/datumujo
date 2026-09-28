# Requirements

Written on 2026-09-24 for the first user, a code forge. Since v0.1.0 on
2026-09-27, datumujo is a project of its own. The column "Example use"
shows what a forge needs; the requirements do not depend on it.

Stage 1 is a storage engine with a typed Go API. It has no query language.
Stage 2 adds a subset of SQL.

Each requirement has an ID. A test or a measurement closes it. A requirement
is not closed by a statement in this file.

## Scope

| ID | Requirement |
|---|---|
| S-1 | The database runs in the process of the program that uses it. It has no server and no network protocol. |
| S-2 | The code uses the Go standard library only. It uses no cgo and no module outside this repository. |
| S-3 | Supported targets: linux/amd64, linux/arm64, darwin/arm64. |
| S-4 | Not in stage 1: a query language, full-text search, replication. The design must not block replication later (see D-5). |

## Data model

| ID | Requirement | Example use |
|---|---|---|
| M-1 | Tables with a primary key and typed columns: int64, float64, bool, string, bytes, time. Each column can allow null. | accounts, repositories, issues, pull requests, comments |
| M-2 | Secondary indexes, also over more than one column. | issues of a repository by state and number |
| M-3 | Unique indexes. A write that breaks one fails, and the transaction does not change. | one e-mail address per account, one name per repository and owner |
| M-4 | Scans in key order over a range or a prefix, in both directions. | lists of issues, comments by time |
| M-5 | Pagination with a cursor that stays valid when other rows change. | the API returns lists page by page |
| M-6 | A counter that a transaction increments. The value is never given twice, also after a crash. | issue and pull request numbers for each repository |
| M-7 | Schema changes in a transaction: add a table, a column or an index. The schema has a version number. | updates with data migration (plan 3.13) |

## Transactions

| ID | Requirement |
|---|---|
| T-1 | Transactions are serializable. |
| T-2 | One writer at a time. Readers do not block the writer, and the writer does not block readers. Each reader sees one consistent snapshot. |
| T-3 | When a commit returns, the data is on stable storage. |
| T-4 | After a crash at any point, the database opens in the state of the last completed commit. |
| T-5 | A read-only transaction that stays open does not stop the writer. The cost of long readers is documented. |

## Integrity

This is rule 10 of the house rules: a silent failure invents a value.

| ID | Requirement |
|---|---|
| I-1 | Every page has a checksum. A read of a damaged page returns an error that names the page. It never returns data. |
| I-2 | A check command reads the full file and reports each damaged page. It exits 0 when the file is intact, 1 on a finding, and 2 when it could not check. |
| I-3 | The file format is documented and has a version. A release reads the files of the release before it. |
| I-4 | A file with an unknown format version is rejected. It is not read on a best-effort basis. |

## Operation

| ID | Requirement | Example use |
|---|---|---|
| O-1 | A consistent backup while the database is open for writes. | backup of the register and the forge (bauplan B-36) |
| O-2 | A restore makes a database that the check command (I-2) accepts. | same |
| O-3 | Statistics: file size, free pages, number of rows for each table. | operations board |

## Tests and proof

| ID | Requirement |
|---|---|
| P-1 | Crash test: a simulated power loss after each write system call of a workload. After each one, T-4 and I-2 hold. |
| P-2 | Fuzz tests for every function that reads the file format. |
| P-3 | Damage test: flip bits in a file. The database reports each flipped page (I-1) and returns no wrong row. |
| P-4 | Stage 2 uses SQLite as the test oracle for SQL results, in tests only. |
| P-5 | Measured load: the metadata of real projects. The first target is the two in-toto repositories from bauplan B-50: 83 and 177 issues, 390 and 776 pull requests, and 1064 and 2159 issue comments. |

## Design constraints

| ID | Constraint |
|---|---|
| D-1 | A single file for each database, plus at most one log file next to it. |
| D-2 | Writes go to new pages or to a log, never over a page that a reader can still see. |
| D-3 | The engine allocates a bounded amount of memory per transaction. The bound is documented. |
| D-4 | The Go API returns errors. It does not panic on data from the file. |
| D-5 | The commit log can be read in order. A replica can later be built on it. |

## Open

| ID | Question |
|---|---|
| Q-1 | Module path: `github.com/dyadik-eu/datumujo` for now. An own domain can replace it before the first release. |
| Q-2 | Page size. Decided on 26.09.2026 after a measurement: 4096 bytes. See "Page size" in design.md. |
| Q-3 | When the repository becomes public. |

## Stage 2: SQL

Decided on 2026-09-28: datumujo stays an embedded database, as SQLite is
(S-1 holds). Stage 2 adds a core subset of SQL on top of the tables of
stage 1. The requirements of stage 1 hold for every statement.

| ID | Requirement |
|---|---|
| L-1 | Statements: CREATE TABLE, CREATE [UNIQUE] INDEX, DROP TABLE, DROP INDEX, ALTER TABLE ADD COLUMN, INSERT, UPDATE, DELETE, SELECT, BEGIN, COMMIT, ROLLBACK. |
| L-2 | SELECT has expressions with aliases, `*`, WHERE, ORDER BY, LIMIT, OFFSET, DISTINCT, GROUP BY, HAVING, the aggregates count, sum, min, max and avg, INNER JOIN and LEFT JOIN. |
| L-3 | Types are strict. A column holds values of its type only. SQL does not convert a value to fit, except int64 to float64 in arithmetic and comparison. A value that does not fit is an error. |
| L-4 | NULL follows SQL: a comparison with NULL is unknown, WHERE keeps a row only when the condition is true, and IS NULL tests for it. |
| L-5 | Values come in as parameters (`?` and `?NNN`). A program never needs to build SQL text from values. |
| L-6 | An error in the SQL text names the line and column where it is. |
| L-7 | A statement is atomic. A statement that fails changes nothing, and the transaction can go on. |
| L-8 | A table without a primary key gets a hidden int64 key. An INTEGER PRIMARY KEY without a value takes the largest key plus one, as in SQLite. |
| L-9 | A query uses the primary key or an index for `=`, `<`, `<=`, `>`, `>=`, BETWEEN and IN on its leading columns, and for ORDER BY in index order. A test shows the plan for each of these forms. |
| L-10 | Sorting, grouping and DISTINCT without an index hold rows in memory. The bound is documented and configurable. A query over the bound fails with an error and returns no partial result. |
| L-11 | A driver for `database/sql` from the Go standard library. |
| L-12 | `datumujo sql FILE` runs SQL from its input and prints the results. It exits 0 on success, 1 when a statement fails, and 2 when it could not open the file. |

Not in stage 2: subqueries, common table expressions, views, triggers,
foreign keys, CHECK constraints, DEFAULT values, window functions,
full-text search and replication.

### The oracle

P-4 names SQLite as the test oracle. Each form of statement in L-1 and
L-2 runs in datumujo and in the `sqlite3` program, and the results must
be the same. The oracle runs in tests only, as a separate program, so S-2
holds. A test that needs the oracle and does not find it fails in CI; it
does not skip.

Where SQL differs on purpose (L-3), the test states the difference:

| Case | SQLite | datumujo |
|---|---|---|
| `'1' = 1` | false, no conversion across storage classes | error: string and int64 do not compare |
| a string in an INTEGER column | stored as a string | error |
| bool | 0 and 1 | true and false; the oracle compares them as 0 and 1 |
| time | no type | a column type; not in oracle tests |
| rows without ORDER BY | an order | an order; the oracle compares them as a multiset |
