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

Recovery cannot tell a commit that did not finish from damage in the middle
of the log: in both cases the chain breaks there. Open therefore reports how
many bytes it left behind, and the check command (I-2) shows them. The next
commit cuts these bytes off first. Otherwise a short new commit could leave
old frames behind it. Each of them could continue the chain by a chance of
2^-32.

Each generation of the log has a random salt in its header, and the chain
starts from it. Frames of an earlier generation that are still in the file
do not chain.

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
