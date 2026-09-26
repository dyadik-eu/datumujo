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
file with an unknown format version is rejected (I-4). Every page number in
the header must point into the file.

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
