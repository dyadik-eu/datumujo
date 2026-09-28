package sqlexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// forge has three repositories; repo 3 has no issue, and issue 5 points
// to a repository that does not exist.
func forge(t *testing.T) *session {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE repo (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	ss.must("CREATE UNIQUE INDEX by_name ON repo (name)")
	ss.must("CREATE TABLE issue (id INTEGER PRIMARY KEY, repo INTEGER, title TEXT, state TEXT)")
	ss.must("CREATE INDEX by_repo ON issue (repo)")
	ss.must("CREATE TABLE comment (id INTEGER PRIMARY KEY, issue INTEGER, body TEXT)")
	ss.must("INSERT INTO repo VALUES (1, 'alpha'), (2, 'beta'), (3, 'gamma')")
	ss.must(`INSERT INTO issue VALUES (1, 1, 'crash', 'open'), (2, 1, 'typo', 'closed'),
		(3, 2, 'slow', 'closed'), (4, 1, 'docs', 'open'), (5, 9, 'lost', 'open')`)
	ss.must("INSERT INTO comment VALUES (1, 1, 'same here'), (2, 1, 'fixed?'), (3, 3, 'yes')")
	return ss
}

func TestJoins(t *testing.T) {
	ss := forge(t)
	for _, c := range []struct {
		src, want string
		plan      string
		scanned   int64
	}{
		{"SELECT r.name, i.title FROM repo r JOIN issue i ON i.repo = r.id ORDER BY 1, 2",
			"alpha crash\nalpha docs\nalpha typo\nbeta slow\n",
			"SCAN repo AS r; SEARCH issue USING INDEX by_repo ((i.repo = r.id)) AS i; SORT", 7},
		// LEFT JOIN keeps gamma with NULL.
		{"SELECT r.name, i.title FROM repo r LEFT JOIN issue i ON i.repo = r.id ORDER BY 1, 2",
			"alpha crash\nalpha docs\nalpha typo\nbeta slow\ngamma <nil>\n",
			"SCAN repo AS r; LEFT SEARCH issue USING INDEX by_repo ((i.repo = r.id)) AS i; SORT", 7},
		// A repository without issues.
		{"SELECT r.name FROM repo r LEFT JOIN issue i ON i.repo = r.id WHERE i.id IS NULL",
			"gamma\n", "", 7},
		// ON decides the match; WHERE drops rows after it.
		{"SELECT r.name, i.title FROM repo r LEFT JOIN issue i ON i.repo = r.id AND i.state = 'open' ORDER BY 1, 2",
			"alpha crash\nalpha docs\nbeta <nil>\ngamma <nil>\n", "", 7},
		{"SELECT r.name, i.title FROM repo r LEFT JOIN issue i ON i.repo = r.id WHERE i.state = 'open' ORDER BY 1, 2",
			"alpha crash\nalpha docs\n", "", 7},
		// Three tables, with a count per repository.
		{`SELECT r.name, count(i.id), count(c.id) FROM repo r LEFT JOIN issue i ON i.repo = r.id
			LEFT JOIN comment c ON c.issue = i.id GROUP BY r.name ORDER BY 1`,
			"alpha 4 2\nbeta 1 1\ngamma 0 0\n", "", 0},
		// The key of the joined table: one row read per issue.
		{"SELECT i.title, r.name FROM issue i LEFT JOIN repo r ON r.id = i.repo ORDER BY 1",
			"crash alpha\ndocs alpha\nlost <nil>\nslow beta\ntypo alpha\n",
			"SCAN issue AS i; LEFT SEARCH repo USING PRIMARY KEY ((r.id = i.repo)) AS r; SORT", 9},
		// WHERE bounds the first table, and an unqualified column name
		// that only one table has.
		{"SELECT title, name FROM issue JOIN repo ON repo.id = issue.repo WHERE issue.id = 3",
			"slow beta\n", "SEARCH issue USING PRIMARY KEY ((issue.id = 3)); SEARCH repo USING PRIMARY KEY ((repo.id = issue.repo))", 2},
		{"SELECT * FROM repo JOIN issue ON issue.repo = repo.id WHERE issue.id = 2",
			"1 alpha 2 1 typo closed\n", "", 0},
		{"SELECT issue.* FROM repo JOIN issue ON issue.repo = repo.id WHERE repo.name = 'beta'",
			"3 2 slow closed\n", "", 0},
		// A table with itself, under two aliases.
		{"SELECT a.id, b.id FROM repo a JOIN repo b ON b.id = a.id + 1 ORDER BY 1",
			"1 2\n2 3\n", "SCAN repo AS a; SEARCH repo USING PRIMARY KEY ((b.id = (a.id + 1))) AS b; SORT", 5},
		// Without a bound, each row of the first table scans the second.
		{"SELECT count(*) FROM repo r JOIN comment c ON c.body LIKE r.name || '%'",
			"0\n", "SCAN repo AS r; SCAN comment AS c; GROUP", 12},
	} {
		plan, scanned, got := ss.planned(t, c.src)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.src, got, c.want)
		}
		if c.plan != "" && plan != c.plan {
			t.Errorf("%s:\n plan %s\n want %s", c.src, plan, c.plan)
		}
		if c.scanned != 0 && scanned != c.scanned {
			t.Errorf("%s: %d rows read, want %d", c.src, scanned, c.scanned)
		}
	}
}

func TestJoinErrors(t *testing.T) {
	ss := forge(t)
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
		cause     error
	}{
		{"SELECT id FROM repo JOIN issue ON issue.repo = repo.id", 1, 8, "ambiguous column name: id", nil},
		{"SELECT 1 FROM repo JOIN repo ON TRUE", 1, 25, "the table name repo is used twice", nil},
		{"SELECT 1 FROM repo r JOIN issue i ON i.id = c.issue JOIN comment c ON TRUE", 1, 45, "no such column: c.issue", nil},
		{"SELECT 1 FROM repo r JOIN issue i ON i.repo", 1, 38, "ON needs BOOLEAN", nil},
		{"SELECT 1 FROM repo r JOIN nosuch n ON TRUE", 1, 27, "no such table: nosuch", table.ErrNoTable},
		{"SELECT x.* FROM repo r JOIN issue i ON TRUE", 1, 8, "no such table: x", nil},
		{"SELECT repo.name FROM repo r JOIN issue i ON TRUE", 1, 8, "no such column: repo.name", nil},
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
		if c.cause != nil && !errors.Is(err, c.cause) {
			t.Errorf("%s: %v does not match %v", c.src, err, c.cause)
		}
	}
}
