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

// TestDefaultsAgainstSQLite runs a random script of tables with defaults
// in SQLite and here (L-13, P-4). The INSERTs name random sets of
// columns, ALTER TABLE adds columns with a default, some NOT NULL, and
// UPDATE changes rows written before and after. Each statement must fail
// in both or in neither, and every table must hold the same rows at the
// end. The defaults fit their columns: a default that does not is an
// error here and a value in SQLite, and TestDefaultErrors covers it.
func TestDefaultsAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{28, 1028} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { defaultsAgainstSQLite(t, seed) })
	}
}

// dcol is a column of the generator: its name, its type, whether it is
// NOT NULL, and whether it has a default.
type dcol struct {
	name, typ    string
	notNull, def bool
}

func defaultsAgainstSQLite(t *testing.T, seed int64) {
	r := rand.New(rand.NewSource(seed))
	constant := func(typ string) string {
		switch typ {
		case "INTEGER":
			return []string{"0", "-3", "42", "-9223372036854775808"}[r.Intn(4)]
		case "REAL":
			// No -0.0: SQLite stores it as 0.0 in a REAL column, datumujo
			// keeps the sign (see the oracle table in requirements.md).
			return []string{"2", "0.5", "1.5", "-1e300"}[r.Intn(4)]
		case "TEXT":
			return []string{"''", "'a''b'", "'ä'"}[r.Intn(3)]
		case "BLOB":
			return []string{"X''", "X'00ff'"}[r.Intn(2)]
		}
		return []string{"TRUE", "FALSE"}[r.Intn(2)]
	}
	types := []string{"INTEGER", "REAL", "TEXT", "BLOB", "BOOLEAN"}
	newCol := func(name string) (dcol, string) {
		c := dcol{name: name, typ: types[r.Intn(len(types))], notNull: r.Intn(3) == 0}
		def := c.name + " " + c.typ
		if c.notNull {
			def += " NOT NULL"
		}
		// Most columns have a default; a NOT NULL one without is an error
		// for an INSERT that does not name it, in both.
		if c.def = r.Intn(4) > 0; c.def {
			def += " DEFAULT " + constant(c.typ)
		}
		return c, def
	}

	var steps []string
	tables := map[string][]dcol{}
	// keyOf names the key: id for a table with an INTEGER PRIMARY KEY,
	// the hidden rowid for one without.
	keyOf := map[string]string{}
	var names []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("t%d", i)
		var defs []string
		var cols []dcol
		keyOf[name] = "rowid"
		if r.Intn(2) == 0 {
			defs = append(defs, "id INTEGER PRIMARY KEY")
			keyOf[name] = "id"
		}
		for j := 0; j < 2+r.Intn(3); j++ {
			c, def := newCol(fmt.Sprintf("c%d", j))
			cols = append(cols, c)
			defs = append(defs, def)
		}
		steps = append(steps, "CREATE TABLE "+name+" ("+strings.Join(defs, ", ")+")")
		tables[name] = cols
		names = append(names, name)
	}
	value := func(typ string) string {
		if r.Intn(5) == 0 {
			return "NULL"
		}
		return constant(typ)
	}
	added := 0
	for len(steps) < 300 {
		name := names[r.Intn(len(names))]
		cols := tables[name]
		switch n := r.Intn(20); {
		case n == 0 && added < 12:
			// An added column that allows NULL may go without a default;
			// the rows that exist read NULL then. One NOT NULL without a
			// default fails in both, and the table stays as it was.
			c, def := newCol(fmt.Sprintf("a%d", added))
			added++
			steps = append(steps, "ALTER TABLE "+name+" ADD COLUMN "+def)
			if !c.notNull || c.def {
				tables[name] = append(cols, c)
			}
		case n < 4:
			c := cols[r.Intn(len(cols))]
			steps = append(steps, fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s %% 3 = %d", name, c.name, value(c.typ), keyOf[name], r.Intn(3)))
		default:
			var named, vals []string
			for _, c := range cols {
				if r.Intn(2) == 0 {
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

	// A table that SQLite rejects is not in the script of either; ALTER
	// TABLE can fail in both, and so can an INSERT. The rows at the end,
	// with the key, are the result.
	var script strings.Builder
	for _, s := range steps {
		script.WriteString(s + ";\n")
	}
	for _, name := range names {
		var cols []string
		for _, c := range append([]dcol{{name: "rowid"}}, tables[name]...) {
			cols = append(cols, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'real' THEN hex(ieee754_to_blob(%[1]s)) WHEN 'text' THEN hex(%[1]s) WHEN 'blob' THEN hex(%[1]s) ELSE %[1]s END", c.name))
		}
		fmt.Fprintf(&script, "SELECT '%s', %s FROM %s ORDER BY rowid;\n", name, strings.Join(cols, ", "), name)
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())
	theirs := map[string][]string{}
	for _, f := range lines {
		theirs[f[0]] = append(theirs[f[0]], strings.Join(f[1:], " "))
	}

	ss := newSession(t, store.Options{})
	failed := 0
	for i, s := range steps {
		_, err := ss.exec(s)
		if _, theyFailed := sqliteErrs[i+1]; theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s, sqliteErrs[i+1], err)
		}
		if err != nil {
			failed++
		}
		ss.commit()
	}
	rows := 0
	for _, name := range names {
		// SQLite reads rowid and the columns of the generator. rowid is
		// id where there is one; here a table with an INTEGER key has no
		// rowid, so id stands for it.
		key := keyOf[name]
		sel := []string{key}
		for _, c := range tables[name] {
			sel = append(sel, c.name)
		}
		q := "SELECT " + strings.Join(sel, ", ") + " FROM " + name + " ORDER BY " + key
		ours, err := runForOracle(ss, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows += len(ours)
		if strings.Join(ours, "\n") != strings.Join(theirs[name], "\n") {
			t.Errorf("table %s:\n SQLite:\n  %s\n here:\n  %s", name, strings.Join(theirs[name], "\n  "), strings.Join(ours, "\n  "))
		}
	}
	t.Logf("%d statements, %d failed in both, %d added columns, %d rows", len(steps), failed, added, rows)
	if failed == 0 || failed > len(steps)/3 || rows < 100 || added < 5 {
		t.Errorf("a weak script: %d failed, %d rows, %d added columns", failed, rows, added)
	}
}
