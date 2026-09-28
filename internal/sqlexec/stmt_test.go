package sqlexec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

const pageSize = 4096

// session is a write transaction for the tests.
type session struct {
	t   testing.TB
	s   *store.Store
	stx *store.Tx
	tx  *table.Tx
}

func newSession(t testing.TB, opt store.Options) *session {
	t.Helper()
	opt.PageSize = pageSize
	s, err := store.Open(vfs.NewSim(), "db", opt)
	if err != nil {
		t.Fatal(err)
	}
	ss := &session{t: t, s: s}
	ss.begin()
	t.Cleanup(func() { ss.stx.Rollback(); s.Close() })
	return ss
}

func (ss *session) begin() {
	var err error
	if ss.stx, err = ss.s.Begin(); err != nil {
		ss.t.Fatal(err)
	}
	if ss.tx, err = table.Begin(ss.stx, pageSize); err != nil {
		ss.t.Fatal(err)
	}
}

func (ss *session) commit() {
	if err := ss.stx.Commit(); err != nil {
		ss.t.Fatal(err)
	}
	ss.begin()
}

// exec runs one statement.
func (ss *session) exec(src string, params ...any) (Result, error) {
	st, err := sqlparse.Parse(src)
	if err != nil {
		return Result{}, err
	}
	return Exec(ss.tx, st, params)
}

func (ss *session) must(src string, params ...any) Result {
	ss.t.Helper()
	r, err := ss.exec(src, params...)
	if err != nil {
		ss.t.Fatalf("%s: %v", src, err)
	}
	return r
}

// dump prints the rows of a table in key order, hidden key included.
func (ss *session) dump(name string) string {
	ss.t.Helper()
	rows, err := ss.tx.Scan(name, table.Options{})
	if err != nil {
		ss.t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		for i, v := range rows.Row() {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(sqlLiteral(v))
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		ss.t.Fatal(err)
	}
	return b.String()
}

func TestInsertAndKeys(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE note (body TEXT, n INTEGER)")
	ss.must("CREATE TABLE issue (id INTEGER PRIMARY KEY, title TEXT NOT NULL, score REAL)")
	for _, c := range []struct {
		src    string
		params []any
		want   Result
	}{
		{"INSERT INTO note VALUES ('a', 1), ('b', NULL)", nil, Result{2, 2}},
		{"INSERT INTO note (n) VALUES (?)", []any{int64(3)}, Result{1, 3}},
		{"INSERT INTO note (rowid, body) VALUES (-5, 'c')", nil, Result{1, -5}},
		{"INSERT INTO issue (title) VALUES ('one')", nil, Result{1, 1}},
		{"INSERT INTO issue VALUES (10, 'ten', 2), (NULL, 'eleven', NULL)", nil, Result{2, 11}},
		{"INSERT INTO issue (id, title, score) VALUES (?1, ?2, ?3)", []any{int64(-3), "neg", 1.5}, Result{1, -3}},
	} {
		if r := ss.must(c.src, c.params...); r != c.want {
			t.Errorf("%s: %+v, want %+v", c.src, r, c.want)
		}
	}
	// The largest key is 3, so the next one is 4, although -5 is smaller.
	if r := ss.must("INSERT INTO note (body) VALUES ('d')"); r.LastInsertID != 4 {
		t.Errorf("next hidden key: %d", r.LastInsertID)
	}
	want := "-5 'c' NULL\n1 'a' 1\n2 'b' NULL\n3 NULL 3\n4 'd' NULL\n"
	if got := ss.dump("note"); got != want {
		t.Errorf("note:\n%swant\n%s", got, want)
	}
	want = "-3 'neg' 1.5\n1 'one' NULL\n10 'ten' 2.0\n11 'eleven' NULL\n"
	if got := ss.dump("issue"); got != want {
		t.Errorf("issue:\n%swant\n%s", got, want)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (k TEXT, n INTEGER, a INTEGER, b INTEGER, PRIMARY KEY (k, n))")
	ss.must("CREATE INDEX by_a ON t (a)")
	ss.must("INSERT INTO t VALUES ('x', 1, 10, 20), ('x', 2, 30, 40), ('y', 1, NULL, 0)")
	// SET reads the old row: a and b swap.
	if r := ss.must("UPDATE t SET a = b, b = a WHERE k = 'x'"); r.RowsAffected != 2 {
		t.Errorf("update: %+v", r)
	}
	if got, want := ss.dump("t"), "'x' 1 20 10\n'x' 2 40 30\n'y' 1 NULL 0\n"; got != want {
		t.Errorf("after the swap:\n%swant\n%s", got, want)
	}
	// WHERE keeps a row only when it is TRUE, not when it is NULL.
	if r := ss.must("UPDATE t SET b = 99 WHERE a > 0"); r.RowsAffected != 2 {
		t.Errorf("update with NULL in WHERE: %+v", r)
	}
	// A new key moves the row.
	ss.must("UPDATE t SET k = 'z', n = n + 10 WHERE k = 'y'")
	want := "'x' 1 20 99\n'x' 2 40 99\n'z' 11 NULL 0\n"
	if got := ss.dump("t"); got != want {
		t.Errorf("t:\n%swant\n%s", got, want)
	}
	// The index follows: a scan through it finds the rows by a.
	rows, err := ss.tx.Scan("t", table.Options{Index: "by_a", Prefix: []any{int64(40)}})
	if err != nil || !rows.Next() || rows.Row()[1] != int64(2) {
		t.Errorf("index by_a after update: %v", err)
	}
	if r := ss.must("DELETE FROM t WHERE a IS NULL OR n = 2"); r.RowsAffected != 2 {
		t.Errorf("delete: %+v", r)
	}
	if r := ss.must("DELETE FROM t"); r.RowsAffected != 1 {
		t.Errorf("delete all: %+v", r)
	}
	if got := ss.dump("t"); got != "" {
		t.Errorf("after delete: %q", got)
	}
}

// TestStatementIsAtomic: a statement that fails changes nothing, and the
// transaction goes on (L-7).
func TestStatementIsAtomic(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, u TEXT)")
	ss.must("CREATE UNIQUE INDEX by_u ON t (u)")
	ss.must("INSERT INTO t VALUES (1, 'a'), (2, 'b')")
	before := ss.dump("t")
	for _, c := range []struct {
		src   string
		cause error
	}{
		{"INSERT INTO t VALUES (3, 'c'), (4, 'a')", table.ErrUnique},
		{"INSERT INTO t VALUES (5, 'e'), (1, 'f')", table.ErrExists},
		{"UPDATE t SET id = id + 1", table.ErrExists},
		{"UPDATE t SET u = 'b'", table.ErrUnique},
		{"INSERT INTO t VALUES (6, 'g'), (7, 1 / 0)", nil},
		{"CREATE TABLE t2 (x INTEGER); INSERT", nil},
	} {
		_, err := ss.exec(c.src)
		if err == nil {
			t.Fatalf("%s: no error", c.src)
		}
		if c.cause != nil && !errors.Is(err, c.cause) {
			t.Errorf("%s: %v, want %v", c.src, err, c.cause)
		}
		if got := ss.dump("t"); got != before {
			t.Errorf("%s changed the table:\n%s", c.src, got)
		}
	}
	// A CREATE that fails later in the same statement leaves no table:
	// here the table exists, the unique index on its data fails.
	ss.must("CREATE TABLE d (x INTEGER)")
	ss.must("INSERT INTO d VALUES (1), (1)")
	if _, err := ss.exec("CREATE UNIQUE INDEX by_x ON d (x)"); !errors.Is(err, table.ErrUnique) {
		t.Errorf("unique index over duplicates: %v", err)
	}
	if _, ok := ss.tx.Schema().Table("d"); !ok {
		t.Fatal("table d is gone")
	}
	if len(mustTable(t, ss, "d").Indexes) != 0 {
		t.Error("the failed index is in the schema")
	}
	// The transaction goes on, and commits.
	ss.must("INSERT INTO t VALUES (3, 'c')")
	ss.commit()
	if got := ss.dump("t"); got != before+"3 'c'\n" {
		t.Errorf("after commit:\n%s", got)
	}
}

func mustTable(t *testing.T, ss *session, name string) *table.Table {
	tb, ok := ss.tx.Schema().Table(name)
	if !ok {
		t.Fatalf("no table %s", name)
	}
	return tb
}

// TestTooLargeStatement: a statement past the bound of the transaction
// fails, and the rollback to the savepoint keeps the transaction usable.
func TestTooLargeStatement(t *testing.T) {
	ss := newSession(t, store.Options{MaxTxBytes: 64 * pageSize})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, body TEXT)")
	body := strings.Repeat("x", 3000)
	var values []string
	for i := 0; i < 200; i++ {
		values = append(values, fmt.Sprintf("(%d, '%s')", i+1, body))
	}
	_, err := ss.exec("INSERT INTO t VALUES " + strings.Join(values, ", "))
	if !errors.Is(err, store.ErrTxTooLarge) {
		t.Fatalf("200 rows of 3 KB in 64 pages: %v", err)
	}
	ss.must("INSERT INTO t VALUES (1, 'small')")
	ss.commit()
	if got := ss.dump("t"); got != "1 'small'\n" {
		t.Errorf("after the failed statement: %q", got)
	}
}

func TestSchemaStatements(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER, b TEXT)")
	ss.must("CREATE TABLE IF NOT EXISTS t (x INTEGER)")
	ss.must("CREATE INDEX i ON t (a)")
	ss.must("CREATE INDEX IF NOT EXISTS i ON t (b)")
	ss.must("ALTER TABLE t ADD COLUMN c REAL")
	ss.must("INSERT INTO t (a, c) VALUES (1, 2)")
	if got := ss.dump("t"); got != "1 1 NULL 2.0\n" {
		t.Errorf("after ADD COLUMN: %q", got)
	}
	ss.must("DROP INDEX i")
	ss.must("DROP INDEX IF EXISTS i")
	ss.must("DROP TABLE t")
	ss.must("DROP TABLE IF EXISTS t")
	if n := len(ss.tx.Schema().Tables); n != 0 {
		t.Errorf("%d tables left", n)
	}
}

func TestStatementErrors(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL, score REAL, flag BOOLEAN)")
	ss.must("CREATE INDEX by_name ON t (name)")
	for _, c := range []struct {
		src       string
		params    []any
		line, col int
		msg       string
		cause     error
	}{
		{"CREATE TABLE t (x INTEGER)", nil, 1, 1, "table t exists", table.ErrSchema},
		{"CREATE TABLE by_name (x INTEGER)", nil, 1, 1, "an index is called by_name", table.ErrSchema},
		{"CREATE TABLE u (rowid INTEGER)", nil, 1, 17, "reserved", nil},
		{"CREATE TABLE u (a INTEGER, a TEXT)", nil, 1, 1, "column a twice", table.ErrSchema},
		{"CREATE INDEX by_name ON t (score)", nil, 1, 1, "index by_name exists", table.ErrSchema},
		{"CREATE INDEX t ON t (score)", nil, 1, 1, "a table is called t", table.ErrSchema},
		{"CREATE INDEX x ON nosuch (a)", nil, 1, 1, "no such table: nosuch", table.ErrNoTable},
		{"CREATE INDEX x ON t (nosuch)", nil, 1, 1, "does not exist", table.ErrSchema},
		{"DROP TABLE nosuch", nil, 1, 1, "no such table", table.ErrNoTable},
		{"DROP INDEX nosuch", nil, 1, 1, "no such index", table.ErrNoIndex},
		{"ALTER TABLE t ADD COLUMN x INTEGER NOT NULL", nil, 1, 26, "must allow NULL", nil},
		{"ALTER TABLE t ADD COLUMN name TEXT", nil, 1, 1, "column name twice", table.ErrSchema},
		{"INSERT INTO nosuch VALUES (1)", nil, 1, 1, "no such table", table.ErrNoTable},
		{"INSERT INTO t VALUES (1, 'a')", nil, 1, 23, "2 values for 4 columns", nil},
		{"INSERT INTO t (id, nosuch) VALUES (1, 2)", nil, 1, 1, "no such column: nosuch", nil},
		{"INSERT INTO t (id, id) VALUES (1, 2)", nil, 1, 1, "column id twice", nil},
		{"INSERT INTO t (id, name) VALUES (1, 2)", nil, 1, 37, "column name is TEXT, and 2 is INTEGER", nil},
		{"INSERT INTO t (id) VALUES (1)", nil, 1, 28, "column t.name is NOT NULL", table.ErrValue},
		{"INSERT INTO t (id, name) VALUES (1, NULL)", nil, 1, 34, "NOT NULL", table.ErrValue},
		{"INSERT INTO t (name, score) VALUES ('a', 9007199254740993)", nil, 1, 42, "has no exact REAL value", nil},
		{"INSERT INTO t (name, flag) VALUES ('a', ?)", []any{int64(1)}, 1, 41, "column flag is BOOLEAN, and ?1 is INTEGER", nil},
		{"INSERT INTO t (name, score) VALUES ('a', 0.0 / 0)", nil, 1, 46, "division by zero", nil},
		{"UPDATE t SET nosuch = 1", nil, 1, 14, "no such column", nil},
		{"UPDATE t SET name = 'a', name = 'b'", nil, 1, 26, "set twice", nil},
		{"UPDATE t SET name = 1", nil, 1, 21, "column name is TEXT", nil},
		{"UPDATE t SET name = 'a' WHERE id", nil, 1, 31, "WHERE needs BOOLEAN", nil},
		{"DELETE FROM t WHERE nosuch = 1", nil, 1, 21, "no such column: nosuch", nil},
		{"DELETE FROM t WHERE u.id = 1", nil, 1, 21, "no such column: u.id", nil},
		{"SELECT 1", nil, 1, 1, "run it with Query", ErrStatement},
		{"BEGIN", nil, 1, 1, "with its methods", ErrStatement},
	} {
		_, err := ss.exec(c.src, c.params...)
		var e *sqlparse.Error
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an error at a position", c.src, err)
			continue
		}
		if e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
		if c.cause != nil && !errors.Is(err, c.cause) {
			t.Errorf("%s: %v does not match %v", c.src, err, c.cause)
		}
	}
	// The rows of the successful statements only.
	ss.must("INSERT INTO t (name, score) VALUES ('a', 3)")
	if got := ss.dump("t"); got != "1 'a' 3.0 NULL\n" {
		t.Errorf("t: %q", got)
	}
}

func TestParams(t *testing.T) {
	ps, err := Params([]any{1, int8(-2), uint32(3), uint64(4), float32(0.5), "s", nil})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(1), int64(-2), int64(3), int64(4), 0.5, "s", nil}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("param %d: %#v, want %#v", i, ps[i], want[i])
		}
	}
	if _, err := Params([]any{uint64(1 << 63)}); err == nil {
		t.Error("2^63 as INTEGER: no error")
	}
}
