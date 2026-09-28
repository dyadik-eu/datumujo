package sqlexec

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// schema is the start of every workload. Table t1 has an INTEGER key and
// two indexes, one unique. Table t2 has a key over two columns. Table t3
// has no key.
var schema = []string{
	"CREATE TABLE t1 (id INTEGER PRIMARY KEY, a INTEGER, b TEXT, c REAL NOT NULL, d BOOLEAN)",
	"CREATE INDEX t1_a ON t1 (a)",
	"CREATE UNIQUE INDEX t1_b ON t1 (b)",
	"CREATE TABLE t2 (k TEXT, n INTEGER, v BLOB, w REAL, PRIMARY KEY (k, n))",
	"CREATE TABLE t3 (x INTEGER, y TEXT)",
}

// workload writes random statements over the schema. Each is valid SQL
// for both SQLite and this package. Some fail in both: a key that
// exists, a value that breaks a unique index, a NULL in a NOT NULL
// column. The workload leaves out one difference to SQLite (see the
// oracle table in the requirements). It sets no key column and no
// column of a unique index to an expression. There the order of the
// rows decides the outcome, and SQLite visits them in another order.
type workload struct {
	r       *rand.Rand
	hasZ    bool // t3 has the column z
	altered bool
}

func (w *workload) one(xs ...string) string { return xs[w.r.Intn(len(xs))] }

func (w *workload) maybeNull(v string) string {
	if w.r.Intn(8) == 0 {
		return "NULL"
	}
	return v
}

func (w *workload) intLit() string  { return strconv.Itoa(w.r.Intn(16) - 3) }
func (w *workload) textLit() string { return w.one("'a'", "'b'", "'c'", "'ab'", "'A'", "''", "'ä'") }
func (w *workload) realLit() string { return w.one("0.5", "1.0", "2.25", "-1.5", "2", "0") }
func (w *workload) boolLit() string { return w.one("TRUE", "FALSE") }
func (w *workload) blobLit() string { return w.one("X'00'", "X'01ff'", "X''", "X'61'") }

func (w *workload) cond(tbl string) string {
	var c string
	switch tbl {
	case "t1":
		c = w.one(
			"a "+w.one("=", "<", ">=", "!=")+" "+w.intLit(),
			"a IS NULL", "b IS NOT NULL", "b LIKE 'a%'", "d", "NOT d",
			"id BETWEEN "+w.intLit()+" AND "+w.intLit(),
			"id IN ("+w.intLit()+", "+w.intLit()+")", "c > "+w.realLit(),
		)
	case "t2":
		c = w.one("k = "+w.textLit(), "n < "+w.intLit(), "w IS NULL", "v = "+w.blobLit(), "k = "+w.textLit()+" AND n = "+w.intLit())
	case "t3":
		c = w.one("x > "+w.intLit(), "y IS NULL", "y = "+w.textLit(), "x IN (1, 2, 3)")
	}
	if w.r.Intn(4) == 0 {
		c = "(" + c + ") " + w.one("AND", "OR") + " (" + w.cond(tbl) + ")"
	}
	return c
}

func (w *workload) where(tbl string) string {
	if w.r.Intn(6) == 0 {
		return ""
	}
	return " WHERE " + w.cond(tbl)
}

func (w *workload) rows(n int, row func() string) string {
	var rs []string
	for i := 0; i < n; i++ {
		rs = append(rs, "("+row()+")")
	}
	return strings.Join(rs, ", ")
}

func (w *workload) statement() string {
	n := 1 + w.r.Intn(3)
	switch w.r.Intn(14) {
	case 0, 1:
		return "INSERT INTO t1 VALUES " + w.rows(n, func() string {
			return strings.Join([]string{
				w.one("NULL", strconv.Itoa(1+w.r.Intn(15))), w.maybeNull(w.intLit()), w.maybeNull(w.textLit()),
				w.maybeNull(w.realLit()), w.maybeNull(w.boolLit()),
			}, ", ")
		})
	case 2:
		return "INSERT INTO t1 (a, c) VALUES " + w.rows(n, func() string { return w.maybeNull(w.intLit()) + ", " + w.realLit() })
	case 3:
		return "INSERT INTO t2 VALUES " + w.rows(n, func() string {
			return strings.Join([]string{w.textLit(), strconv.Itoa(w.r.Intn(4)), w.maybeNull(w.blobLit()), w.maybeNull(w.realLit())}, ", ")
		})
	case 4:
		if w.hasZ {
			return "INSERT INTO t3 VALUES " + w.rows(n, func() string {
				return w.maybeNull(w.intLit()) + ", " + w.maybeNull(w.textLit()) + ", " + w.maybeNull(w.realLit())
			})
		}
		return "INSERT INTO t3 VALUES " + w.rows(n, func() string { return w.maybeNull(w.intLit()) + ", " + w.maybeNull(w.textLit()) })
	case 5:
		return "UPDATE t1 SET " + w.one(
			"a = a + "+w.intLit(), "a = -a", "a = abs(a)", "a = coalesce(a, "+w.intLit()+")",
			"c = c * 2", "c = round(c + 0.25, 1)", "c = a", "d = NOT d", "d = a > 3",
			"b = "+w.maybeNull(w.textLit()), "a = CASE WHEN d THEN a ELSE "+w.intLit()+" END",
		) + w.where("t1")
	case 6:
		return "UPDATE t2 SET " + w.one("w = w / 2", "w = n", "v = "+w.blobLit(), "w = coalesce(w, 1.5) + n") + w.where("t2")
	case 7:
		return "UPDATE t3 SET " + w.one("x = x + 1", "y = y || 'x'", "y = upper(y)", "y = substr(y, 1, 1)", "x = length(y)") + w.where("t3")
	case 8, 9:
		t := w.one("t1", "t2", "t3")
		return "DELETE FROM " + t + w.where(t)
	case 10:
		return w.one("CREATE INDEX IF NOT EXISTS t3_y ON t3 (y)", "DROP INDEX IF EXISTS t3_y", "CREATE UNIQUE INDEX t3_x ON t3 (x)", "DROP INDEX t3_x")
	case 11:
		if !w.altered && w.r.Intn(3) == 0 {
			w.altered, w.hasZ = true, true
			return "ALTER TABLE t3 ADD COLUMN z REAL"
		}
		return "INSERT INTO t1 (id, c) VALUES (" + strconv.Itoa(1+w.r.Intn(15)) + ", " + w.maybeNull(w.realLit()) + ")"
	case 12:
		return "INSERT INTO t2 (k, n) VALUES (" + w.textLit() + ", " + strconv.Itoa(w.r.Intn(4)) + ")"
	}
	return "UPDATE t1 SET c = c + 1" + w.where("t1")
}

// dumpQuery is the SQLite query that prints a table in key order, as
// oracleForm prints a value. The argument cols names the columns, with
// the hidden key first where there is one.
func dumpQuery(tbl string, cols []string, order string) string {
	var parts []string
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("typeof(%s), CASE typeof(%s) WHEN 'real' THEN hex(ieee754_to_blob(%s)) WHEN 'text' THEN hex(%s) WHEN 'blob' THEN hex(%s) ELSE %s END", c, c, c, c, c, c))
	}
	return fmt.Sprintf("SELECT 'D', '%s', %s FROM %s ORDER BY %s;", tbl, strings.Join(parts, ", "), tbl, order)
}

// TestWritesAgainstSQLite runs one random workload in SQLite and here.
// Each statement must fail in both or in neither. A statement that
// succeeds must change as many rows. At the end every table must hold
// the same rows with the same values and storage classes (P-4).
func TestWritesAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{18, 1018} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { writesAgainstSQLite(t, seed, 500) })
	}
}

func writesAgainstSQLite(t *testing.T, seed int64, n int) {
	w := &workload{r: rand.New(rand.NewSource(seed))}
	stmts := append([]string(nil), schema...)
	for len(stmts) < len(schema)+n {
		stmts = append(stmts, w.statement())
	}
	var script strings.Builder
	for i, s := range stmts {
		fmt.Fprintf(&script, "%s;\nSELECT 'C', %d, changes();\n", s, i)
	}
	t3cols := []string{"rowid", "x", "y"}
	if w.hasZ {
		t3cols = append(t3cols, "z")
	}
	tables := []struct {
		name  string
		cols  []string
		order string
	}{
		{"t1", []string{"id", "a", "b", "c", "d"}, "id"},
		{"t2", []string{"k", "n", "v", "w"}, "k, n"},
		{"t3", t3cols, "rowid"},
	}
	for _, tb := range tables {
		script.WriteString(dumpQuery(tb.name, tb.cols, tb.order) + "\n")
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())

	ss := newSession(t, store.Options{})
	ours := make([]error, len(stmts))
	changes := make([]int64, len(stmts))
	for i, s := range stmts {
		r, err := ss.exec(s)
		ours[i], changes[i] = err, r.RowsAffected
	}
	theirChanges := map[int]string{}
	var theirDump []string
	for _, f := range lines {
		switch f[0] {
		case "C":
			i, _ := strconv.Atoi(f[1])
			theirChanges[i] = f[2]
		case "D":
			theirDump = append(theirDump, strings.Join(f[1:], " "))
		default:
			t.Fatalf("sqlite3 line %q", f)
		}
	}
	failed, compared := 0, 0
	for i, s := range stmts {
		theirErr, theirFailed := sqliteErrs[2*i+1]
		switch {
		case theirFailed != (ours[i] != nil):
			t.Errorf("statement %d: %s\n SQLite: %q\n here: %v", i, s, theirErr, ours[i])
		case theirFailed:
			failed++
		default:
			compared++
			// changes() of SQLite keeps the count of the last INSERT,
			// UPDATE or DELETE through a CREATE, DROP or ALTER.
			dml := strings.HasPrefix(s, "INSERT") || strings.HasPrefix(s, "UPDATE") || strings.HasPrefix(s, "DELETE")
			if got := strconv.FormatInt(changes[i], 10); dml && got != theirChanges[i] {
				t.Errorf("statement %d: %s\n SQLite changed %s rows, here %s", i, s, theirChanges[i], got)
			}
		}
	}
	var ourDump []string
	for _, tb := range tables {
		rows, err := ss.tx.Scan(tb.name, table.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			fields := []string{tb.name}
			for _, v := range rows.Row() {
				class, value := oracleForm(v)
				fields = append(fields, class, value)
			}
			ourDump = append(ourDump, strings.Join(fields, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(ourDump, "\n") != strings.Join(theirDump, "\n") {
		t.Errorf("the tables differ\nSQLite:\n%s\nhere:\n%s", strings.Join(theirDump, "\n"), strings.Join(ourDump, "\n"))
	}
	t.Logf("%d statements: %d succeeded in both, %d failed in both; %d rows at the end", len(stmts), compared, failed, len(ourDump))
	if compared < len(stmts)/2 || failed == 0 || len(ourDump) == 0 {
		t.Errorf("a weak workload: %d succeeded, %d failed, %d rows", compared, failed, len(ourDump))
	}
}
