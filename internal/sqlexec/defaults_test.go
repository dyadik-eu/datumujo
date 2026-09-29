package sqlexec

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// TestDefaults checks L-13: an INSERT that does not name a column gives
// it its default. A column it names keeps the value, NULL included. An
// added column with a default gives it to the rows that exist, so it can
// be NOT NULL.
func TestDefaults(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must(`CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER NOT NULL DEFAULT -3, r REAL DEFAULT 2,
		s TEXT DEFAULT 'a''b', y BLOB DEFAULT X'00ff', b BOOLEAN DEFAULT TRUE, z INTEGER DEFAULT NULL)`)
	ss.must("INSERT INTO t (id) VALUES (1)")
	ss.must("INSERT INTO t (id, n, s) VALUES (2, 7, NULL)")
	ss.must("INSERT INTO t VALUES (3, 1, 1.5, 'x', X'', FALSE, 4)")
	ss.must("INSERT INTO t (s) VALUES ('auto'), ('key')")
	want := "1 -3 2.0 'a''b' X'00FF' TRUE NULL\n" +
		"2 7 2.0 NULL X'00FF' TRUE NULL\n" +
		"3 1 1.5 'x' X'' FALSE 4\n" +
		"4 -3 2.0 'auto' X'00FF' TRUE NULL\n" +
		"5 -3 2.0 'key' X'00FF' TRUE NULL\n"
	if got := ss.dump("t"); got != want {
		t.Errorf("t:\n%s\nwant\n%s", got, want)
	}
	// DEFAULT NULL on a column that allows NULL is no default: the schema
	// holds none for z.
	tb, _ := ss.tx.Schema().Table("t")
	if tb.Columns[6].Default != "" || tb.Columns[1].Default != "-3" || tb.Columns[2].Default != "2" {
		t.Errorf("the defaults in the schema: %+v", tb.Columns)
	}

	// A key that is not the next key can have a default.
	ss.must("CREATE TABLE k (name TEXT PRIMARY KEY DEFAULT 'none', v INTEGER)")
	ss.must("INSERT INTO k (v) VALUES (1)")
	if _, err := ss.exec("INSERT INTO k (v) VALUES (2)"); !errors.Is(err, table.ErrExists) {
		t.Errorf("a second row with the default key: %v", err)
	}

	// In a key of two columns, no column takes the next key, so an
	// INTEGER column there can have a default.
	ss.must("CREATE TABLE two (a INTEGER DEFAULT 1, b INTEGER, PRIMARY KEY (a, b))")
	ss.must("INSERT INTO two (b) VALUES (5)")
	if got := ss.dump("two"); got != "1 5\n" {
		t.Errorf("two: %q", got)
	}

	// ALTER TABLE: the rows that exist read the default, and the column
	// can be NOT NULL.
	ss.must("ALTER TABLE t ADD COLUMN added TEXT NOT NULL DEFAULT 'old'")
	ss.must("ALTER TABLE t ADD COLUMN more REAL DEFAULT 5")
	ss.must("ALTER TABLE t ADD COLUMN zero REAL NOT NULL DEFAULT -0.0")
	ss.must("INSERT INTO t (id, added) VALUES (6, 'new')")
	ss.must("UPDATE t SET more = 0 WHERE id = 2")
	ss.commit()
	got, err := ss.query("SELECT id, added, more FROM t ORDER BY id")
	wantRows := "1 'old' 5.0\n2 'old' 0.0\n3 'old' 5.0\n4 'old' 5.0\n5 'old' 5.0\n6 'new' 5.0\n"
	if err != nil || got != wantRows {
		t.Errorf("after ALTER TABLE:\n%s\nwant\n%s%v", got, wantRows, err)
	}
	// -0.0 keeps its sign, in the rows that exist and in a new one.
	for _, id := range []int64{1, 6} {
		row, _, err := ss.tx.Get("t", id)
		if f, ok := row[9].(float64); err != nil || !ok || f != 0 || !math.Signbit(f) {
			t.Errorf("row %d: zero is %v, want -0.0: %v", id, row[9], err)
		}
	}
	if _, err := ss.exec("INSERT INTO t (id, added) VALUES (7, NULL)"); !errors.Is(err, table.ErrValue) {
		t.Errorf("NULL in the added NOT NULL column: %v", err)
	}
}

// TestCurrentTimestamp: every row of one INSERT gets the same time, the
// time of the statement, in UTC.
func TestCurrentTimestamp(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE e (id INTEGER PRIMARY KEY, at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)")
	before := time.Now()
	ss.must("INSERT INTO e (id) VALUES (1), (2), (3)")
	after := time.Now()
	ss.must("INSERT INTO e VALUES (4, ?)", time.Unix(0, 0))
	rows, err := ss.tx.Scan("e", table.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var times []time.Time
	for rows.Next() {
		times = append(times, rows.Row()[1].(time.Time))
	}
	if err := rows.Err(); err != nil || len(times) != 4 {
		t.Fatalf("%d rows, %v", len(times), err)
	}
	at := times[0]
	if at.Before(before) || at.After(after) || at.Location() != time.UTC {
		t.Errorf("CURRENT_TIMESTAMP %v, the statement ran from %v to %v", at, before, after)
	}
	if !times[1].Equal(at) || !times[2].Equal(at) {
		t.Errorf("the rows of one INSERT got %v", times[:3])
	}
	if !times[3].Equal(time.Unix(0, 0)) {
		t.Errorf("a given value became %v", times[3])
	}
}

// TestDefaultErrors: a default that could not be used, or not be stored
// exactly, is an error when the table is made, not when a row is
// written.
func TestDefaultErrors(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"CREATE TABLE u (id INTEGER PRIMARY KEY DEFAULT 7)", 1, 48, "takes the next key"},
		{"CREATE TABLE u (id INTEGER DEFAULT 7, PRIMARY KEY (id))", 1, 36, "takes the next key"},
		{"CREATE TABLE u (a INTEGER NOT NULL DEFAULT NULL)", 1, 44, "NOT NULL, and its default is NULL"},
		{"CREATE TABLE u (a TEXT PRIMARY KEY DEFAULT NULL)", 1, 44, "NOT NULL, and its default is NULL"},
		{"CREATE TABLE u (a INTEGER DEFAULT 'x')", 1, 35, "column a is INTEGER, and 'x' is TEXT"},
		{"CREATE TABLE u (a INTEGER DEFAULT 1.5)", 1, 35, "column a is INTEGER, and 1.5 is REAL"},
		{"CREATE TABLE u (a INTEGER DEFAULT TRUE)", 1, 35, "column a is INTEGER, and TRUE is BOOLEAN"},
		{"CREATE TABLE u (a REAL DEFAULT 9007199254740993)", 1, 32, "has no exact REAL value"},
		{"CREATE TABLE u (a TEXT DEFAULT CURRENT_TIMESTAMP)", 1, 32, "column a is TEXT, and CURRENT_TIMESTAMP is TIMESTAMP"},
		{"ALTER TABLE t ADD COLUMN at TIMESTAMP DEFAULT CURRENT_TIMESTAMP", 1, 47, "takes a constant default"},
		{"ALTER TABLE t ADD COLUMN n INTEGER NOT NULL DEFAULT NULL", 1, 53, "NOT NULL, and its default is NULL"},
		{"ALTER TABLE t ADD COLUMN n INTEGER DEFAULT 'x'", 1, 44, "column n is INTEGER, and 'x' is TEXT"},
	} {
		_, err := ss.exec(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an error at a position", c.src, err)
			continue
		}
		if e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
	if n := len(ss.tx.Schema().Tables); n != 1 {
		t.Errorf("%d tables after only rejected ones", n)
	}
	if tb, _ := ss.tx.Schema().Table("t"); len(tb.Columns) != 2 {
		t.Errorf("rejected columns were added: %+v", tb.Columns)
	}
}

// TestDefaultsKeepVersion1: a default of NULL stores nothing, so the file
// stays at format version 1, which v0.2.0 reads (L-19). A real default
// raises it.
func TestDefaultsKeepVersion1(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT DEFAULT NULL)")
	ss.must("ALTER TABLE t ADD COLUMN w INTEGER DEFAULT NULL")
	if ss.stx.Version() != 1 {
		t.Errorf("format version %d with defaults of NULL only", ss.stx.Version())
	}
	ss.must("ALTER TABLE t ADD COLUMN x INTEGER DEFAULT 0")
	if ss.stx.Version() != 2 {
		t.Errorf("format version %d with a default", ss.stx.Version())
	}
}
