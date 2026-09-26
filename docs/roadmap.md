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
| 6 | B+tree over pages: byte keys and values, get, put, delete, scans in both directions, free pages; tested against a map as oracle; crash test with the tree | M-4 (bytes), P-1 (tree) |
| 7 | Page size: measure with the load of P-5 and set the value for new files | Q-2 |
| 8 | Typed tables: schema in the file, column types, null, row encoding, primary key, schema changes with a version | M-1, M-7 |
| 9 | Indexes and scans: secondary and unique indexes, range and prefix scans, cursors, counters | M-2, M-3, M-4, M-5, M-6 |
| 10 | Check command, damage test, statistics | I-2, P-3, O-3 |
| 11 | Backup while writes go on, restore | O-1, O-2 |
| 12 | Load test with the metadata of the in-toto repositories; memory bound per transaction | P-5, D-3 |

After step 12, abelejo uses datumujo for the metadata of its forge (phase 3
of its plan).

Not in stage 1: a query language, full-text search, replication (S-4). The
log of step 4 keeps replication possible (D-5).
