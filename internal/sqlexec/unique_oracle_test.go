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

// TestUniqueAgainstSQLite runs a random script of tables with UNIQUE in
// SQLite and here (L-15, P-4). UNIQUE sits on columns and on pairs of
// columns. The values come from small sets, so that INSERT and UPDATE
// often hit a value a row has already. Some values are NULL, which never
// conflicts. DELETE frees values again. Each statement must fail in both
// or in neither, and every table must hold the same rows at the end.
func TestUniqueAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{30, 1030} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { uniqueAgainstSQLite(t, seed) })
	}
}

func uniqueAgainstSQLite(t *testing.T, seed int64) {
	r := rand.New(rand.NewSource(seed))
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	value := func(typ string) string {
		if r.Intn(8) == 0 {
			return "NULL"
		}
		if typ == "INTEGER" {
			return strconv.Itoa(r.Intn(6))
		}
		return pick("'a'", "'b'", "'c'", "'d'", "'e'", "'f'")
	}
	type col struct{ name, typ string }

	var steps []string
	tables := map[string][]col{}
	keyOf := map[string]string{}
	var names []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("t%d", i)
		var defs []string
		var cols []col
		keyOf[name] = "rowid"
		if r.Intn(2) == 0 {
			defs = append(defs, "id INTEGER PRIMARY KEY")
			keyOf[name] = "id"
		}
		// A UNIQUE over a pair holds c0 and c1, and neither has a UNIQUE
		// of its own. Over a column that is unique alone, the pair would
		// add no rule, and the test could not see it.
		pair := r.Intn(3) > 0
		for j := 0; j < 2+r.Intn(3); j++ {
			c := col{fmt.Sprintf("c%d", j), pick("INTEGER", "TEXT")}
			def := c.name + " " + c.typ
			if !(pair && j < 2) && r.Intn(2) == 0 {
				def += " UNIQUE"
			}
			cols = append(cols, c)
			defs = append(defs, def)
		}
		if pair {
			defs = append(defs, "UNIQUE ("+pick("c0, c1", "c1, c0")+")")
		}
		steps = append(steps, "CREATE TABLE "+name+" ("+strings.Join(defs, ", ")+")")
		tables[name] = cols
		names = append(names, name)
	}
	for len(steps) < 300 {
		name := names[r.Intn(len(names))]
		cols := tables[name]
		switch n := r.Intn(20); {
		case n < 2:
			steps = append(steps, fmt.Sprintf("DELETE FROM %s WHERE %s %% 4 = %d", name, keyOf[name], r.Intn(4)))
		case n < 6:
			c := cols[r.Intn(len(cols))]
			steps = append(steps, fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s %% 5 = %d", name, c.name, value(c.typ), keyOf[name], r.Intn(5)))
		default:
			var named, vals []string
			for _, c := range cols {
				if r.Intn(4) > 0 {
					named = append(named, c.name)
					vals = append(vals, value(c.typ))
				}
			}
			if len(named) == 0 {
				named, vals = []string{cols[0].name}, []string{value(cols[0].typ)}
			}
			steps = append(steps, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", name, strings.Join(named, ", "), strings.Join(vals, ", ")))
		}
	}

	ss := newSession(t, store.Options{})
	var errs []error
	for _, s := range steps {
		_, err := ss.exec(s)
		ss.commit()
		errs = append(errs, err)
	}

	var script strings.Builder
	for _, s := range steps {
		script.WriteString(s + ";\n")
	}
	var queries []string
	for _, name := range names {
		sel := []string{keyOf[name]}
		for _, c := range tables[name] {
			sel = append(sel, c.name)
		}
		queries = append(queries, "SELECT "+strings.Join(sel, ", ")+" FROM "+name+" ORDER BY "+keyOf[name])
		var forms []string
		for _, c := range sel {
			forms = append(forms, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'text' THEN hex(%[1]s) ELSE %[1]s END", c))
		}
		fmt.Fprintf(&script, "SELECT '%s', %s FROM %s ORDER BY %s;\n", name, strings.Join(forms, ", "), name, keyOf[name])
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())
	theirs := map[string][]string{}
	for _, f := range lines {
		theirs[f[0]] = append(theirs[f[0]], strings.Join(f[1:], " "))
	}

	failed, conflicts := 0, 0
	for i, s := range steps {
		err := errs[i]
		msg, theyFailed := sqliteErrs[i+1]
		if theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s, msg, err)
		}
		if err != nil {
			failed++
			if strings.Contains(msg, "UNIQUE constraint failed") {
				conflicts++
			}
		}
	}
	rows := 0
	for i, name := range names {
		ours, err := runForOracle(ss, queries[i])
		if err != nil {
			t.Fatalf("%s: %v", queries[i], err)
		}
		rows += len(ours)
		if strings.Join(ours, "\n") != strings.Join(theirs[name], "\n") {
			t.Errorf("table %s:\n SQLite:\n  %s\n here:\n  %s", name, strings.Join(theirs[name], "\n  "), strings.Join(ours, "\n  "))
		}
	}
	t.Logf("%d statements, %d failed in both, %d of them on UNIQUE, %d rows", len(steps), failed, conflicts, rows)
	// Each way a statement can end must come up often enough to count.
	if conflicts < 40 || len(steps)-failed < 100 || rows < 20 {
		t.Errorf("a weak script: %d failed, %d on UNIQUE, %d passed, %d rows", failed, conflicts, len(steps)-failed, rows)
	}
}
