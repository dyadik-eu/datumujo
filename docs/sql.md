# The SQL of datumujo

This is the SQL of stage 2: what it has, how its values behave, and its
limits. The requirements are L-1 to L-12 in
[requirements.md](requirements.md). Each difference to SQLite is in the
oracle table there. How the parts work is in [design.md](design.md).

## Where SQL runs

| Way | For |
|---|---|
| `DB.Exec`, `DB.Query` | one call, one transaction |
| `Tx.Exec`, `Tx.Query`, `View.Query` | inside a transaction or a snapshot of the Go API |
| `DB.Session` | SQL text with BEGIN, COMMIT and ROLLBACK, as a client connection |
| `Session.Script` | a text of several statements, SELECT included |
| the package `sqldriver` | `database/sql`, as the driver "datumujo" |
| `datumujo sql FILE [SQL]` | the shell |

An error is a `*SQLError` with the line and the column in the text.
`errors.Is` finds the error of the engine behind it, such as ErrUnique.

## Statements

```sql
CREATE TABLE [IF NOT EXISTS] t (c TYPE [NOT NULL] [PRIMARY KEY], ..., [PRIMARY KEY (c, ...)])
CREATE [UNIQUE] INDEX [IF NOT EXISTS] i ON t (c, ...)
DROP TABLE [IF EXISTS] t
DROP INDEX [IF EXISTS] i
ALTER TABLE t ADD [COLUMN] c TYPE
INSERT INTO t [(c, ...)] VALUES (e, ...), ...
UPDATE t SET c = e, ... [WHERE e]
DELETE FROM t [WHERE e]
SELECT [DISTINCT] e [[AS] a], ... | * | t.*
  [FROM t [[AS] a] [[INNER] JOIN | LEFT [OUTER] JOIN t [[AS] a] ON e] ...]
  [WHERE e] [GROUP BY e, ...] [HAVING e]
  [ORDER BY e [ASC | DESC], ...] [LIMIT e [OFFSET e]]
BEGIN [TRANSACTION]
COMMIT [TRANSACTION]
ROLLBACK [TRANSACTION]
```

A statement is atomic. When it fails, it changes nothing, and the
transaction goes on. Outside a transaction of a Session, each statement
commits on its own.

A table without a PRIMARY KEY has a hidden INTEGER key `rowid`. `*`
does not show it, but `rowid` names it. An INSERT without a value for
it, or for an INTEGER key of one column, takes the largest key plus
one. A column of the key is NOT NULL.

An added column allows NULL: the rows that exist have no value for it.

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
first that does not fails. The tests are `TestLimits` in
`internal/sqlexec`, and `TestDepth` and `TestErrors` in
`internal/sqlparse`.

| Limit | Value |
|---|---|
| a name of a table, column or index | 255 bytes of UTF-8 |
| columns of a table | 1024, the hidden key of a table without PRIMARY KEY included |
| indexes of a table | 64 |
| the key form of a primary key | 1000 bytes at a page size of 4096; a TEXT key of 998 bytes |
| an index entry | 1000 bytes: the index columns, then the key |
| a value outside a key | no limit of its own; the changes of a transaction are at most `Options.MaxTxBytes`, 16 MiB by default |
| the rows a statement holds to sort, group or change | `Options.QueryMemory`, 64 MiB by default |
| nesting of expressions | 200 levels |
| parameters | 32766 |

The key form of a TEXT or a BLOB is its bytes plus 2 bytes of end
mark. Each byte 0 in it takes 2 bytes. An INTEGER and a REAL take 8
bytes, a TIMESTAMP 12, a BOOLEAN 1. A column that allows NULL takes one
byte more in an index.

## Not in stage 2

Subqueries, common table expressions, views, triggers, foreign keys,
CHECK, DEFAULT, window functions, UNION, NATURAL and USING joins, and
upsert. The roadmap to v1.0 in [roadmap.md](roadmap.md) says which of
them come when.
