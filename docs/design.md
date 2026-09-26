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

## Pages carry their own checksum

Every page ends with a trailer: a CRC-32C over the page and its page number.
A page that was written to the wrong place, or read from the wrong place,
fails the check just like a damaged page. A failed check is an error that
names the page (I-1). The read returns no data.

Page 0 is the header: a magic value, the format version, the page size and
the page count. A file with an unknown format version is rejected (I-4).

The page size is a field of the header. The value for new files is decided
after a measurement with the load of P-5 (Q-2). Until then the tests run with
more than one size.

## One process, one writer

The database runs in one process (S-1). Open takes an exclusive lock on the
database file, so a second process cannot open it at the same time. Inside
the process, one write transaction at a time holds the writer lock. Readers
take no lock.

## All file access goes through one interface

The engine reads and writes files through an internal interface with the
calls it needs: read at, write at, sync, truncate, size, close. There are two
implementations: the operating system, and a simulated disk for tests. The
simulated disk keeps the writes since the last sync apart. It can drop them
or apply part of them, as a power loss would. The crash test (P-1)
runs a workload, stops it after each write call, simulates the power loss,
and opens the database again.
