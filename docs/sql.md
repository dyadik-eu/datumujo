# The SQL of datumujo

This is the SQL of stage 2 and of the parts of stage 3 that are done.
It says what the SQL has, how its values behave, and its limits. The requirements are
L-1 to L-12, L-13 for DEFAULT, L-14 for CHECK, L-15 for UNIQUE, L-16 for subqueries, L-17 for ON CONFLICT and L-18 for EXPLAIN, in [requirements.md](requirements.md). Each difference to SQLite is in the
oracle table there. How the parts work is in [design.md](design.md).

## Where SQL runs

| Way | For |
|---|---|
| `DB.Exec`, `DB.Query` | one call, one transaction |
| `Tx.Exec`, `Tx.Query`, `View.Query` | inside a transaction or a snapshot of the Go API |
| `DB.Session` | SQL text with BEGIN, COMMIT and ROLLBACK, as a client connection |
| `Session.Script` | a text of several statements, SELECT and EXPLAIN included |
| the package `sqldriver` | `database/sql`, as the driver "datumujo" |
| `datumujo sql FILE [SQL]` | the shell |

An error is a `*SQLError` with the line and the column in the text.
`errors.Is` finds the error of the engine behind it, such as ErrUnique.

## Statements

```sql
CREATE TABLE [IF NOT EXISTS] t (c TYPE [NOT NULL] [PRIMARY KEY] [UNIQUE] [DEFAULT d] [CHECK (e)], ...,
  [PRIMARY KEY (c, ...)], [UNIQUE (c, ...)], ..., [CHECK (e)], ...)
CREATE [UNIQUE] INDEX [IF NOT EXISTS] i ON t (c, ...)
DROP TABLE [IF EXISTS] t
DROP INDEX [IF EXISTS] i
ALTER TABLE t ADD [COLUMN] c TYPE [NOT NULL] [DEFAULT d] [CHECK (e)]
INSERT INTO t [(c, ...)] VALUES (e, ...), ...
  [ON CONFLICT [(c, ...)] DO NOTHING | DO UPDATE SET c = e, ... [WHERE e]]
UPDATE t SET c = e, ... [WHERE e]
DELETE FROM t [WHERE e]
SELECT [DISTINCT] e [[AS] a], ... | * | t.*
  [FROM t [[AS] a] [[INNER] JOIN | LEFT [OUTER] JOIN t [[AS] a] ON e] ...]
  [WHERE e] [GROUP BY e, ...] [HAVING e]
  [ORDER BY e [ASC | DESC], ...] [LIMIT e [OFFSET e]]
BEGIN [TRANSACTION]
COMMIT [TRANSACTION]
ROLLBACK [TRANSACTION]
EXPLAIN [QUERY PLAN] SELECT ... | INSERT ... | UPDATE ... | DELETE ...
```

A statement is atomic. When it fails, it changes nothing, and the
transaction goes on. Outside a transaction of a Session, each statement
commits on its own.

A table without a PRIMARY KEY has a hidden INTEGER key `rowid`. `*`
does not show it, but `rowid` names it. An INSERT without a value for
it, or for an INTEGER key of one column, takes the largest key plus
one. A column of the key is NOT NULL.

## Defaults

`d` is a constant: a number with an optional sign, a string, a blob,
TRUE, FALSE or NULL. A TIMESTAMP column can take `CURRENT_TIMESTAMP`
too. An INSERT that does not name a column gives it its default, and
NULL without one. A column it names keeps the value, NULL included.

```sql
CREATE TABLE issue (
  id INTEGER PRIMARY KEY,
  state TEXT NOT NULL DEFAULT 'open',
  opened TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
)
```

A default must fit its column, and the table is not made if it does
not. The INTEGER key of one column takes the next key and cannot have a
default. A NOT NULL column cannot have DEFAULT NULL. CURRENT_TIMESTAMP is
the time of the statement in UTC, the same for each of its rows.

A column that ALTER TABLE adds gives the rows that exist its default,
and NULL without one. With a default it can be NOT NULL; without one it
must allow NULL. Its default is a constant, not CURRENT_TIMESTAMP. The
rows are not rewritten.

The first default makes the file format version 2, which v0.2.0 does
not open.

## Checks

A CHECK on a column or on the table is a BOOLEAN of the columns of the
table. A check of a column can read the other columns too. An INSERT or
UPDATE fails when a check is FALSE for a row it writes, and it then
changes no row. NULL passes. The error matches `ErrCheck`.

```sql
CREATE TABLE issue (
  id INTEGER PRIMARY KEY,
  votes INTEGER NOT NULL DEFAULT 0 CHECK (votes >= 0),
  opened TIMESTAMP,
  closed TIMESTAMP,
  CHECK (closed >= opened)
)
```

With this table, `UPDATE issue SET votes = -1` fails:
`CHECK ((votes >= 0)) of table issue is false`. A row with `opened` or
`closed` NULL passes the check of the table.

A check must be BOOLEAN, and it takes no parameter and no aggregate.
Otherwise the table is not made. An error inside a check, such as a
division by zero, is an error of the statement.

ALTER TABLE ADD COLUMN with a CHECK tests the rows that exist, with the
default of the new column. A FALSE fails the statement, and the column
is not added. A check can have no name: `CONSTRAINT name` is not in the
SQL.

The first check makes the file format version 2, as a default does.

## Unique

UNIQUE on a column, or UNIQUE (c, ...) on the table, allows no two rows
with the same values in those columns. A row with NULL in one of them
never conflicts. The INSERT or UPDATE that breaks the rule fails with
`ErrUnique` and changes no row.

```sql
CREATE TABLE member (
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  team INTEGER,
  seat INTEGER,
  UNIQUE (team, seat)
)
```

With this table, a second row with team 1 and seat 1 fails. Two rows
with team 1 and seat NULL do not.

Each UNIQUE is a unique index of the table, called `<table>_unique_<n>`
with the first n that no index and no table has. The name shows in
`datumujo check`, and DROP INDEX drops the index and its rule. A UNIQUE
over the columns of the primary key, or over the columns of an earlier
UNIQUE in any order, makes no second index.

A UNIQUE names columns of the statement, each once. ALTER TABLE ADD
COLUMN takes no UNIQUE; add the column, then CREATE UNIQUE INDEX on it.
UNIQUE keeps the file at format version 1, which v0.2.0 reads.

## Subqueries

A SELECT in parentheses can stand in an expression, in three forms:

| Form | Result |
|---|---|
| `(SELECT ...)` | the one column of its row; NULL without a row |
| `EXISTS (SELECT ...)` | TRUE when the query has a row |
| `x [NOT] IN (SELECT ...)` | as IN with a list of the values of its one column |

```sql
SELECT name FROM member
WHERE team IN (SELECT id FROM team WHERE active)
  AND seat > (SELECT min(seat) FROM member)
```

They work in SELECT, INSERT, UPDATE and DELETE. In a query, they work
in WHERE, in its columns, in ORDER BY, LIMIT and HAVING, and in another
subquery. A subquery as a value with more than one row is an error;
SQLite takes the first row. IN with an empty subquery is FALSE, even for
NULL. Otherwise NULL in the subquery or as x gives NULL, unless a value
equals x.

A subquery can read the columns of the queries around it, as `t.id`
here:

```sql
SELECT id, (SELECT max(v) FROM u WHERE u.k = t.id) FROM t
```

A name of a table of the subquery is never one of a query around. Of
the queries around, the nearest that knows a name has it. In a query
with GROUP BY, a subquery reads the keys of GROUP BY: `t.a` for
`GROUP BY a` too. A key that is an expression, as `a + 1`, matches by
its text only.

A subquery runs at its first use, and keeps its answer while the values
it reads from the queries around stay the same. One that reads none
runs once for each run of its statement. One that no row needs does not
run. An index can narrow the scan of a subquery. Above, an index on
`u.k` reads only the rows of each `t.id`.

A subquery sees the tables as they were before the statement. An
INSERT of three rows with `(SELECT count(*) FROM t)` gives each row
the same count. An UPDATE computes all new rows before it writes the
first: a subquery that reads the table of the UPDATE sees no new row.
SQLite lets each row see the rows written before it.

Some subqueries are errors:

- one in a CHECK
- a column of a query around in LIMIT or OFFSET, as in SQLite
- an aggregate that reads only columns of a query around, as
  `(SELECT sum(t.a))`; SQLite makes it an aggregate of that query

## On conflict

INSERT ... ON CONFLICT handles a row that has the key or the unique
values of a row that is there. DO NOTHING skips the new row. DO UPDATE
changes the row that is there.

```sql
CREATE TABLE stock (sku TEXT PRIMARY KEY, n INTEGER NOT NULL)
INSERT INTO stock VALUES ('a', 3), ('a', 2)
  ON CONFLICT (sku) DO UPDATE SET n = n + excluded.n
```

This gives one row, `'a'` with 5. In DO UPDATE, a column by its name or
as `stock.n` is the row that is there. `excluded.n` is the row that the
INSERT would have written, with the defaults of the columns it does not
name. A WHERE after SET keeps the row when it is not TRUE.

The target names the columns of the primary key or of one UNIQUE, in
any order. A conflict on another key then fails with `ErrUnique`.
Without a target, every key counts: DO UPDATE takes the row of the
primary key first, then the UNIQUE in their order. A NULL in a UNIQUE
conflicts with no row.

The rows of one INSERT go in turn: a later row can conflict with one
that an earlier row wrote or changed. A new row first passes NOT NULL
and its checks, also when DO NOTHING would skip it. The row of DO
UPDATE passes them too, and a new key moves it. The count of changed
rows holds the rows written and the rows that DO UPDATE changed.

DO UPDATE takes no subquery: it would run while the INSERT writes, and
see the rows written before.

## Explain

EXPLAIN before a SELECT, INSERT, UPDATE or DELETE gives the plan of the
statement and does not run it. It reads no row and writes none. Run it
with Query, as a SELECT; Exec refuses it. `EXPLAIN QUERY PLAN` gives the
same rows.

The plan has one TEXT column, `plan`. The first row is the statement.
Each subquery has a row after it with its number, its text and its
plan. CORRELATED marks a subquery that reads a column of a query around
it. A subquery
inside subquery 1 is 1.1.

```sql
CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER)
CREATE TABLE u (k INTEGER, v INTEGER)
CREATE INDEX u_k ON u (k)
EXPLAIN SELECT id, (SELECT max(v) FROM u WHERE u.k = t.id) FROM t WHERE id = 2
```

| plan |
|---|
| `SEARCH t USING PRIMARY KEY ((id = 2))` |
| `SUBQUERY 1 (SELECT max(v) FROM u WHERE (u.k = t.id)), CORRELATED: SEARCH u USING INDEX u_k ((u.k = t.id)); GROUP` |

The row of an INSERT gives the number of rows, and ON CONFLICT the keys
that it looks up: `INSERT INTO t: 2 ROWS`, then `ON CONFLICT: LOOK UP
PRIMARY KEY; DO UPDATE`. UPDATE and DELETE give how they find their
rows: `UPDATE u: SEARCH u USING INDEX u_k ((k = 1))`.

EXPLAIN compiles the statement as a run would. A statement that fails
to compile fails under EXPLAIN too, at the same place. In a transaction,
the plan sees the tables and indexes of the transaction.

## Types

| Type | Names | Go value |
|---|---|---|
| INTEGER | INTEGER, INT, BIGINT | int64 |
| REAL | REAL, DOUBLE, DOUBLE PRECISION, FLOAT | float64 |
| BOOLEAN | BOOLEAN, BOOL | bool |
| TEXT | TEXT, VARCHAR | string, UTF-8 |
| BLOB | BLOB, BYTEA | []byte |
| TIMESTAMP | TIMESTAMP | time.Time, in UTC |

A type takes no length. Types are strict. A column holds values of its
type only, and an operator takes the types it names.

There are two exceptions. An INTEGER and a REAL mix in arithmetic and in comparisons.
An INTEGER goes into a REAL column when a REAL holds it exactly. A type
that does not fit is an error before the statement runs. For a
parameter, it is an error when the statement runs.

## Values and operators

The operators bind as in SQLite, from the lowest:

| Level | Operators |
|---|---|
| 1 | OR |
| 2 | AND |
| 3 | NOT |
| 4 | `=` `==` `!=` `<>` IS, IS NOT, IN, NOT IN, BETWEEN, NOT BETWEEN, LIKE, NOT LIKE |
| 5 | `<` `<=` `>` `>=` |
| 6 | `+` `-` |
| 7 | `*` `/` `%` |
| 8 | `\|\|` |
| 9 | unary `-` and `+` |

NULL follows SQL. A comparison with NULL is NULL, and WHERE keeps a row
only when the condition is TRUE. AND, OR and NOT use three values. IS
and IS NOT compare NULL as a value.

INTEGER with INTEGER gives INTEGER; `/` cuts towards zero. An overflow
and a division by zero are errors. `||` joins two TEXT values. LIKE has
`%` for any run and `_` for one character; A to Z match their lower
case. A comparison of two TEXT values compares their bytes.

Parameters are `?` and `?N`, N from 1 to 32766. A bare `?` is one more
than the largest number before it.

## Functions

| Function | Result |
|---|---|
| `abs(x)` | the absolute value |
| `length(x)` | the characters of a TEXT, the bytes of a BLOB |
| `lower(x)`, `upper(x)` | A to Z in the other case |
| `trim(x[, c])`, `ltrim`, `rtrim` | x without the characters of c, a space by default |
| `replace(x, a, b)` | x with each a as b |
| `instr(x, a)` | the place of the first a in x, from 1, or 0 |
| `substr(x, start[, n])` | a part of a TEXT or a BLOB, as in SQLite |
| `coalesce(a, b, ...)`, `ifnull(a, b)` | the first value that is not NULL |
| `nullif(a, b)` | NULL when a equals b, else a |
| `min(a, b, ...)`, `max(a, b, ...)` | the smallest or largest value, NULL if one is NULL |
| `round(x[, n])` | x rounded to n places, half away from zero, as REAL |
| `CAST(x AS type)` | x as another type, when it converts exactly |

The aggregates are `count(*)`, `count(x)`, `sum(x)`, `avg(x)`, `min(x)`
and `max(x)`, each also with DISTINCT. `sum` stays INTEGER while it
fits; `avg` is REAL. Over no row, `count` is 0 and the others are NULL.

## Queries

A query without FROM has one row. ORDER BY takes a number of a column,
an alias of the result, or an expression. NULL sorts first. Rows that
ORDER BY does not order come in no fixed order.

With GROUP BY, HAVING or an aggregate, each column of the result must be
in GROUP BY or in an aggregate. NULL is one group, as for DISTINCT.

In a join, a column name without a table must be a column of exactly
one table. A table that comes twice needs an alias. ON reads only the
tables up to its own. LEFT JOIN gives a row without a match with NULL in
the columns of its table.

## Limits

Each limit has a test that the last value that fits passes and the
first that does not fails. The tests are `TestLimits` and
`TestLimitsOfStage3` in `internal/sqlexec`, and `TestDepth` and
`TestErrors` in `internal/sqlparse`.

| Limit | Value |
|---|---|
| a name of a table, column or index | 255 bytes of UTF-8 |
| columns of a table | 1024, the hidden key of a table without PRIMARY KEY included |
| indexes of a table | 64, each UNIQUE included |
| the key form of a primary key | 1000 bytes at a page size of 4096; a TEXT key of 998 bytes |
| an index entry | 1000 bytes: the index columns, then the key |
| the name of the index of a UNIQUE | 255 bytes, so a table name of at most 246 bytes for `_unique_1` |
| a value outside a key | no limit of its own; the changes of a transaction are at most `Options.MaxTxBytes`, 16 MiB by default |
| the rows a statement holds to sort, group or change, and the values of IN (SELECT ...), also in a correlated subquery | `Options.QueryMemory`, 64 MiB by default |
| nesting of expressions | 200 levels; each subquery is a level, so 199 subqueries fit inside each other |
| the text of all DEFAULT and CHECK of a file | no limit of its own; see below |
| parameters | 32766 |

The schema is one value. It holds the text of each DEFAULT and each
CHECK, and each change of the schema writes it again in its
transaction. So `Options.MaxTxBytes` bounds it. Measured with a bound
of 1 MiB: a table with a DEFAULT of 1037305 bytes can be made. But from
518613 bytes on, the next change of the schema fails, also a CREATE
TABLE without a default. Keep the texts of all DEFAULT and CHECK
together below half of the bound.

The key form of a TEXT or a BLOB is its bytes plus 2 bytes of end
mark. Each byte 0 in it takes 2 bytes. An INTEGER and a REAL take 8
bytes, a TIMESTAMP 12, a BOOLEAN 1. A column that allows NULL takes one
byte more in an index.

## Not in stage 2

- common table expressions, views, triggers, foreign keys
- window functions, UNION, NATURAL and USING joins
- a DEFAULT that is an expression, `INSERT ... DEFAULT VALUES`
- `CONSTRAINT` with a name

The roadmap to v1.0 in [roadmap.md](roadmap.md) says which of
them come when.
