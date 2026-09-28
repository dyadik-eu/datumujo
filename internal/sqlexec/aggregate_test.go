package sqlexec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
)

func TestAggregates(t *testing.T) {
	ss := issues(t)
	ss.must("CREATE TABLE r (x REAL)")
	for i := 0; i < 10; i++ {
		ss.must("INSERT INTO r VALUES (0.1)")
	}
	for _, c := range []struct{ src, want string }{
		{"SELECT count(*), count(votes), count(DISTINCT votes), sum(votes), sum(DISTINCT votes), avg(votes), min(votes), max(score) FROM issue",
			"5 4 3 14 11 3.5 1 2.5\n"},
		// Over no row: one row, count 0, the others NULL.
		{"SELECT count(*), sum(votes), avg(votes), min(state) FROM issue WHERE id > 99", "0 NULL NULL NULL\n"},
		// With GROUP BY and no row: no row.
		{"SELECT state, count(*) FROM issue WHERE id > 99 GROUP BY state", ""},
		{"SELECT state, count(*), sum(votes) FROM issue GROUP BY state ORDER BY state",
			"'closed' 2 1\n'draft' 1 3\n'open' 2 10\n"},
		{"SELECT state FROM issue GROUP BY state HAVING count(*) > 1 ORDER BY 1", "'closed'\n'open'\n"},
		{"SELECT state, count(*) AS n FROM issue GROUP BY state ORDER BY n DESC, state", "'closed' 2\n'open' 2\n'draft' 1\n"},
		{"SELECT state FROM issue GROUP BY state ORDER BY max(votes) DESC", "'open'\n'draft'\n'closed'\n"},
		// NULL is one group, and it sorts first.
		{"SELECT votes > 2, count(*) FROM issue GROUP BY votes > 2 ORDER BY 1", "NULL 1\nFALSE 1\nTRUE 3\n"},
		{"SELECT state || '!', count(*) FROM issue GROUP BY state || '!' ORDER BY 1", "'closed!' 2\n'draft!' 1\n'open!' 2\n"},
		{"SELECT sum(votes) * 2, avg(score) + count(*) FROM issue", "28 6.75\n"},
		// A repeated key changes no group.
		{"SELECT state, count(*) FROM issue GROUP BY state, state HAVING sum(votes) > 2 ORDER BY 1", "'draft' 1\n'open' 2\n"},
		{"SELECT DISTINCT count(*) FROM issue GROUP BY state ORDER BY 1", "1\n2\n"},
		{"SELECT state, min(id), max(id) FROM issue GROUP BY state HAVING min(votes) IS NULL OR max(votes) < 2", "'closed' 2 5\n"},
		{"SELECT count(*) FROM issue GROUP BY state ORDER BY state LIMIT 1 OFFSET 1", "1\n"},
		// Without FROM, the one row is a group.
		{"SELECT count(*), sum(3)", "1 3\n"},
		// The sum of REAL values is compensated, as in SQLite.
		{"SELECT sum(x), avg(x), count(x) FROM r", "1.0 0.1 10\n"},
		{"SELECT min(score), max(score) FROM issue WHERE score IS NULL", "NULL NULL\n"},
	} {
		got, err := ss.query(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.src, got, c.want)
		}
	}
}

func TestAggregateErrors(t *testing.T) {
	ss := issues(t)
	ss.must("CREATE TABLE big (n INTEGER)")
	ss.must("INSERT INTO big VALUES (9223372036854775807), (1)")
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"SELECT id, count(*) FROM issue", 1, 8, "column id must be in GROUP BY or in an aggregate function"},
		{"SELECT state, votes FROM issue GROUP BY state", 1, 15, "column votes must be in GROUP BY"},
		{"SELECT state FROM issue GROUP BY state ORDER BY votes", 1, 49, "column votes must be in GROUP BY"},
		{"SELECT count(*) FROM issue HAVING votes > 1", 1, 35, "column votes must be in GROUP BY"},
		{"SELECT * FROM issue GROUP BY state", 1, 8, "* with GROUP BY or an aggregate"},
		{"SELECT id FROM issue WHERE count(*) > 1", 1, 28, "aggregate function count is not allowed here"},
		{"SELECT count(*) FROM issue GROUP BY count(*)", 1, 37, "an aggregate in GROUP BY"},
		{"SELECT sum(max(votes)) FROM issue", 1, 12, "an aggregate inside the aggregate sum"},
		{"SELECT sum(state) FROM issue", 1, 12, "sum needs INTEGER or REAL, and state is TEXT"},
		{"SELECT avg(state) FROM issue", 1, 12, "avg needs INTEGER or REAL"},
		{"SELECT sum(*) FROM issue", 1, 8, "sum(*) does not exist"},
		{"SELECT count(id, state) FROM issue", 1, 8, "count takes 1 argument, not 2"},
		{"SELECT state FROM issue GROUP BY state HAVING count(*)", 1, 47, "HAVING needs BOOLEAN"},
		{"SELECT count(*) FROM issue ORDER BY 2", 1, 37, "ORDER BY 2: the result has 1 columns"},
		{"SELECT sum(n) FROM big", 1, 8, "integer overflow in sum"},
		{"SELECT count(nosuch) FROM issue", 1, 14, "no such column: nosuch"},
	} {
		_, err := ss.query(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an error at a position", c.src, err)
			continue
		}
		if e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// TestGroupMemory: groups count against the bound of memory (L-10).
func TestGroupMemory(t *testing.T) {
	ss := newSession(t, store.Options{MaxTxBytes: 64 << 20})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, g TEXT)")
	for i := 0; i < 2000; i++ {
		ss.must("INSERT INTO t VALUES (?, ?)", int64(i), fmt.Sprintf("group %d", i%500))
	}
	ss.lim = Limits{MaxMemory: 20000}
	if _, err := ss.query("SELECT g, count(*) FROM t GROUP BY g"); !errors.Is(err, ErrMemory) {
		t.Errorf("500 groups: %v, want ErrMemory", err)
	}
	if _, err := ss.query("SELECT count(DISTINCT id) FROM t"); !errors.Is(err, ErrMemory) {
		t.Errorf("count of 2000 distinct values: %v, want ErrMemory", err)
	}
	if got, err := ss.query("SELECT count(*), max(g) FROM t"); err != nil || got != "2000 'group 99'\n" {
		t.Errorf("one group: %q %v", got, err)
	}
	// The values of the keys count, next to the text that finds a group:
	// 100 keys of 200 bytes each take 24400 bytes as values alone.
	ss.lim = Limits{}
	ss.must("CREATE TABLE w (id INTEGER PRIMARY KEY, k TEXT)")
	for i := 0; i < 100; i++ {
		ss.must("INSERT INTO w VALUES (?, ?)", int64(i), fmt.Sprintf("%0200d", i))
	}
	ss.lim = Limits{MaxMemory: 36000}
	if _, err := ss.query("SELECT k, count(*) FROM w GROUP BY k"); !errors.Is(err, ErrMemory) {
		t.Errorf("100 keys of 200 bytes in 36000 bytes: %v, want ErrMemory", err)
	}
	ss.lim = Limits{MaxMemory: 60000}
	if _, err := ss.query("SELECT k, count(*) FROM w GROUP BY k"); err != nil {
		t.Errorf("100 keys of 200 bytes in 60000 bytes: %v", err)
	}
}

// TestAggregateTypes: values of mixed type, measured against SQLite on
// 28.09.2026. max(n, r) gives an INTEGER or a REAL per row.
func TestAggregateTypes(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE m (id INTEGER PRIMARY KEY, n INTEGER, r REAL)")
	// The INTEGER sum overflows, then a REAL comes: the sum is a REAL.
	ss.must("INSERT INTO m VALUES (1, 9223372036854775807, -1.0), (2, 1, -1.0), (3, 0, 0.5)")
	ss.must("CREATE TABLE e (id INTEGER PRIMARY KEY, n INTEGER, r REAL)")
	// 1 and 1.0 are equal; min and max keep the first, the INTEGER.
	ss.must("INSERT INTO e VALUES (1, 1, 0.0), (2, 0, 1.0)")
	// An INTEGER, then a REAL: the REAL sum starts from the INTEGER sum.
	// Then two large INTEGERs, which go into the REAL sum in two parts
	// each; as float64 they would round and cancel to 0. SQLite: 6.5.
	ss.must("CREATE TABLE b (id INTEGER PRIMARY KEY, n INTEGER, r REAL)")
	ss.must(`INSERT INTO b VALUES (1, 5, -1e300), (2, -9223372036854775808, 0.5),
		(3, 9223372036854775807, -1e300), (4, -9223372036854775806, -1e300)`)
	ss.must("CREATE TABLE huge (x REAL)")
	ss.must("INSERT INTO huge VALUES (1e308), (1e308)")
	for _, c := range []struct{ src, want string }{
		{"SELECT sum(max(n, r)) FROM m", "9.223372036854776e+18\n"},
		{"SELECT min(max(n, r)), max(max(n, r)) FROM e", "1 1\n"},
		{"SELECT sum(max(n, r)) FROM b", "6.5\n"},
	} {
		got, err := ss.query(c.src)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
	got, err := ss.query("SELECT sum(max(n, r)) FROM m")
	if err == nil {
		v := strings.TrimSpace(got)
		if f, perr := strconv.ParseFloat(v, 64); perr != nil || f != 9223372036854775808 {
			t.Errorf("sum after the overflow: %s, want 9223372036854775808 as SQLite gives", v)
		}
	}
	if _, err := ss.query("SELECT sum(x) FROM huge"); err == nil || !strings.Contains(err.Error(), "REAL overflow in sum") {
		t.Errorf("sum past float64: %v", err)
	}
}
