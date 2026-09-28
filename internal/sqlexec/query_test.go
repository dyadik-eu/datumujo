package sqlexec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// query runs a SELECT in the session and prints the result, one line per
// row, values as SQL literals.
func (ss *session) query(src string, params ...any) (string, error) {
	st, err := sqlparse.Parse(src)
	if err != nil {
		return "", err
	}
	q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), ss.lim)
	if err != nil {
		return "", err
	}
	rows, err := q.Run(ss.tx, params)
	if err != nil {
		return "", err
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
	return b.String(), rows.Err()
}

func issues(t *testing.T) *session {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE issue (id INTEGER PRIMARY KEY, state TEXT NOT NULL, votes INTEGER, score REAL)")
	ss.must(`INSERT INTO issue VALUES (1, 'open', 3, 1.5), (2, 'closed', NULL, 2.0), (3, 'open', 7, NULL),
		(4, 'draft', 3, 1.0), (5, 'closed', 1, 2.5)`)
	ss.must("CREATE TABLE note (body TEXT)")
	ss.must("INSERT INTO note VALUES ('b'), ('a'), (NULL)")
	return ss
}

func TestSelect(t *testing.T) {
	ss := issues(t)
	for _, c := range []struct {
		src, want string
		params    []any
	}{
		{"SELECT * FROM issue WHERE id = 1", "1 'open' 3 1.5\n", nil},
		{"SELECT id, votes * 2 AS v FROM issue WHERE votes > 2", "1 6\n3 14\n4 6\n", nil},
		// WHERE keeps TRUE only: votes > 2 is NULL for id 2.
		{"SELECT id FROM issue WHERE NOT votes > 2", "5\n", nil},
		{"SELECT id FROM issue WHERE state = ?", "2\n5\n", []any{"closed"}},
		// ORDER BY: NULL first, DESC puts it last; ties keep key order.
		{"SELECT id, votes FROM issue ORDER BY votes", "2 NULL\n5 1\n1 3\n4 3\n3 7\n", nil},
		{"SELECT id, votes FROM issue ORDER BY votes DESC", "3 7\n1 3\n4 3\n5 1\n2 NULL\n", nil},
		{"SELECT id, state FROM issue ORDER BY 2, 1 DESC", "5 'closed'\n2 'closed'\n4 'draft'\n3 'open'\n1 'open'\n", nil},
		{"SELECT id AS n, state FROM issue ORDER BY n DESC LIMIT 2", "5 'closed'\n4 'draft'\n", nil},
		{"SELECT id FROM issue ORDER BY score * -1, id", "3\n5\n2\n1\n4\n", nil},
		// ORDER BY a column that is not in the result.
		{"SELECT state FROM issue ORDER BY id DESC", "'closed'\n'draft'\n'open'\n'closed'\n'open'\n", nil},
		{"SELECT DISTINCT state FROM issue", "'open'\n'closed'\n'draft'\n", nil},
		{"SELECT DISTINCT state FROM issue ORDER BY 1", "'closed'\n'draft'\n'open'\n", nil},
		{"SELECT DISTINCT votes FROM issue ORDER BY 1 DESC", "7\n3\n1\nNULL\n", nil},
		// 3 and 3.0 are one value for DISTINCT, as in SQLite. The first
		// row gives max(3, 3.0), which is the INTEGER 3.
		{"SELECT DISTINCT max(votes, 3.0) FROM issue WHERE votes IS NOT NULL ORDER BY 1", "3\n7\n", nil},
		{"SELECT id FROM issue LIMIT 2 OFFSET 1", "2\n3\n", nil},
		{"SELECT id FROM issue LIMIT -1 OFFSET 3", "4\n5\n", nil},
		{"SELECT id FROM issue LIMIT 2 OFFSET -5", "1\n2\n", nil},
		{"SELECT id FROM issue LIMIT -7", "1\n2\n3\n4\n5\n", nil},
		// An alias after * counts the columns of *.
		// Here the second column, state, would give the other order.
		{"SELECT *, id AS m FROM issue WHERE id > 3 ORDER BY m", "4 'draft' 3 1.0 4\n5 'closed' 1 2.5 5\n", nil},
		{"SELECT id FROM issue LIMIT 0", "", nil},
		{"SELECT id FROM issue LIMIT ?1 OFFSET ?2", "4\n", []any{int64(1), int64(3)}},
		{"SELECT id FROM issue AS i WHERE i.id < 3", "1\n2\n", nil},
		{"SELECT i.* FROM issue i WHERE id = 4", "4 'draft' 3 1.0\n", nil},
		{"SELECT issue.state FROM issue WHERE id = 2", "'closed'\n", nil},
		// A table without a key: * leaves out the hidden key, rowid shows it.
		{"SELECT * FROM note", "'b'\n'a'\nNULL\n", nil},
		{"SELECT rowid, body FROM note ORDER BY body", "3 NULL\n2 'a'\n1 'b'\n", nil},
		// Without FROM, one row; WHERE can drop it.
		{"SELECT 1 + 1, 'x' || 'y'", "2 'xy'\n", nil},
		{"SELECT 1 WHERE FALSE", "", nil},
		{"SELECT 1 LIMIT 1 OFFSET 1", "", nil},
	} {
		got, err := ss.query(c.src, c.params...)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.src, got, c.want)
		}
	}
}

func TestSelectColumns(t *testing.T) {
	ss := issues(t)
	st, _ := sqlparse.Parse("SELECT *, votes + 1, score AS s, ?1 FROM issue")
	q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range q.Columns() {
		typ := "?"
		if c.Known {
			typ = TypeName(c.Type)
		}
		got = append(got, c.Name+":"+typ)
	}
	want := "id:INTEGER state:TEXT votes:INTEGER score:REAL (votes + 1):INTEGER s:REAL ?1:?"
	if strings.Join(got, " ") != want {
		t.Errorf("columns %s\n want %s", strings.Join(got, " "), want)
	}
}

func TestSelectErrors(t *testing.T) {
	ss := issues(t)
	for _, c := range []struct {
		src       string
		params    []any
		line, col int
		msg       string
		cause     error
	}{
		{"SELECT * FROM nosuch", nil, 1, 15, "no such table: nosuch", table.ErrNoTable},
		{"SELECT nosuch FROM issue", nil, 1, 8, "no such column: nosuch", nil},
		{"SELECT issue.id FROM issue AS i", nil, 1, 8, "no such column: issue.id", nil},
		{"SELECT x.* FROM issue", nil, 1, 8, "no such table: x", nil},
		{"SELECT *", nil, 1, 8, "* needs a table", nil},
		{"SELECT id FROM issue WHERE votes", nil, 1, 28, "WHERE needs BOOLEAN", nil},
		{"SELECT id FROM issue ORDER BY 3", nil, 1, 31, "the result has 1 columns", nil},
		{"SELECT id FROM issue ORDER BY 0", nil, 1, 31, "the result has 1 columns", nil},
		{"SELECT id FROM issue LIMIT 'a'", nil, 1, 28, "LIMIT needs INTEGER", nil},
		{"SELECT id FROM issue LIMIT id", nil, 1, 28, "no such column: id", nil},
		{"SELECT id FROM issue LIMIT ?", []any{nil}, 1, 28, "need an INTEGER", nil},
		{"SELECT id FROM issue OFFSET 1", nil, 1, 22, "; or the end", nil},
		{"SELECT id FROM issue GROUP BY state", nil, 1, 8, "column id must be in GROUP BY or in an aggregate function", nil},
		{"SELECT id FROM issue JOIN note ON TRUE", nil, 1, 22, "JOIN is not supported yet (roadmap step 22)", ErrStatement},
		{"SELECT id / 0 FROM issue", nil, 1, 11, "division by zero", nil},
	} {
		_, err := ss.query(c.src, c.params...)
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
}

// TestSelectStreams: a query without ORDER BY and DISTINCT gives each row
// when it is read. An error in a later row ends the rows with Err, after
// the rows before it.
func TestSelectStreams(t *testing.T) {
	ss := issues(t)
	st, _ := sqlparse.Parse("SELECT id, 10 / (3 - id) FROM issue")
	q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Run(ss.tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if n != 2 || rows.Err() == nil || !strings.Contains(rows.Err().Error(), "division by zero") {
		t.Errorf("%d rows, then %v", n, rows.Err())
	}
	if rows.Next() {
		t.Error("Next after the error")
	}
}

// TestSelectOrderIsStable: equal keys of ORDER BY keep the order of the
// table. Go sorts fewer than 12 values stably in any case, so the table
// has 40 rows.
func TestSelectOrderIsStable(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, g INTEGER)")
	for i := 1; i <= 40; i++ {
		ss.must("INSERT INTO t VALUES (?, ?)", int64(i), int64(i%3))
	}
	got, err := ss.query("SELECT id FROM t ORDER BY g")
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for g := 0; g < 3; g++ {
		for i := 1; i <= 40; i++ {
			if i%3 == g {
				fmt.Fprintf(&want, "%d\n", i)
			}
		}
	}
	if got != want.String() {
		t.Errorf("order:\n%s", got)
	}
}

// TestSelectDistinctKey: the key of DISTINCT keeps values apart that a
// simple join of the values would run together.
func TestSelectDistinctKey(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a TEXT, b TEXT)")
	ss.must("INSERT INTO t VALUES ('a;sb', 'c'), ('a', 'b;sc'), ('a', 'b;sc'), (NULL, 'x'), ('', 'x')")
	got, err := ss.query("SELECT DISTINCT a, b FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if want := "'a;sb' 'c'\n'a' 'b;sc'\nNULL 'x'\n'' 'x'\n"; got != want {
		t.Errorf("distinct:\n%s", got)
	}
}

// TestSelectCheckedBeforeRun: a type that does not fit is an error of
// Prepare, also when no row would reach the check of the run.
func TestSelectCheckedBeforeRun(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (n INTEGER)")
	st, _ := sqlparse.Parse("SELECT n FROM t WHERE n")
	if _, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), Limits{}); err == nil || !strings.Contains(err.Error(), "WHERE needs BOOLEAN") {
		t.Errorf("WHERE n on an empty table: %v", err)
	}
}
