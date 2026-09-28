package sqlexec

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// tableInfo names the columns of a table of the schema by type, and the
// columns that give the rows a total order.
type tableInfo struct {
	name  string
	cols  map[byte][]string // by type: i, f, t, b, x
	all   []string          // the columns of *
	order string            // the key
}

var queryTables = []tableInfo{
	{"t1", map[byte][]string{'i': {"id", "a"}, 't': {"b"}, 'f': {"c"}, 'b': {"d"}}, []string{"id", "a", "b", "c", "d"}, "id"},
	{"t2", map[byte][]string{'i': {"n"}, 't': {"k"}, 'x': {"v"}, 'f': {"w"}}, []string{"k", "n", "v", "w"}, "k, n"},
	{"t3", map[byte][]string{'i': {"x"}, 't': {"y"}}, []string{"x", "y"}, "rowid"},
}

// queries writes random SELECT statements over the tables.
type queries struct {
	w *workload
}

// expr writes an expression of the type typ over the columns of tb.
func (g queries) expr(tb tableInfo, typ byte) string {
	w := g.w
	col := func(t byte) string {
		cs := tb.cols[t]
		if len(cs) == 0 {
			return ""
		}
		return cs[w.r.Intn(len(cs))]
	}
	c := col(typ)
	if c == "" {
		return ""
	}
	var opts []string
	switch typ {
	case 'i':
		opts = []string{c, c + " + " + w.intLit(), "abs(" + c + ")", "coalesce(" + c + ", " + w.intLit() + ")", "-" + c}
		if t := col('t'); t != "" {
			opts = append(opts, "length("+t+")")
		}
		if b := col('b'); b != "" {
			opts = append(opts, "CASE WHEN "+b+" THEN "+c+" ELSE "+w.intLit()+" END")
		}
	case 'f':
		opts = []string{c, c + " * 2", "round(" + c + ", 1)", c + " + 0.5"}
		if i := col('i'); i != "" {
			opts = append(opts, c+" + "+i)
		}
	case 't':
		opts = []string{c, "upper(" + c + ")", c + " || 'x'", "substr(" + c + ", 1, 1)", "coalesce(" + c + ", 'z')"}
	case 'b':
		opts = []string{c, "NOT " + c}
	case 'x':
		// substr of an empty BLOB is a named difference to SQLite.
		opts = []string{c, "coalesce(" + c + ", X'ee')"}
	}
	return opts[w.r.Intn(len(opts))]
}

// query writes one SELECT and names its columns. ordered reports that the
// order of its rows is total, so a comparison can keep it.
func (g queries) query(tb tableInfo) (src string, names []string, ordered bool) {
	w := g.w
	var items []string
	if w.r.Intn(5) == 0 {
		items = []string{"*"}
		names = tb.all
	} else {
		for n := 1 + w.r.Intn(3); len(items) < n; {
			typ := "iftbx"[w.r.Intn(5)]
			e := g.expr(tb, typ)
			if e == "" {
				continue
			}
			names = append(names, "c"+strconv.Itoa(len(items)+1))
			items = append(items, e+" AS "+names[len(names)-1])
		}
	}
	distinct := w.r.Intn(4) == 0
	var b strings.Builder
	b.WriteString("SELECT ")
	if distinct {
		b.WriteString("DISTINCT ")
	}
	b.WriteString(strings.Join(items, ", ") + " FROM " + tb.name)
	if w.r.Intn(3) > 0 {
		b.WriteString(" WHERE " + w.cond(tb.name))
	}
	switch {
	case distinct:
		// The rows are distinct, so the order of all columns is total.
		var ns []string
		for i := range names {
			n := strconv.Itoa(i + 1)
			if w.r.Intn(2) == 0 {
				n += " DESC"
			}
			ns = append(ns, n)
		}
		b.WriteString(" ORDER BY " + strings.Join(ns, ", "))
		ordered = true
	case w.r.Intn(3) > 0:
		// Terms of every kind, then the key, which makes the order total.
		var terms []string
		for n := w.r.Intn(3); n > 0; n-- {
			var term string
			switch w.r.Intn(3) {
			case 0:
				term = strconv.Itoa(1 + w.r.Intn(len(names)))
			case 1:
				term = names[w.r.Intn(len(names))]
			default:
				term = g.expr(tb, "iftb"[w.r.Intn(4)])
			}
			if term == "" {
				continue
			}
			if w.r.Intn(2) == 0 {
				term += " DESC"
			}
			terms = append(terms, term)
		}
		b.WriteString(" ORDER BY " + strings.Join(append(terms, tb.order), ", "))
		ordered = true
	}
	if ordered && w.r.Intn(3) == 0 {
		fmt.Fprintf(&b, " LIMIT %d", w.r.Intn(6)-1)
		if w.r.Intn(2) == 0 {
			fmt.Fprintf(&b, " OFFSET %d", w.r.Intn(5)-1)
		}
	}
	return b.String(), names, ordered
}

// TestSelectAgainstSQLite fills the tables with a random workload in
// SQLite and here, then runs random queries in both. The rows must be
// the same, in the same order where the query orders them totally, and
// as a multiset where it does not (P-4).
func TestSelectAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{19, 1019} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { selectAgainstSQLite(t, seed) })
	}
}

func selectAgainstSQLite(t *testing.T, seed int64) {
	w := &workload{r: rand.New(rand.NewSource(seed)), altered: true} // no ALTER: the queries know the columns
	stmts := append([]string(nil), schema...)
	for len(stmts) < len(schema)+300 {
		stmts = append(stmts, w.statement())
	}
	// More rows, so each query has some to compare: INSERTs only. A key
	// that exists makes one fail, in both.
	for i := 0; i < 150; i++ {
		switch i % 3 {
		case 0:
			stmts = append(stmts, fmt.Sprintf("INSERT INTO t1 (a, b, c, d) VALUES (%s, %s, %s, %s)", w.maybeNull(w.intLit()), w.maybeNull(w.textLit()+" || '"+strconv.Itoa(i)+"'"), w.realLit(), w.maybeNull(w.boolLit())))
		case 1:
			stmts = append(stmts, fmt.Sprintf("INSERT INTO t2 VALUES (%s, %d, %s, %s)", w.textLit(), w.r.Intn(40), w.maybeNull(w.blobLit()), w.maybeNull(w.realLit())))
		default:
			stmts = append(stmts, fmt.Sprintf("INSERT INTO t3 VALUES (%s, %s)", w.maybeNull(w.intLit()), w.maybeNull(w.textLit())))
		}
	}
	g := queries{w}
	type q struct {
		src     string
		names   []string
		ordered bool
	}
	var qs []q
	for len(qs) < 600 {
		src, names, ordered := g.query(queryTables[w.r.Intn(len(queryTables))])
		qs = append(qs, q{src, names, ordered})
	}

	var script strings.Builder
	for _, s := range stmts {
		script.WriteString(s + ";\n")
	}
	for i, x := range qs {
		var cols []string
		for _, n := range x.names {
			cols = append(cols, fmt.Sprintf("typeof(%s), CASE typeof(%s) WHEN 'real' THEN hex(ieee754_to_blob(%s)) WHEN 'text' THEN hex(%s) WHEN 'blob' THEN hex(%s) ELSE %s END", n, n, n, n, n, n))
		}
		fmt.Fprintf(&script, "SELECT 'Q', %d, %s FROM (%s);\n", i, strings.Join(cols, ", "), x.src)
	}
	lines, errs := sqlitetest.RunErrors(t, script.String())
	for n, e := range errs {
		if n > len(stmts) {
			t.Fatalf("SQLite refused query %d: %s\n%s", n-len(stmts)-1, qs[n-len(stmts)-1].src, e)
		}
	}
	theirs := make([][]string, len(qs))
	for _, f := range lines {
		i, err := strconv.Atoi(f[1])
		if f[0] != "Q" || err != nil {
			t.Fatalf("sqlite3 line %q", f)
		}
		theirs[i] = append(theirs[i], strings.Join(f[2:], " "))
	}

	ss := newSession(t, store.Options{})
	for _, s := range stmts {
		ss.exec(s) // the writes oracle checks these; some fail in both
	}
	rowsSeen, ordered := 0, 0
	for i, x := range qs {
		st, err := sqlparse.Parse(x.src)
		if err != nil {
			t.Fatalf("%s: %v", x.src, err)
		}
		pq, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select))
		if err != nil {
			t.Fatalf("%s: %v", x.src, err)
		}
		rows, err := pq.Run(ss.tx, nil)
		if err != nil {
			t.Fatalf("%s: %v", x.src, err)
		}
		var ours []string
		for rows.Next() {
			var fields []string
			for _, v := range rows.Row() {
				class, value := oracleForm(v)
				fields = append(fields, class, value)
			}
			ours = append(ours, strings.Join(fields, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", x.src, err)
		}
		their := append([]string(nil), theirs[i]...)
		if !x.ordered {
			sort.Strings(ours)
			sort.Strings(their)
		} else {
			ordered++
		}
		if strings.Join(ours, "\n") != strings.Join(their, "\n") {
			t.Errorf("%s\n SQLite:\n  %s\n here:\n  %s", x.src, strings.Join(their, "\n  "), strings.Join(ours, "\n  "))
		}
		rowsSeen += len(ours)
	}
	t.Logf("%d queries, %d in total order, %d rows compared", len(qs), ordered, rowsSeen)
	if rowsSeen < 2000 {
		t.Errorf("only %d rows compared", rowsSeen)
	}
}
