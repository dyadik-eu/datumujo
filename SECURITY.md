# Security

## What to report

datumujo stores data. So a report is welcome for each of these, also when
nobody attacks anything:

- A read returns wrong data and no error (I-1).
- A crash or a power loss leaves the database in a state other than the
  last completed commit (T-4).
- A damaged or crafted file makes the engine panic, hang or use memory
  without bound (D-4).
- The check command reports an intact file for a damaged one (I-2).

## How to report

Use [private vulnerability reporting](https://github.com/dyadik-eu/datumujo/security/advisories/new)
on GitHub. Do not open a public issue. Include the version or commit, the
Go version, the operating system, and a program or a file that shows the
problem.

The answer comes in the advisory. The fix is published with the advisory
when it is released.

## Supported versions

Before v1, only the latest release gets fixes.
