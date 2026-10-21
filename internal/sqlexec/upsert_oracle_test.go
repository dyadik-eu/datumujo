package sqlexec

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// TestUpsertAgainstSQLite runs a random script of INSERT ... ON CONFLICT
// in SQLite and here (L-17, P-4). The values come from small sets, so
// rows often conflict, also two rows of one INSERT. The targets are none,
// the key, a unique index, and some that match no key. In DO UPDATE,
// the script reads excluded and the row there is, with or without
// WHERE. It holds no subquery there: here that is an error on purpose.
//
// Each statement must fail in both or in neither. One that runs in both
// must change as many rows, and both tables must hold the same rows at
// the end.
func TestUpsertAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{33, 1033} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { upsertAgainstSQLite(t, seed) })
	}
}

func upsertAgainstSQLite(t *testing.T, seed int64) {
	r := rand.New(rand.NewSource(seed))
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	num := func() string {
		if r.Intn(10) == 0 {
			return "NULL"
		}
		return strconv.Itoa(r.Intn(6))
	}
	text := func() string {
		if r.Intn(10) == 0 {
			return "NULL"
		}
		return pick("'a'", "'b'", "'c'", "'d'", "'e'")
	}
	steps := []string{
		"CREATE TABLE p (id INTEGER PRIMARY KEY, a TEXT UNIQUE, n INTEGER NOT NULL DEFAULT 0, b INTEGER)",
		"CREATE TABLE q (c INTEGER, d TEXT, e INTEGER, UNIQUE (c, d))",
	}
	// clause is ON CONFLICT for p or q, with its targets and the names
	// of DO UPDATE.
	clause := func(table string) string {
		targets := []string{"", " (id)", " (a)", " (b)"}
		sets := []string{
			"n = n + excluded.n", "b = excluded.b", "b = coalesce(b, 0) + 1",
			"a = excluded.a", "n = excluded.n, b = p.b", "id = excluded.id + 10",
			"n = NULL",
		}
		wheres := []string{"", " WHERE excluded.n > p.n", " WHERE b IS NULL", " WHERE p.a < excluded.a"}
		if table == "q" {
			targets = []string{"", " (c, d)", " (d, c)", " (e)"}
			sets = []string{"e = coalesce(e, 0) + excluded.e", "e = excluded.e", "d = excluded.d", "c = q.c + 1"}
			wheres = []string{"", " WHERE excluded.e > q.e", " WHERE e IS NULL"}
		}
		target := pick(targets...)
		if r.Intn(3) == 0 {
			return " ON CONFLICT" + target + " DO NOTHING"
		}
		return " ON CONFLICT" + target + " DO UPDATE SET " + pick(sets...) + pick(wheres...)
	}
	for len(steps) < 300 {
		var rows []string
		if r.Intn(2) == 0 {
			for k := 1 + r.Intn(3); k > 0; k-- {
				id := strconv.Itoa(1 + r.Intn(8))
				if r.Intn(4) == 0 {
					id = "NULL"
				}
				rows = append(rows, fmt.Sprintf("(%s, %s, %s, %s)", id, text(), num(), num()))
			}
			steps = append(steps, "INSERT INTO p VALUES "+strings.Join(rows, ", ")+clause("p"))
		} else {
			for k := 1 + r.Intn(3); k > 0; k-- {
				rows = append(rows, fmt.Sprintf("(%s, %s, %s)", num(), text(), num()))
			}
			steps = append(steps, "INSERT INTO q VALUES "+strings.Join(rows, ", ")+clause("q"))
		}
	}

	// SQLite: changes() after each statement, so a step that runs in
	// both can be counted.
	var script strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&script, "%s;\nSELECT 'C', %d, changes();\n", s, i)
	}
	queries := []string{"SELECT id, a, n, b FROM p ORDER BY id", "SELECT rowid, c, d, e FROM q ORDER BY rowid"}
	names := []string{"p", "q"}
	for k, q := range queries {
		cols := strings.Split(strings.TrimPrefix(strings.Split(q, " FROM")[0], "SELECT "), ", ")
		var forms []string
		for _, c := range cols {
			forms = append(forms, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'text' THEN hex(%[1]s) ELSE %[1]s END", c))
		}
		fmt.Fprintf(&script, "SELECT '%s', %s FROM %s;\n", names[k], strings.Join(forms, ", "), strings.SplitN(q, " FROM ", 2)[1])
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())
	theirChanges := map[int]string{}
	theirs := map[string][]string{}
	for _, f := range lines {
		if f[0] == "C" {
			i, _ := strconv.Atoi(f[1])
			theirChanges[i] = f[2]
			continue
		}
		theirs[f[0]] = append(theirs[f[0]], strings.Join(f[1:], " "))
	}

	ss := newSession(t, store.Options{})
	failed, updated, nothing := 0, 0, 0
	for i, s := range steps {
		res, err := ss.exec(s)
		ss.commit()
		// A statement is line 2i+1 of the script, its changes() line 2i+2.
		msg, theyFailed := sqliteErrs[2*i+1]
		if theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s, msg, err)
			continue
		}
		if err != nil {
			failed++
			continue
		}
		if got := strconv.FormatInt(res.RowsAffected, 10); got != theirChanges[i] {
			t.Errorf("step %d: %s\n SQLite changed %s rows, here %s", i, s, theirChanges[i], got)
		}
		if strings.Contains(s, "DO UPDATE") {
			updated++
		} else if strings.Contains(s, "DO NOTHING") {
			nothing++
		}
	}
	rows := 0
	for k, q := range queries {
		ours, err := runForOracle(ss, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows += len(ours)
		if strings.Join(ours, "\n") != strings.Join(theirs[names[k]], "\n") {
			t.Errorf("table %s:\n SQLite:\n  %s\n here:\n  %s", names[k], strings.Join(theirs[names[k]], "\n  "), strings.Join(ours, "\n  "))
		}
	}
	t.Logf("%d statements: %d failed in both, %d DO UPDATE and %d DO NOTHING ran, %d rows", len(steps), failed, updated, nothing, rows)
	if failed < 20 || updated < 60 || nothing < 30 || rows < 10 {
		t.Errorf("a weak script: %d failed, %d DO UPDATE, %d DO NOTHING, %d rows", failed, updated, nothing, rows)
	}
}
