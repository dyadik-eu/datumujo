# Design, stage 1

Written on 2026-09-26. The requirements are in [requirements.md](requirements.md).
This file records how stage 1 meets them, and which alternatives were
rejected and why. A section changes only with the code that it describes.

## Writes go to a log of page images

The database is a file of fixed-size pages. A commit does not change that
file. It appends the new version of each changed page to a log file next to
it. Then it appends a commit record and calls fsync on the log. When fsync
returns, the commit is durable (T-3).

A reader starts with a snapshot: the position of the last commit record in
the log. To read a page, it takes the newest version of that page in the log
up to its snapshot. If the log has no version of the page, it reads the
database file. So a reader never sees a later commit, and it never waits for
the writer (T-2).

A checkpoint copies the newest page versions from the log into the database
file, calls fsync on the file, and then starts a new log. It copies a page
only when no open reader can still need the older version of it (D-2). A
reader that stays open stops the checkpoint, and the log grows while it is
open. This is the cost of long readers (T-5).

On open, the database reads the log from the start and uses it up to the
last complete commit record. A commit record carries a checksum over the
frames of its commit, chained to the checksum of the commit before. A frame
after the last valid commit record is from a commit that did not finish, and
is ignored (T-4).

A damaged frame breaks the chain as a commit that did not finish does.
Open tells the two apart by what follows. Each frame carries the number of
its commit in the generation. Commits are written one after the other, and
each is synced before the next starts. So if a frame of a commit after the
next one follows the break, the commit with the break was complete. That
is damage, and Open refuses the log: cutting it would drop that commit and
every later one.

A frame after the break counts if it chains to the frame before it. Two
checksums of that frame qualify: the one it stores, and the one computed
from its content. One damaged frame spoils only one of them. So a single
flipped bit anywhere before the last commit is found. This holds for a
checksum field too, and when the next commit has one frame only.

Damage in the last commit remains. It looks exactly like a commit that did
not finish, because nothing follows it. Open uses the log up to the commit
before and reports how many bytes it left. The check command (I-2) shows
them as a finding. The next commit cuts these bytes off first. Otherwise a
short new commit could leave old frames behind it.

Each generation of the log has a random salt in its header. The chain
starts from it, and every frame checksum covers it. So frames of an
earlier generation never chain, not even to each other. A new generation
cuts the file and syncs the cut before it writes the new header. Otherwise
a power loss could keep the header and lose the cut, and the old frames
would stand as bytes after the last commit.

This meets:

| ID | How |
|---|---|
| D-1 | one database file and one log file |
| D-2 | a commit writes only to the log; a checkpoint writes a page only when no reader needs its older version |
| D-5 | the log holds the commits in order; a replica can later read it |
| T-2 | readers use a snapshot and never take the writer lock |
| T-3 | a commit returns after fsync of the log |
| T-4 | recovery uses the log up to the last valid commit record |

### Rejected: copy-on-write tree with two root pages

LMDB and bbolt write new pages for every change and then switch one of two
root pages. Crash safety is simpler, because there is no log to recover. But
there is no ordered log of commits, which D-5 asks for, and every commit
copies the path from the changed leaf to the root.

### Rejected: rollback journal

SQLite by default saves the old page versions in a journal and then writes
the database file in place. While it writes, no reader can read. This
breaks T-2.

Every commit also writes page 0, the header with the new page count. So a
header page that a checkpoint left torn in the file is replaced by its
newest image in the log, like any other page. Open reads the page size from
the log header for this case.

Commits have numbers that go on across generations of the log while the
database is open. A snapshot holds a commit number, and after a new
generation starts, it never takes a commit of that generation for its own.

## A new file is created in one step

For a new database, Open writes the header to a file with the suffix
"-new". It calls fsync on that file and then renames it. So a database file that
exists had its header on the disk once. Written in place, a power loss
during creation left a torn header and no log. That file could not be
opened, and it looked like a database cut short by damage. Creating it again
without a word would hide such damage.

## Pages carry their own checksum

Every page ends with a trailer: a CRC-32C over the page and its page number.
A page that was written to the wrong place, or read from the wrong place,
fails the check just like a damaged page. A failed check is an error that
names the page (I-1). The read returns no data.

Page 0 is the header. It holds a magic value, the format version, the page
size and the page count. It also holds the head and length of the free list,
and four root slots. A
file with a format version this code does not read is rejected (I-4).
Every page number in the header must point into the file. The format
versions are in "Format versions" below.

Page 0 belongs to the store. Readers and the writer do not read it as a
page; they read its fields. Inside a write transaction the fields show the
changes of that transaction.

## Free pages

A freed page goes on a list whose head is in the header. Each free page
holds a mark and the number of the next one. Allocate takes the page freed
last, before the file grows. It checks the mark and the next number first.
A page on the list without the mark means that the list is damaged. Handing
it out would give away data.

A freed page can be reused in the next commit at once. A snapshot of an
earlier commit still reads its old content from the log or from the file.
No checkpoint changes the file while that snapshot is open.

The page size is a field of the header. New files get 4096 bytes. The tests
run with more than one size.

## Page size

The value for new files comes from a measurement with the load of P-5
(Q-2). `internal/load` loads the issues, pull requests and comments of
in-toto/in-toto-golang and in-toto/in-toto into four trees. It runs only
with `DATUMUJO_LOAD` set; its package comment has the commands that fetch
the data.

The data, fetched on 26.09.2026:

| | in-toto-golang | in-toto |
|---|---|---|
| issues | 83 | 177 |
| pull requests | 390 | 775 |
| issue comments | 1064 | 2159 |
| review comments | 810 | 2460 |

9343 rows, 13.4 MB of keys and values. 16 comments belong to in-toto #380.
The API does not return this pull request any more, so a prefix scan cannot
find them. The test counts them apart.

Environment: Go 1.26.5, darwin/arm64, Apple M1 Pro, 16 GiB, macOS 27.0,
APFS, commit a132fc3. Each time is the median of three runs. A commit holds
100 rows. The load ends with a checkpoint.

| page size | file | pages | overflow | depth | load | get all issues | comments by prefix | full scan |
|---|---|---|---|---|---|---|---|---|
| 1024 | 16.7 MiB | 17060 | 16037 | 4 | 633ms | 11.1ms | 23.9ms | 15.0ms |
| 2048 | 18.5 MiB | 9496 | 8376 | 3 | 647ms | 11.8ms | 24.8ms | 12.3ms |
| 4096 | 20.5 MiB | 5235 | 4090 | 3 | 639ms | 16.4ms | 34.5ms | 11.2ms |
| 8192 | 22.8 MiB | 2911 | 1994 | 3 | 618ms | 18.4ms | 47.2ms | 11.4ms |
| 16384 | 25.2 MiB | 1609 | 911 | 2 | 624ms | 24.1ms | 63.3ms | 11.0ms |

What the numbers show:

* The load takes the same time at every size. A CPU profile puts it in the
  writes and syncs of the commits.
* Most pages are overflow pages: bodies and diff hunks. The last page of
  each chain is partly empty, so the file grows with the page size.
* Lookups and prefix scans get slower as pages grow, although the tree gets
  flatter. There is no page cache. Each node read is one pread of a whole
  page, and in the profile the pread takes 36 % of all CPU time. A larger
  page costs more per read.

Why 4096 and not 2048, which reads about 30 % faster here:

* The key limit follows from the page size. It is 232 bytes at 1024, 488 at
  2048 and 1000 at 4096. An index key holds the indexed value and the
  primary key. 488 bytes leave little room for a name and a path.
* The read cost measures the read path without a cache. A page cache turns
  it around: a flatter tree then needs fewer reads and fewer cached pages.
* 4096 is the page size of the operating system on linux/amd64. It is also
  the size that SSDs and file systems write in one piece.

The measurement is repeated with the load test of step 12. If the engine
gets a page cache, it is repeated then as well.

Repeated on 26.09.2026 with typed tables and indexes: the load test of
"Load test" below, three rounds, each size once per round. The median
of three runs:

| page size | file | largest transaction | load | get every issue | comments by prefix | check | backup |
|---|---|---|---|---|---|---|---|
| 2048 | 19.0 MB | 1.53 MB | 6.18 s | 16.3 ms | 36.9 ms | 97 ms | 207 ms |
| 4096 | 20.7 MB | 1.64 MB | 6.48 s | 25.8 ms | 55.9 ms | 121 ms | 232 ms |
| 8192 | 23.2 MB | 1.84 MB | 6.61 s | 26.3 ms | 71.5 ms | 126 ms | 217 ms |

The picture is the one above. 2048 reads about a third faster here and
makes a smaller file. The reasons for 4096 hold: the key limit, and a
read path without a cache. New files keep 4096 bytes.

## The tree

Tables and indexes are B+trees of byte keys and byte values, ordered by
bytes.Compare. An entry takes at most a quarter of a node, so a node that
grows past its page splits into two that fit. A value that would make its
entry larger goes to a chain of overflow pages.

The root page of a tree never changes. When the root splits, its content
moves to two new pages, and the root becomes an internal node over them. So
the number of the root is kept once, in a root slot of the header.

A node below a quarter of a page merges with a neighbour when both fit in
one page. Entries do not move between neighbours. So a node can stay below
a quarter; this costs space, not correctness. An internal root with one
child takes over the content of that child.

A cursor holds the path from the root to its leaf. It needs no links
between leaves, so a split or merge changes no neighbour.

A node is decoded, changed and encoded again for every change. The decoder
checks each length against the page. It accepts only the shortest form of a
number. So it returns an error on content that this code does not write.

## Tables

A table is a tree. Its key is the primary key of a row, its value holds
the other columns. The schema lists the tables with their columns, key
and root page. It is one record in a catalog tree at root slot 0. Each
schema change writes the record again with the next version number, in the
same transaction as the rows. A rollback removes both.

Column types are int64, float64, bool, string, bytes and time. Go values
of exactly these types are accepted; an `int` for an int64 column is an
error. A string must be UTF-8. A key column cannot allow null.

The key form keeps the order of the values, so a scan in key order is a
scan in value order:

| Type | Key form |
|---|---|
| int64 | 8 bytes big-endian, sign bit flipped |
| float64 | 8 bytes: sign bit set for a positive number, all bits flipped for a negative one |
| bool | one byte, 0 or 1 |
| string, bytes | the bytes, each 0x00 as 0x00 0xFF, then 0x00 0x01 |
| time | seconds as int64 key form, then nanoseconds as 4 bytes big-endian |

The end mark 0x00 0x01 sorts below every other byte after 0x00. So a
string sorts before each longer string it starts. A key over two columns
keeps the order of the first column.

Three values change on the way through a key:

* A float key of -0 is stored as +0. The two are equal, and one key must
  not have two forms.
* NaN is rejected in a key. It has no place in the order.
* A time comes back in UTC. The instant is kept, the location is not.

Outside the key, a float keeps its bits, NaN and -0 included, and a time
keeps its instant.

A stored row starts with the number of columns outside the key. A bitmap
follows, with one bit for each null. Then come the values that are not
null. A column added later
must allow null. A row written before it has fewer columns, and the
missing ones read as null. So adding a column rewrites no row.

The decoders of keys, rows and the schema accept only what the encoders
write. Numbers are in their shortest form. No bit is set past the last
column, and no byte follows the end. A fuzz test checks for each decoder that an input it
accepts encodes to the same bytes.

## Format versions

Format version 1 is the file of v0.1.0 and v0.2.0. Version 2 adds to the
schema a default and a fill for each column, and the checks of each
table. That is schema format 3. The
pages, the log, the keys and the rows are the same in both versions.

This code reads both (I-3). A new file has version 1. The commit that
writes the first default, fill or check raises the file to version 2
(requirement L-19). So a program that uses only what v0.2.0 knows keeps
files that the old release reads. A file of version 2 gets the error for
an unknown version from v0.2.0, before it reads a page past the header
(I-4).

The rules hold in both directions:

* A schema without a default, a fill or a check is written in schema
  format 2, the format of v0.2.0. A schema with one is written in
  format 3.
* A file of version 1 with a schema of format 3 is damaged. So is a
  schema of format 3 without a default, a fill or a check: that is not
  what the encoder writes.
* A version is never lowered. When the last default or check is dropped,
  the file stays at version 2.

The table layer stores the texts and does not read them. The SQL layer
parses them (L-13, L-14). A fill is a value; "Defaults and fills" says
what reads it.

### Rejected: lower the version again

A file could go back to version 1 when the last default or check is
dropped. It would not help v0.2.0 at once. Open reads the header in the
file before the log. A lowered version reaches the file only at a
checkpoint. Until then, v0.2.0 refuses the file anyway. A version that
only goes up is one state less to test.

### Rejected: raise every file to version 2

v0.3 could write version 2 into each file it opens. Nothing in such a
file needs version 2, and v0.2.0 could no longer read it.

The tests are `TestFormatVersion` and `TestExtendedSchemaInVersion1` in
`internal/table`, and `TestFileOfV020` and `TestFileOfV2` in the root
package. `testdata/format/v0.2.0.db` is a file that v0.2.0 wrote.
`tests/format.test.sh` builds the tag v0.2.0 and checks that it writes
this file again, byte for byte. It then checks that v0.2.0 reads it and
refuses `testdata/format/v2.db`, with a positive control for the build
of the tag.

## Defaults and fills

The schema keeps the DEFAULT of a column as text, the way
`sqlparse.Default` prints it: `-3`, `'a''b'`, `X'00ff'`, `TRUE`,
`CURRENT_TIMESTAMP`. An INSERT parses the text for each column it does not
name. It fits the value to the column as it fits a value of the
statement. CURRENT_TIMESTAMP is read once per statement, in UTC, so all
rows of one INSERT get the same time.

A column that ALTER TABLE adds has a fill as well: the value of its
default, stored as a value. A row written before the column ends before
it, and the table layer gives such a row the fill instead of null. So
adding a column still rewrites no row, and the column can be NOT NULL.
The text of the default stays next to it for the INSERTs after.

A default is checked when the table or column is made, not when a row
is written. It must fit the type of its column; an INTEGER fits a REAL
column when a REAL holds it exactly. DEFAULT NULL on a column that
allows NULL is the same as no default and stores nothing, so the file
stays at version 1.

### Rejected: parse the default in the table layer

The table layer could read the text of a default and fill old rows from
it. It would need the SQL parser and the rules that fit a value to a
column. And it would evaluate text on every read of an old row. A fill
is the value, fixed when the column is added.

### Rejected: rewrite the rows that exist

ALTER TABLE could write the default into every row. It costs a write of
the whole table. And the transaction limit (`Options.MaxTxBytes`) would
bound the size of a table that can take a new column. A fill costs a
few bytes in the schema.

### Rejected: an expression as a default

SQLite takes `DEFAULT (expr)` in CREATE TABLE and refuses it in ALTER
TABLE. L-13 names constants and CURRENT_TIMESTAMP. A constant is checked
against its column once; an expression could fail on a later INSERT.

## Checks

The schema keeps each CHECK of a table as the text that
`sqlparse.Expr.String` writes. A check of a column and a check of the
table end in the same list. Both may read any column of the table, as
in SQLite. An INSERT or an UPDATE parses and compiles the
checks of its table once, and tests each row it writes after NOT NULL.
A FALSE fails the statement with ErrCheck; the savepoint of the
statement takes back the rows it wrote before. NULL passes, as in SQL.

A check is compiled when the table or column is made. It must be
BOOLEAN, and the same row must give the same answer each time: no
parameter and no aggregate. So a check that could never run is an error
at once, not on the first row. A check that the schema holds and that
does not compile is damage; the first statement that writes the table
reports it.

ALTER TABLE ADD COLUMN with a CHECK tests the rows that exist, with the
fill of the new column, as SQLite does. It reads the table once. A FALSE
fails the statement, and its savepoint takes the column back.

### Rejected: test the rows that exist only against the fill

A check of an added column can read the other columns, so the fill
alone does not answer it. The scan costs a read of the table, and no
write.

## Unique

Each UNIQUE of CREATE TABLE becomes a unique index of the new table. The
table layer has kept unique indexes since stage 1, with the rule that a
NULL never conflicts. So UNIQUE adds no code to the write path and no
field to the schema. A file with UNIQUE alone stays at format version 1.

The index is called `<table>_unique_<n>`, with the first n from 1 that
no index and no table has. Some UNIQUE make no index, since the rows
they allow are the same. That is a UNIQUE over the columns of the key,
and one over the columns of an earlier UNIQUE, in any order.

### Rejected: a mark for the index of a UNIQUE

SQLite refuses DROP INDEX on the index of a UNIQUE and reserves the
prefix `sqlite_autoindex_` for these names. Both need a mark in the
schema or a reserved name. A mark would raise the file to version 2 for
UNIQUE alone. A reserved prefix would make index names that v0.2.0 takes
an error. So the index is a plain one, and DROP INDEX drops its rule.

## Subqueries

The parser reads `(SELECT ...)`, `EXISTS (SELECT ...)` and
`x IN (SELECT ...)` as nodes that hold a Select. The engine prepares
each as a Query of its own when it compiles the expression around it.
The statement keeps a list of its subqueries, and each run of the
statement binds them to its source and parameters. The resolver of the
expression around is passed on. So a name that only the query around
knows is a correlated subquery, roadmap step 32, and not an unknown
column.

A subquery of step 31 reads no column of the query around. Its answer
is the same for each row, so it runs once, at its first use, and keeps
the answer for the run. A test counts the scans of each table: one per
run, for a value, for EXISTS and for IN.

A value subquery reads at most
two rows, enough to tell one from more. EXISTS reads one. IN reads all
rows, counts them toward `QueryMemory`, and sorts the values. A lookup
is then a binary search with the comparison of the engine, so 1.0 is in a
set that holds 1.

All subqueries of a statement see the state before it. UPDATE and
DELETE found their rows before the first write already. INSERT now
computes the values of all rows first, then writes them. Measured in
SQLite 3.54.0 over an empty t: an INSERT of three rows with
`(SELECT count(*) FROM t)`, the same, and `(SELECT count(*) + 10 FROM t)`
gives 0, 0 and 10. Row by row, it would give 0, 1 and 12.

`Compile` takes no subquery; `compileWith` does. A CHECK and the values
of the planner use `Compile`. A CHECK reads one row and must give the
same answer each time, so a subquery there is refused by name. The
planner takes IN as a list of values to scan; IN with a subquery has an
empty list, so the planner skips it.

### Rejected: run each subquery at the start of its statement

That would give the same answers for less code. But a subquery that no
row needs would run too, and fail too. In SQLite,
`CASE WHEN FALSE THEN (SELECT a FROM t) ELSE 5 END` is 5 with two rows
in t, and a WHERE over an empty table runs no subquery.

### Rejected: a hash set for IN

A hash needs one key for values that compare equal, such as 1 and 1.0.
The sorted list uses the comparison that `=` uses, so IN and `=` cannot
disagree.

### Rejected: the first row of a value subquery, as in SQLite

L-16 asks for an error. A query that gives more rows than its author
expected is a wrong answer that no one sees.

## Indexes

An index is a tree of its own. The key of an entry is the index columns of
a row, then the row's primary key. The value is empty. The primary key
makes each entry distinct, so one layout serves unique and other indexes.

A column that allows null gets a mark before its value in an index: 0 for
null, 1 for a value. So null sorts first. A float in an indexed column
must not be NaN, as in a key.

A unique index refuses a write when an entry with the same index values
exists. The key forms of the index values are prefix-free, so this is one
seek. A row with a null in the index conflicts with no row, as in SQL.

A write checks everything before it changes a page: the row, the key
lengths and each unique index. So a refused write changes nothing, and
the caller can go on in the same transaction. An update changes only the
entries whose index values change.

CreateIndex fills the new index from the rows. If a row does not fit, it
frees the pages of the new tree and leaves the schema as it was.

A scan through an index reads the row by its primary key. It checks that
the row exists and has the entry's index values. Otherwise it returns an
error: an index that does not match its table must not give a wrong row.

## Scans and cursors

A scan takes values for the leading columns of the key or of an index.
There are three kinds: a prefix, a lower bound where it starts, and an
upper bound before which it stops. It runs in both directions. The bounds become byte keys.
The end of a prefix is its successor: the prefix with trailing 0xFF bytes
removed and the last byte incremented.

A cursor is the byte key of the last row a page returned. The next page
starts at the first key after it, whatever rows exist then. So a cursor
stays valid when rows are added or deleted. A row that exists through the
whole walk comes exactly once. A row added behind the cursor does not
come.

## Drop and savepoints

DropTable frees the pages of the table and of each of its indexes, then
writes the schema without the table. DropIndex does the same for one
index. The pages go on the free list, and the next allocation takes them.
The check shows that no page is lost: after a drop, every page is the
header, free, or in a tree that the schema names.

A savepoint is the state of the write transaction at one point. It holds
the header, the map of changed pages, the pages freed in it, and the
schema.
RollbackTo sets the transaction back to that state, and the transaction
goes on.

A savepoint copies the map, not the pages. This works because a
write never changes a page of the map in place; it puts a new one. The
cost is one copy of the map per savepoint, at most MaxTxBytes divided by
the page size entries. Stage 2 takes a savepoint before each SQL
statement, so a failed statement changes nothing (L-7).

## SQL text

The package `internal/sqlparse` reads SQL into a tree and prints a tree
back to SQL. It knows no tables and no values. A name that does not exist
is an error of a later step.

Operators bind as in SQLite. From the lowest, the levels are:

| Level | Operators |
|---|---|
| 1 | OR |
| 2 | AND |
| 3 | NOT |
| 4 | `=` `!=` IS IN BETWEEN LIKE |
| 5 | `<` `<=` `>` `>=` |
| 6 | `+` `-` |
| 7 | `*` `/` `%` |
| 8 | `\|\|` |
| 9 | unary `-` and `+` |

 An IN list ends in a parenthesis.
So an operator that binds more tightly after it takes the whole IN as
its left operand: `a IN (1) + 2` is `(a IN (1)) + 2`, as in SQLite.

The printer puts parentheses around every operator. So a test reads the
grouping from the text, and the oracle can check it. SQLite computes
each random expression twice: as written, and as printed. The two values
are the same when this parser groups the operators as SQLite does. Three
controls with a wrong grouping must give different values; otherwise
the comparison proves nothing.

Rules of the text:

| Rule | Why |
|---|---|
| A minus directly before a number is part of the number. | -9223372036854775808 is an int64; the number without the minus is not. |
| An integer literal past int64 is an error. A REAL literal past float64 is an error. | L-3: no silent conversion. SQLite makes a REAL and Inf. |
| A type takes no length: `VARCHAR(20)` is an error. | The column would not check the length, and the text would say that it does. |
| Type names are INTEGER, INT, BIGINT, REAL, DOUBLE [PRECISION], FLOAT, BOOLEAN, BOOL, TEXT, VARCHAR, BLOB, BYTEA and TIMESTAMP. | Each maps to one column type of stage 1. |
| ADD, BEGIN, COLUMN, COMMIT, EXISTS, IF, INDEX, KEY, ROLLBACK and TRANSACTION are not reserved. | A column can be called key or index. The printer puts such a name in double quotes. |
| A name without quotes is in lower case. A name in double quotes keeps its case. | As in SQL. |
| `?` is one more than the largest parameter number before it in the statement; `?N` names N. | As in SQLite. The printer writes every parameter as `?N`. |
| Expressions nest at most 200 levels deep. | Deeper text gives an error, not a stack overflow. |

An error names the line and the column, in characters (L-6). A fuzz test
parses any text. If it parses, the printed text must parse to the same
tree and print the same again.

## Values and expressions

The package `internal/sqlexec` compiles an expression, then runs it for
a row with the parameters. A value is a value of the table layer: nil,
int64, float64, bool, string, []byte or time.Time. So a row goes into a
table as it is.

The compiler checks the types before the run (L-3). `'1' = 1`, `1 + 'a'`
and `WHERE 1` are errors, with the position, before any row is read.
The type of a parameter is known only at the run, and so is the type of
NULL. The run checks them there. A value of an expression whose type is
known before the run has that type; a fuzz test checks this. So a CASE
with an INTEGER branch never gives a TEXT from a parameter.

The rules follow SQLite where a value fits its type:

| Rule | Detail |
|---|---|
| INTEGER with INTEGER | stays INTEGER; `/` cuts towards zero, `%` keeps the sign of the left side |
| INTEGER with REAL | gives REAL |
| comparison of INTEGER and REAL | exact: 9007199254740993 is greater than 9007199254740992.0 |
| NULL | a comparison with NULL is NULL; AND, OR and NOT use three values |
| AND, OR | the right side does not run when the left side decides |
| LIKE | `%` any run, `_` one character; A to Z match their lower case, other letters only themselves |
| lower, upper | A to Z only |
| round | half away from zero, on the exact binary value: `round(2.675, 2)` is 2.67 |
| min, max with more arguments | of equal values, min returns the last and max the first |
| `-x` | `0 - x`, so `-(0.0)` is 0.0; a minus in front of a number literal is part of it |

Where SQLite converts a value or makes one up, datumujo refuses. The
cases are in the oracle table of the requirements. A division by zero
and an overflow are errors, not NULL and not a REAL.

The oracle test writes 6000 random expressions of each type per run.
It compares every value with SQLite: a REAL by its bits through
`ieee754_to_blob`, TEXT and BLOB as hex. Only the named differences
are left out. For `substr` of an empty BLOB, the SQLite text makes up
for the difference, so these cases are compared too. Six controls run
other text in SQLite, and the comparison must see each difference.

## Writes in SQL

`Exec` runs CREATE, DROP, ALTER TABLE ADD COLUMN, INSERT, UPDATE and
DELETE on the tables of stage 1. A SQL table is a table of the engine:

| SQL | Engine |
|---|---|
| a column | a column of the same type; NULL allowed unless NOT NULL or in the key |
| PRIMARY KEY | the key |
| no PRIMARY KEY | a first column `rowid` of type int64 as the key; `*` and INSERT without a column list do not show it |
| an index | an index; its name is unique across all tables, as in SQLite |

An INSERT can give no value to the hidden key or to an INTEGER key of
one column. Then the key is the largest key plus one, or 1 in an empty
table (L-8).
The largest key -5 gives -4, as in SQLite. After the largest int64 there
is no next key, and the INSERT fails.

An UPDATE and a DELETE read every row they change before they write
one; a write during a scan leaves the scan undefined. Every SET reads
the old row, so `SET a = b, b = a` swaps. A new key moves the row: the
old row is deleted and the new one inserted.

Each statement runs after a savepoint (L-7). When it fails, the
transaction goes back to the savepoint and goes on. This covers a
statement that goes past MaxTxBytes too. An error of the engine, such as
ErrUnique, reaches the program through `errors.Is`, and the error names
the position of the statement.

The oracle test runs 500 random statements over three tables in SQLite
and here. Each statement must fail in both or in neither. One that
succeeds must change as many rows. At the end every table must hold the
same rows with the same values. The workload sets no key column and no
column of a unique index to an expression. There the order of the rows
decides the outcome, and SQLite visits them in another order.

The rows that an UPDATE or a DELETE changes are in memory until the
statement ends. The bound of memory counts them; see "Plans and memory".

## Queries

`Query` runs one SELECT over one table on a DB, a View or a Tx. A query
of a DB holds a view of the last commit until its rows are closed.
While it is open, no checkpoint runs (T-5).

The parts run in the order of SQL: WHERE, the select list, DISTINCT,
ORDER BY, then OFFSET and LIMIT. The rules follow SQLite:

| Part | Rule |
|---|---|
| `*` | the columns of the table without the hidden key; `rowid` names that one |
| alias of the table | with `FROM t AS x`, the columns are `x.c`; `t.c` is an error |
| ORDER BY | a number is a column of the result, a name that is an alias in the result is that column, anything else is an expression over the row of the table |
| order | NULL first in ascending order; rows with equal keys keep the order of the table |
| DISTINCT | NULL equals NULL, and 3 equals 3.0; the first row of a group stays |
| LIMIT, OFFSET | INTEGER; a negative LIMIT is no limit, a negative OFFSET skips no row |
| no FROM | one row, which WHERE can drop |

A query without ORDER BY and DISTINCT streams. It reads the next row of
the table when the program asks for it. An error in a row ends the rows
with Err, after the rows before it. A query with ORDER BY or DISTINCT
reads all rows that pass WHERE first, unless the plan gives them in the
order of ORDER BY. See "Plans and memory".

GROUP BY, HAVING, aggregates and JOIN are errors that name the step of
the roadmap that adds them.

The oracle test fills the tables with the workload of the writes and
150 more INSERTs. Then it runs 600 random queries in SQLite and here.
Where a query orders its rows totally, the rows must come in the same
order. For this, each ORDER BY ends with the key, and DISTINCT orders by
all columns. The other queries compare their rows as a multiset.

## Plans and memory

A SELECT, an UPDATE and a DELETE read one table. The plan chooses how
(L-9): every row, or a part of the key or of an index. A plan only
narrows the rows. The whole WHERE still runs on every row the scan
gives. So a plan can make a statement slow, but not wrong.

The plan looks at the terms of WHERE that AND joins. A term counts when
it compares a column with a value that reads no column: `=`, `<`, `<=`,
`>`, `>=`, BETWEEN and IN, with the column on either side. For the key
and for each index, it takes the leading columns that have `=` or IN,
then a range on the next column. The most `=` columns win, then a
range.

The key wins a tie: a scan of the key reads each row once, and an index
reads the entry and then the row. IN on several columns makes at most
1000 scans.

The values come from the run, so parameters work. A value that no row
can equal makes no scan: NULL, NaN, or 2.5 for an INTEGER column. A
REAL bound of an INTEGER column becomes the next whole number inside the
range: `id <= 4.5` reads to 4.

An INTEGER that REAL does not hold exactly bounds no REAL column. Rounded, it could leave out a row that
WHERE keeps. A value of a type that does not compare with the column is
an error of WHERE. The scan then reads the whole key or index, so the
error comes as it would without a plan.

A scan in the order of ORDER BY needs no sort, and a LIMIT stops it
early. It must be ascending, or descending over all columns of the key.
A scan backwards gives rows with equal values in the other order of the
key. The sort keeps them in the order of the key. IN with more
than one value, and DISTINCT, make a sort.

`Query.Plan` describes the plan: `SCAN t`, `SEARCH t USING INDEX i
((g = 3))`, `IN ORDER`, `SORT`. A test shows the plan and the rows read
for each form. Measured on 28.09.2026 with 100000 rows, at a load of
about 5:

| Query | Plan | Rows read | Time |
|---|---|---|---|
| `id = 54321` | primary key | 1 | 11 µs |
| the same | scan | 100000 | 40 ms |
| `g = 7` | index | 100 | 1.3 ms |
| the same | scan | 100000 | 55 ms |
| `id BETWEEN 1000 AND 1099` | primary key | 100 | 60 µs |
| `ORDER BY id DESC LIMIT 10` | primary key in order | 10 | 16 µs |
| the same | scan and sort | 100000 | 70 ms |

A statement that sorts, drops repeated rows, or changes rows holds rows
in memory. `Options.QueryMemory` bounds their bytes, 64 MiB by default
(L-10). A row counts 24 bytes for each value plus the bytes of its text
and blobs. A statement past the bound fails with ErrQueryMemory. It
returns no rows and changes nothing. A LIMIT does not bound a sort: the
sort reads all rows first.

The oracle tests run each random query and each random write twice here:
with the plan and without. The results must be the same. The query
test has two floors. At least one in twenty of its queries uses a
SEARCH, and at least one in five groups rows.

## Groups and aggregates

A query with GROUP BY, HAVING or an aggregate runs in two steps. The
first reads the rows that pass WHERE and puts each into its group. The
second computes one row per group: the values of GROUP BY, then the
result of each aggregate. The select list, HAVING and ORDER BY read
that group row.

Before they compile, the query replaces each aggregate in them, and
each expression that is in GROUP BY, with a column of the group row.
Two expressions are the same when they print the same. A column of the
table that is left over is an error. SQLite would take its value from
some row of the group, a value that no rule picks (rule 10). PostgreSQL
refuses such a query too.

The aggregates are count, sum, avg, min and max, each also with
DISTINCT. They follow SQLite, measured on 28.09.2026:

| Aggregate | Rule |
|---|---|
| `count(*)`, `count(x)` | all rows; the rows where x is not NULL |
| `sum` | INTEGER while all values are INTEGER and the sum fits; an overflow is an error, as in SQLite. With a REAL value, a REAL sum by the compensated summation of Kahan, Babuska and Neumaier, as in SQLite. |
| `avg` | always REAL |
| `min`, `max` | NULL is left out; of equal values the first stays |
| over no row | count is 0, the others NULL |

Without GROUP BY there is one group, also when no row passes WHERE.
With GROUP BY and no row there is no group. NULL is one group, and 3
and 3.0 are one group, as for DISTINCT. The groups come in the order of
their first row; only ORDER BY fixes an order.

The groups and the values that DISTINCT inside an aggregate has seen
count against the bound of memory (L-10).

The oracle test makes a third of its random queries group rows. They
have keys over any type, every aggregate with and without DISTINCT,
HAVING, WHERE and LIMIT. It compares a REAL sum by its bits, so it checks the
summation of SQLite too.

## Joins

FROM and JOIN name tables, each under its alias or its name. The joined
row has the columns of the first table, then those of the second, and
so on. Each table knows the offset of its first column in that row. So
expressions, WHERE, groups and ORDER BY work on joined rows as on the
rows of one table.

| Rule | As in SQLite |
|---|---|
| `t.x` looks in the table named t; `x` alone must be a column of exactly one table, else "ambiguous column name" | yes |
| a table name twice is an error; a join of a table with itself needs an alias | yes |
| ON reads the tables up to its own, not those after it | yes, for LEFT JOIN |
| LEFT JOIN gives a row without a match with NULL in the columns of its table | yes |
| `*` gives the columns of all tables in order, `t.*` those of one | yes |

The join is a nested loop in the order of the query. For each row of
the tables before, it scans the next table. That scan has a plan of its
own. Its bounds come from the terms of ON and WHERE whose value reads
only the tables before it: `ON i.repo = r.id` reads issue through the
index on repo, with the value of r.id of the current row. The order of
the tables is not changed; to choose it is part of v0.4 in the roadmap.

A term of WHERE can bound the scan of the right table of a LEFT JOIN.
This is safe: every form of term that bounds a scan is false or NULL
for NULL. So WHERE drops the row with NULL that the join would add
where the bounded scan finds nothing. The whole WHERE runs on each
joined row after the join, as for one table.

The oracle test makes a quarter of its random queries joins: INNER and
LEFT, three tables, a range in ON, and a table with itself. Each one
also runs without a plan, and both must agree with SQLite.

## Sessions and the driver

A Session runs SQL text as the connection of a client does. BEGIN,
COMMIT and ROLLBACK start and end a transaction of the session.
Outside one, each statement is a transaction of its own, as in SQLite.
So a text of three statements, of which the second fails, keeps the
first. BeginRead starts a read-only transaction on a snapshot, which
never waits.

The package `sqldriver` is the driver for `database/sql` (L-11). Each
connection is a Session. The pool opens several connections, but a
file of datumujo is open once per process. So the connections of all
`sql.DB` of one file share one open database, with a count of their
users; the last Close releases the file. A program that has the
database open already passes it to `NewConnector`.

| Case | Rule |
|---|---|
| a transaction that BEGIN in the text left open | ends when the connection goes back to the pool or is closed; the next user does not get it |
| `TxOptions.ReadOnly` | a read-only transaction; a write is an error |
| a level of isolation | served by a serializable transaction (T-1), the strongest |
| a named argument | an error: use `?` or `?N` |
| a context | checked before a statement starts, not while it runs |
| the type of a result column | `DatabaseTypeName` gives INTEGER, REAL and so on, or "" when only the run knows it |

One write transaction runs at a time. A second one waits, also on
another connection of the pool; readers never wait. A test runs four
writers and four readers on a pool of four connections under the race
detector.

## The command sql

`datumujo sql FILE [SQL]` runs the SQL text of its argument, or else of
its input, on a Session (L-12). For each SELECT it prints a line with
the names of the columns, then a line per row, with a tab between the
values. Three kinds of text print as a SQL literal in quotes. They are
a text with a tab or a line break, the text NULL, and a text that
starts with a quote. So each
line stays one row, and NULL stays apart from the text 'NULL'.

The first statement that fails stops the text, like the flag -bail of
sqlite3. A transaction that is still open rolls back. The command
exits 0 when all statements ran, 1 when one failed, and 2 when it
could not open the file.

The command runs a text through Session.Script. Script runs the
statements in order and hands the rows of each SELECT to a function.
An error in a row stops the script even when that function does not
ask for it.

## Counters

A counter is a number in the catalog tree, stored with the rows of the
transaction that increments it. A committed value is never given again,
also not after a crash. A value given in a transaction that rolls back is
given again, because nothing outside the transaction has seen it commit.
Counters do not change the schema version.

## Checking a file

`datumujo check FILE` reads the whole database. It exits 0 when the file
is intact, 1 on a finding, and 2 when it could not check (I-2). It does
not create a missing file, and it writes nothing: opening the database
cuts no log, only the next commit does.

The check runs two passes.

1. It reads every page and reports each whose checksum fails. This pass
   depends on no structure, so it finds a damaged page that no tree
   reaches.
2. It walks the structures: the free list, the catalog, and each table
   and index with the tree check. It reads every row, and every entry of
   every index against its row. An index must have one entry for each
   row.

If every structure was walked to its end, each page but the header must
belong to exactly one of them. A page used twice, or used by nothing, is
a finding. A root slot that no structure uses is a finding too.

The report also gives statistics (O-3): the size of the file and the log,
and the number of pages and free pages. For each table and index, it
gives the rows or entries and the pages.

### Damage in the log

Damage in a frame with a later commit behind it makes the log refuse to
open. The check reports that as a finding, and the database does not
open until the log is restored.

Bytes after the last complete commit are a commit that did not finish, or
damage in the last commit. They are a finding too. After a crash that is
a false alarm; after damage it is the only warning.

Measured on 26.09.2026 with 1000 rows in two commits: one flipped bit in
the middle of the log hid both commits. The first version of the check
said intact. With step 10a, the same flip in the first commit
refuses the log and names the frame.

## Backup and restore

A backup is one snapshot written to a new file (O-1). The program goes on
writing meanwhile. The snapshot stops checkpoints until the copy is done,
as any long reader does (T-5). The copy holds every page as of one commit
and has no log.

The copy goes to TARGET-new and is synced. Then the check command reads
it. Only a copy that the check accepts is renamed to TARGET. So a file
under the target name is always whole and sound, also after a power loss
in the middle (O-2).

A backup never replaces a file. If TARGET or TARGET-log exists, it fails.
A log next to a copy would be applied to it when it opens. Files of a
copy that did not finish are removed first.

A restore is a backup of a database that no program has open. It opens
the source read-only. The command is `datumujo backup FILE COPY` or
`datumujo restore COPY FILE`. A program that has the database open calls
backup.Backup.

### Read-only open

The check, the backup and the restore read a database without a change
to it. Read-only open creates no file and starts no log. A missing log
stands as an empty one in memory. Begin and Checkpoint fail. It still
takes the lock file, so no program writes to the database meanwhile.

### A snapshot takes the header of its commit

The store keeps the number of the last commit next to its header, and
changes both at once when a commit is done. The log counts a commit when
its frames are synced, before that. A snapshot that took the number from
the log read pages of one commit with the header of the one before. The
first backup test found this.

## Memory of a transaction

A transaction holds the pages it changes until it commits. MaxTxBytes
bounds them (D-3), 16 MiB by default at any page size. The page that
would pass the bound fails with ErrTxTooLarge, and the caller rolls the
transaction back. A page already changed costs nothing more. A commit
builds all frames in one buffer, so it adds one more copy of the pages
and the frame headers. The peak of a transaction is about twice its
bound.

Measured on the file system of the operating system, with a bound of
4 MiB and pages of 4096 bytes. A full transaction held 4.24 MB on the
heap, and its commit allocated 4.60 MB. Before the commit built the
frames in place, it allocated 9.59 MB: each frame once alone, and once
in the buffer.

Outside a transaction, the log keeps a small entry for each page image
until the next checkpoint. A commit starts a checkpoint when the log has
reached a size; see "The public API".

## Load test

The load test of P-5 (`internal/load`, `TestLoad`) loads the issues,
pull requests, comments and review comments of in-toto/in-toto-golang
and in-toto/in-toto. It uses three tables and three indexes on the
issues: by state, by author, and by time of change. Each issue goes in
with its comments in one commit, as a forge writes them, and a checkpoint
follows every 100 commits.

Environment: Go 1.26.5, darwin/arm64, Apple M1 Pro, macOS 27.0, APFS,
pages of 4096 bytes, data fetched on 26.09.2026. Other programs ran on
the machine, with a load average between 23 and 65. The times are the
median of five runs, with the lowest and highest in brackets.

| measure | value |
|---|---|
| rows | 1425 issues and pull requests, 3223 comments, 3270 review comments |
| load | 1427 commits in 6.72 s (6.46 to 8.70), about 4.7 ms per commit |
| largest transaction | 401 pages, 1.64 MB, of 16 MiB allowed |
| memory | 590 KB allocated per commit; at most 12.3 KB more held after the load |
| file | 20.7 MB, 5059 pages |
| get every issue by key | 25.5 ms (21.8 to 32.3) for 1425 |
| open issues, 30 per page with the cursor | 1.25 ms (1.23 to 2.41) for 5 pages |
| comments of each issue by prefix | 55.9 ms (49.8 to 72.8) for 1425 issues |
| 30 issues changed last, per repository | 1.15 ms (0.86 to 1.82) for 2 |
| check | 153 ms (105 to 316), intact |
| backup while the database is open | 319 ms (207 to 405), accepted by the check |

The counts, the largest transaction and the file size were the same in
all five runs. The test compares each count with the JSON data: rows per
table, open issues through the paged index, and comments through the
prefix scans. It also compares the rows the check counts with the scans.
16 comments belong to in-toto #380, which the API no longer returns.
They are in the table, and no prefix scan of an issue finds them.

A commit costs about 4.7 ms; where the time goes was not measured here.
The reads a forge makes per request stay below a millisecond. For this load,
the engine needs no page cache.

## The public API

The package at the root of the module is what a program uses. Everything
under `internal/` stays free to change. The package has Open with
Options, a DB with Begin, Update, View, Read, Checkpoint and Backup, and
the functions Check and Restore. Tx and View carry the methods of the
table layer: tables, indexes, rows, scans and counters. The types of
tables, rows and scans are aliases of the engine's types, so a program
can name them.

A commit that leaves the log at Options.CheckpointBytes or larger starts
a checkpoint, 4 MiB by default. While a view of an earlier commit is
open, the checkpoint does nothing, and the next commit tries again. A
negative size turns this off, and the program calls Checkpoint itself.
If the checkpoint fails, Commit returns an error that matches
ErrCheckpoint. The commit is durable then; only the log did not shrink.

The load test runs through this API. Three runs on 26.09.2026, with a
load average of about 5, gave the same counts as below. The log stayed
under 4 MiB after each commit, and the checkpoints ran by themselves.

## One process, one writer

The database runs in one process (S-1). Inside the process, one write
transaction at a time holds the writer lock, and a checkpoint takes it too.
Readers take no lock.

Open takes an exclusive lock on a file with the suffix "-lock" before it
reads anything. A second Open of the same database fails until Close, in
another process and in the same one. On the operating system the lock is
flock(2), and the kernel releases it when the process ends. The lock is on
a file of its own, because creating a database renames a new file over the
old name.

## All file access goes through one interface

The engine reads and writes files through an internal interface with the
calls it needs: read at, write at, sync, truncate, size, close. There are two
implementations: the operating system, and a simulated disk for tests. The
simulated disk keeps the writes since the last sync apart. It can drop them
or apply part of them, as a power loss would. The crash test (P-1)
runs a workload, stops it after each write call, simulates the power loss,
and opens the database again.
