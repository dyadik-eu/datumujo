# Roadmap, stage 1

Each step is about one pull request. A step is done when its tests are green
in CI and the requirements it names are closed by a test or a measurement.
The design is in [design.md](design.md).

| Step | Content | Closes |
|---|---|---|
| 1 | Design and this roadmap | |
| 2 | File interface: operating system and simulated disk with power loss, each with its own tests | basis for P-1 |
| 3 | Page format: header page, checksum trailer, format version, errors that name the page; fuzz test of the reader | I-1, I-4, P-2 (pages) |
| 4 | Log: frames, commit records with chained checksums, recovery on open; crash test at page level | T-3, T-4, D-1, P-1 (pages) |
| 5 | Snapshots and checkpoint: readers that do not block, one writer, checkpoint only where no reader needs the old page | T-2, T-5, D-2 |
| 5a | Exclusive lock on the file at Open, on the operating system and on the simulated disk | S-1 |
| 6 | B+tree over pages: byte keys and values, get, put, delete, scans in both directions, free pages; tested against a map as oracle; crash test with the tree | M-4 (bytes), P-1 (tree) |
| 7 | Page size: measure with the load of P-5 and set the value for new files | Q-2 |
| 8 | Typed tables: schema in the file, column types, null, row encoding, primary key, schema changes with a version | M-1, M-7 |
| 9 | Indexes and scans: secondary and unique indexes, range and prefix scans, cursors, counters | M-2, M-3, M-4, M-5, M-6 |
| 10 | Check command, damage test, statistics | I-2, P-3, O-3 |
| 10a | Log: tell a damaged frame from a commit that did not finish; refuse to open a damaged log instead of cutting it | I-1, T-4, P-3 (log) |
| 11 | Backup while writes go on, restore | O-1, O-2 |
| 12 | Load test with the metadata of the in-toto repositories; memory bound per transaction | P-5, D-3 |
| 13 | Public API at the root of the module; checkpoint by log size | use from other modules |

Stage 1 is tagged as v0.1.0. Its first user is a code forge that keeps its
metadata in datumujo.

Not in stage 1: a query language, full-text search, replication (S-4). The
log of step 4 keeps replication possible (D-5).

# Roadmap, stage 2

The requirements are in [requirements.md](requirements.md), under
"Stage 2: SQL". The rules of stage 1 hold. A step is one pull request. It
is done when its tests are green in CI and a test or a measurement closes
the requirements it names.

| Step | Content | Closes |
|---|---|---|
| 14 | Requirements and this roadmap | |
| 15 | Engine: drop a table and an index and free their pages; a savepoint in the write transaction | L-1 (drop), L-7 (basis) |
| 16 | Lexer and parser for the subset of L-1 and L-2, with positions in errors; fuzz test: a parsed statement prints to text that parses to the same tree | L-6 |
| 17 | Values and expressions: types, NULL, operators, CAST and the core functions; the oracle in tests | L-3, L-4, P-4 (basis) |
| 18 | DDL and writes: CREATE, DROP, ALTER TABLE ADD COLUMN, INSERT, UPDATE, DELETE, parameters, the hidden key; Exec on DB and Tx; a statement rolls back to its savepoint | L-1, L-5, L-7, L-8 |
| 19 | SELECT over one table: expressions, WHERE, ORDER BY, LIMIT, OFFSET, DISTINCT; Query and Rows | L-2 (part) |
| 20 | Planner: key and index for L-9, index order for ORDER BY; the memory bound | L-9, L-10 |
| 21 | Aggregates, GROUP BY and HAVING | L-2 (part) |
| 22 | INNER JOIN and LEFT JOIN, with a lookup by key or index where one fits | L-2 |
| 23 | Driver for `database/sql` | L-11 |
| 24 | SQL shell in the command | L-12 |
| 25 | Random statements against the oracle; the SQL dialect and its limits in the docs | P-4 |

Stage 2 is tagged as v0.2.0.

# Roadmap, stage 3: v0.3

The requirements are L-13 to L-19 in [requirements.md](requirements.md),
under "Stage 3". The rules of the stages before hold.

| Step | Content | Closes |
|---|---|---|
| 26 | Requirements and this roadmap | |
| 27 | Format version 2: the schema holds defaults and checks. A file becomes version 2 with its first default or check, and not before. Files of version 1 open. A test keeps a file that v0.2.0 wrote, and one that v0.2.0 refuses | L-19, I-3, I-4 |
| 28 | DEFAULT: constants and CURRENT_TIMESTAMP; INSERT without a value; ALTER TABLE ADD COLUMN gives the rows that exist the value, with NOT NULL allowed | L-13 |
| 29 | CHECK on a column and on a table, for INSERT and UPDATE | L-14 |
| 30 | UNIQUE on a column and on a table, as unique indexes with a name that the schema keeps | L-15 |
| 31 | Subqueries without columns of the query around them: scalar, IN, EXISTS | L-16 (part) |
| 32 | Subqueries with columns of the query around them | L-16 |
| 33 | INSERT ... ON CONFLICT DO NOTHING and DO UPDATE, with `excluded` | L-17 |
| 34 | EXPLAIN, in the API, the driver and the shell | L-18 |
| 35 | The oracle over all new forms, mixed with the forms of stage 2; docs/sql.md; the limits of the new forms | P-4 |

Stage 3 is tagged as v0.3.0 when step 35 is done.

# Towards v1.0

Decided on 2026-09-28: stable before broad. The release v1.0 promises a
file format and an API that stay. It has the SQL core and the
extensions that most programs need. Broader SQL can follow in 1.x releases.

The stages have no dates. A stage is done when a test or a measurement
in the repository meets each of its criteria, as in stages 1 and 2.
Each stage gets a roadmap of steps before its work starts.

| Version | Content | Done when |
|---|---|---|
| v0.2 | SQL core: steps 14 to 25 above | step 25 is done |
| v0.3 | SQL for programs: DEFAULT, CHECK, UNIQUE on a column, subqueries in WHERE and in the select list (scalar, IN, EXISTS), INSERT ... ON CONFLICT, EXPLAIN; steps 26 to 35 below | each form runs against the oracle, and each difference to SQLite is in the oracle table |
| v0.4 | Speed: statistics for the planner, the order of joins, sorting past the memory bound | a benchmark suite runs from the repository against SQLite, and each release publishes its results |
| v0.5 | Release candidate for the format | the file format has a specification from which a second reader can be written; the repository holds a file of every release, and each release reads all of them; each fuzz target ran 24 hours; the crash test ran on real disks |
| v1.0 | The promise | see below |

## What v1.0 promises

- The API follows semantic versioning. A program that builds against
  v1.0 builds against every 1.x.
- Every 1.x release reads every file that v1.0 or a later 1.x wrote
  (I-3). A file with a format that a release does not know is refused,
  not read in part (I-4).
- Every limit is documented with its number: sizes, memory bounds, the
  largest key.
- The latest 1.x gets security fixes, as SECURITY.md describes.

## After v1.0

Views, foreign keys, window functions, full-text search and replication
(S-4) are candidates for 1.x releases. None of them may change the file
format in a way that 1.0 cannot read.

# Work packages of v0.4 to v1.0

These are the parts of each version, without steps. Each version gets
its steps before its work starts, because the open decisions below
change them. The criteria of each version are in the table "Towards
v1.0" above.

## v0.4: speed

- Statistics for the planner: rows of each table and distinct values of
  each index, kept by ANALYZE.
- The order of the tables of a join, by the cost that the statistics
  give.
- A sort of ORDER BY with LIMIT that keeps only the rows of the limit.
- A sort past the memory bound, if the benchmark shows that programs
  need it.
- Prepared statements that keep their plan, in the API and the driver.
- A benchmark suite against SQLite in the repository, with its results
  for each release.

Open decisions:

- the workloads of the benchmark
- whether a sort may write a temporary file next to the database; D-1
  allows the database file and its log only

## v0.5: the format for 1.x

- A specification of the file format from which a second reader can be
  written.
- A file of every release in the repository, and a test that each
  release reads all of them.
- 24 hours of fuzzing for each fuzz target before the release.
- The crash test on real disks, beside the simulated disk of stage 1.
- The file format frozen for 1.x.

Open decisions:

- how the crash test cuts the power of a real disk
- whether a second reader is part of the tests

## v1.0: the promise

- A review of the API: names, errors, and what stays internal.
- Documentation of every exported name.
- The promises of the section "What v1.0 promises" above, each with a
  test.
