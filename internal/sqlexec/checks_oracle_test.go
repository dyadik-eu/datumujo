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

// TestChecksAgainstSQLite runs a random script of tables with checks in
// SQLite and here (L-14, P-4). Checks sit on columns and on tables, some
// read two columns. INSERT and UPDATE write values that make some checks
// FALSE and some NULL. ALTER TABLE adds columns with a default and a
// check, which can be FALSE for the rows that exist. Each statement must
// fail in both or in neither, and every table must hold the same rows at
// the end. Each check is a BOOLEAN of its columns: one that is not is an
// error here and not in SQLite, and TestCheckErrors covers it.
func TestChecksAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{29, 1029} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { checksAgainstSQLite(t, seed) })
	}
}

func checksAgainstSQLite(t *testing.T, seed int64) {
	r := rand.New(rand.NewSource(seed))
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	value := func(typ string) string {
		if r.Intn(6) == 0 {
			return "NULL"
		}
		switch typ {
		case "INTEGER":
			return strconv.Itoa(r.Intn(21) - 5)
		case "REAL":
			return pick("-1.5", "0.0", "2.5", "7.25")
		case "TEXT":
			return pick("''", "'a'", "'bb'", "'ccc'", "'dddd'")
		}
		return pick("TRUE", "FALSE")
	}
	// check returns a condition on column c of type typ.
	check := func(c, typ string) string {
		switch typ {
		case "INTEGER":
			return pick(c+" > -2", c+" <> 3", c+" BETWEEN -5 AND 12", c+" % 2 = 0 OR "+c+" > 9")
		case "REAL":
			return pick(c+" < 5.0", c+" >= -1.0", c+" <> 0.0")
		case "TEXT":
			return pick("length("+c+") < 3", c+" <> 'bb'", c+" LIKE '_%'")
		}
		return pick(c+" = TRUE", "NOT "+c, c+" IS NOT NULL")
	}
	types := []string{"INTEGER", "INTEGER", "REAL", "TEXT", "BOOLEAN"}
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
		for j := 0; j < 3+r.Intn(2); j++ {
			c := col{fmt.Sprintf("c%d", j), types[r.Intn(len(types))]}
			def := c.name + " " + c.typ
			if r.Intn(2) == 0 {
				def += " CHECK (" + check(c.name, c.typ) + ")"
			}
			cols = append(cols, c)
			defs = append(defs, def)
		}
		// A check of the table over two INTEGER columns, if there are.
		var ints []string
		for _, c := range cols {
			if c.typ == "INTEGER" {
				ints = append(ints, c.name)
			}
		}
		if len(ints) >= 2 {
			defs = append(defs, fmt.Sprintf("CHECK (%s <= %s + 8)", ints[0], ints[1]))
		}
		steps = append(steps, "CREATE TABLE "+name+" ("+strings.Join(defs, ", ")+")")
		tables[name] = cols
		names = append(names, name)
	}
	// The script runs here as it grows, so that the generator knows which
	// ALTER TABLE added its column. SQLite runs it after, and must agree
	// on every step.
	ss := newSession(t, store.Options{})
	var errs []error
	run := func(s string) error {
		_, err := ss.exec(s)
		ss.commit()
		errs = append(errs, err)
		return err
	}
	for _, s := range steps {
		run(s)
	}
	added := 0
	for len(steps) < 300 {
		name := names[r.Intn(len(names))]
		cols := tables[name]
		switch n := r.Intn(20); {
		case n == 0 && added < 15:
			// The default can make the new check FALSE for the rows that
			// exist; then the statement fails in both, and the table stays.
			c := col{fmt.Sprintf("a%d", added), types[r.Intn(len(types))]}
			added++
			// A NULL default passes each check, so it is not drawn here.
			def := value(c.typ)
			for def == "NULL" {
				def = value(c.typ)
			}
			s := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s DEFAULT %s CHECK (%s)", name, c.name, c.typ, def, check(c.name, c.typ))
			steps = append(steps, s)
			if run(s) == nil {
				tables[name] = append(cols, c)
			}
		case n < 5:
			c := cols[r.Intn(len(cols))]
			s := fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s %% 3 = %d", name, c.name, value(c.typ), keyOf[name], r.Intn(3))
			steps = append(steps, s)
			run(s)
		default:
			var named, vals []string
			for _, c := range cols {
				if r.Intn(3) > 0 {
					named = append(named, c.name)
					vals = append(vals, value(c.typ))
				}
			}
			if len(named) == 0 {
				named, vals = []string{cols[0].name}, []string{value(cols[0].typ)}
			}
			s := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", name, strings.Join(named, ", "), strings.Join(vals, ", "))
			steps = append(steps, s)
			run(s)
		}
	}

	var script strings.Builder
	for _, s := range steps {
		script.WriteString(s + ";\n")
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())
	if len(lines) != 0 {
		t.Fatalf("sqlite3 printed rows for a script without a query: %q", lines)
	}

	failed, failedAlter := 0, 0
	for i, s := range steps {
		err := errs[i]
		if _, theyFailed := sqliteErrs[i+1]; theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s, sqliteErrs[i+1], err)
		}
		if err != nil {
			failed++
			if strings.HasPrefix(s, "ALTER") {
				failedAlter++
			}
		}
	}

	// The rows at the end. A column that ALTER TABLE could not add is in
	// neither table, and the query leaves it out.
	rows := 0
	var query strings.Builder
	var queries []string
	for _, name := range names {
		tb, _ := ss.tx.Schema().Table(name)
		have := map[string]bool{}
		for _, c := range tb.Columns {
			have[c.Name] = true
		}
		key := keyOf[name]
		sel := []string{key}
		for _, c := range tables[name] {
			if have[c.name] {
				sel = append(sel, c.name)
			}
		}
		q := "SELECT " + strings.Join(sel, ", ") + " FROM " + name + " ORDER BY " + key
		queries = append(queries, q)
		var forms []string
		for _, c := range sel {
			forms = append(forms, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'real' THEN hex(ieee754_to_blob(%[1]s)) WHEN 'text' THEN hex(%[1]s) ELSE %[1]s END", c))
		}
		fmt.Fprintf(&query, "SELECT '%s', %s FROM %s ORDER BY %s;\n", name, strings.Join(forms, ", "), name, key)
	}
	// The script again, with the queries after it. Its errors are the
	// ones above.
	theirLines, again := sqlitetest.RunErrors(t, script.String()+query.String())
	if len(again) != len(sqliteErrs) {
		t.Fatalf("sqlite3 gave %d errors, then %d for the same script", len(sqliteErrs), len(again))
	}
	theirs := map[string][]string{}
	for _, f := range theirLines {
		theirs[f[0]] = append(theirs[f[0]], strings.Join(f[1:], " "))
	}
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
	t.Logf("%d statements, %d failed in both, %d of %d ALTER TABLE, %d rows", len(steps), failed, failedAlter, added, rows)
	// Each way a statement can end must come up often enough to count.
	// These are a failed write, a successful write, and an ALTER TABLE
	// that a row that exists makes fail.
	if failed < 50 || len(steps)-failed < 100 || failedAlter == 0 || rows < 60 {
		t.Errorf("a weak script: %d failed, %d passed, %d ALTER TABLE failed, %d rows", failed, len(steps)-failed, failedAlter, rows)
	}
}
