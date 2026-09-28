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

// aggQuery writes one SELECT with GROUP BY or aggregates.
func (g queries) aggQuery(tb tableInfo) (src string, names []string, ordered bool) {
	w := g.w
	pick := func(types string) string {
		for {
			if e := g.expr(tb, types[w.r.Intn(len(types))]); e != "" {
				return e
			}
		}
	}
	var keys []string
	for n := w.r.Intn(3); len(keys) < n; {
		keys = append(keys, pick("iftbx"))
	}
	var items []string
	for _, k := range keys {
		if w.r.Intn(4) > 0 {
			items = append(items, k)
		}
	}
	for n := 1 + w.r.Intn(3); n > 0; n-- {
		d := ""
		if w.r.Intn(4) == 0 {
			d = "DISTINCT "
		}
		switch w.r.Intn(6) {
		case 0:
			items = append(items, "count(*)")
		case 1:
			items = append(items, "count("+d+pick("iftbx")+")")
		case 2:
			items = append(items, "sum("+d+pick("if")+")")
		case 3:
			items = append(items, "avg("+d+pick("if")+")")
		case 4:
			items = append(items, w.one("min(", "max(")+pick("iftbx")+")")
		default:
			items = append(items, "count(*) * 2 + "+w.intLit())
		}
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		names = append(names, "c"+strconv.Itoa(i+1))
		b.WriteString(it + " AS " + names[i])
	}
	b.WriteString(" FROM " + tb.name)
	if w.r.Intn(3) == 0 {
		b.WriteString(" WHERE " + w.cond(tb.name))
	}
	if len(keys) > 0 {
		b.WriteString(" GROUP BY " + strings.Join(keys, ", "))
	}
	if w.r.Intn(3) == 0 {
		b.WriteString(" HAVING " + w.one("count(*) > "+strconv.Itoa(w.r.Intn(4)), "sum("+pick("i")+") > "+w.intLit(), "min("+pick("t")+") IS NOT NULL"))
	}
	if w.r.Intn(2) == 0 {
		// Rows that are the same in all columns compare as equal, so the
		// order over all columns is total for the comparison.
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
		if w.r.Intn(3) == 0 {
			fmt.Fprintf(&b, " LIMIT %d", w.r.Intn(4))
		}
	}
	return b.String(), names, ordered
}

// qualify returns the columns of a table under a name, as name.column.
func qualify(tb tableInfo, name string) tableInfo {
	q := tableInfo{name: name, cols: map[byte][]string{}, order: tb.order}
	for typ, cs := range tb.cols {
		for _, c := range cs {
			q.cols[typ] = append(q.cols[typ], name+"."+c)
		}
	}
	for _, c := range tb.all {
		q.all = append(q.all, name+"."+c)
	}
	return q
}

// joins are the shapes of the joins of the test. Each has the text after
// FROM, the tables under their names, and whether WHERE can name columns
// without a table. The column names of t1, t2 and t3 differ, so it can,
// except in a join of a table with itself.
var joins = []struct {
	from     string
	tables   []tableInfo
	bareCond bool
}{
	{"t1 JOIN t2 ON t2.n = t1.a", []tableInfo{qualify(queryTables[0], "t1"), qualify(queryTables[1], "t2")}, true},
	{"t1 LEFT JOIN t2 ON t2.n = t1.a AND t2.w > 0.5", []tableInfo{qualify(queryTables[0], "t1"), qualify(queryTables[1], "t2")}, true},
	{"t2 LEFT JOIN t1 ON t1.id = t2.n", []tableInfo{qualify(queryTables[1], "t2"), qualify(queryTables[0], "t1")}, true},
	{"t1 JOIN t3 ON t3.x = t1.a", []tableInfo{qualify(queryTables[0], "t1"), qualify(queryTables[2], "t3")}, true},
	{"t3 LEFT JOIN t1 ON t1.b = t3.y", []tableInfo{qualify(queryTables[2], "t3"), qualify(queryTables[0], "t1")}, true},
	{"t1 JOIN t2 ON t2.n = t1.a LEFT JOIN t3 ON t3.x = t2.n", []tableInfo{qualify(queryTables[0], "t1"), qualify(queryTables[1], "t2"), qualify(queryTables[2], "t3")}, true},
	{"t2 JOIN t1 ON t1.a >= t2.n AND t1.a < t2.n + 3", []tableInfo{qualify(queryTables[1], "t2"), qualify(queryTables[0], "t1")}, true},
	{"t1 AS p LEFT JOIN t1 AS q ON q.id = p.a", []tableInfo{qualify(queryTables[0], "p"), qualify(queryTables[0], "q")}, false},
}

// joinQuery writes one SELECT over a join.
func (g queries) joinQuery() (src string, names []string, ordered bool) {
	w := g.w
	j := joins[w.r.Intn(len(joins))]
	var items []string
	for n := 1 + w.r.Intn(4); len(items) < n; {
		if e := g.expr(j.tables[w.r.Intn(len(j.tables))], "iftbx"[w.r.Intn(5)]); e != "" {
			items = append(items, e)
		}
	}
	grouped := w.r.Intn(4) == 0
	if grouped {
		items = items[:1]
		arg := ""
		for arg == "" {
			arg = g.expr(j.tables[len(j.tables)-1], "iftbx"[w.r.Intn(5)])
		}
		items = append(items, "count(*)", "count("+arg+")")
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		names = append(names, "c"+strconv.Itoa(i+1))
		b.WriteString(it + " AS " + names[i])
	}
	b.WriteString(" FROM " + j.from)
	if j.bareCond && w.r.Intn(2) == 0 {
		b.WriteString(" WHERE " + w.cond(j.tables[w.r.Intn(len(j.tables))].name))
	}
	if grouped {
		b.WriteString(" GROUP BY " + items[0])
	}
	if w.r.Intn(2) == 0 {
		var ns []string
		for i := range names {
			ns = append(ns, strconv.Itoa(i+1))
		}
		b.WriteString(" ORDER BY " + strings.Join(ns, ", "))
		ordered = true
		if w.r.Intn(3) == 0 {
			fmt.Fprintf(&b, " LIMIT %d", w.r.Intn(6))
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
		tb := queryTables[w.r.Intn(len(queryTables))]
		var src string
		var names []string
		var ordered bool
		switch w.r.Intn(4) {
		case 0:
			src, names, ordered = g.aggQuery(tb)
		case 1:
			src, names, ordered = g.joinQuery()
		default:
			src, names, ordered = g.query(tb)
		}
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
	// run runs a query here, with the plan or without.
	run := func(src string, lim Limits) ([]string, string) {
		st, err := sqlparse.Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		pq, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), lim)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		rows, err := pq.Run(ss.tx, nil)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		var out []string
		for rows.Next() {
			var fields []string
			for _, v := range rows.Row() {
				class, value := oracleForm(v)
				fields = append(fields, class, value)
			}
			out = append(out, strings.Join(fields, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return out, pq.Plan()
	}
	rowsSeen, ordered := 0, 0
	plans := map[string]int{}
	for i, x := range qs {
		ours, plan := run(x.src, Limits{})
		plain, _ := run(x.src, Limits{NoIndex: true})
		plans[strings.Fields(plan)[0]]++
		if strings.Contains(plan, "GROUP") {
			plans["GROUP"]++
		}
		if strings.Count(plan, "SCAN")+strings.Count(plan, "SEARCH") > 1 {
			plans["JOIN"]++
		}
		their := append([]string(nil), theirs[i]...)
		if !x.ordered {
			sort.Strings(ours)
			sort.Strings(plain)
			sort.Strings(their)
		} else {
			ordered++
		}
		if strings.Join(ours, "\n") != strings.Join(their, "\n") {
			t.Errorf("%s\n plan %s\n SQLite:\n  %s\n here:\n  %s", x.src, plan, strings.Join(their, "\n  "), strings.Join(ours, "\n  "))
		}
		if strings.Join(ours, "\n") != strings.Join(plain, "\n") {
			t.Errorf("%s\n plan %s gives other rows than a full scan", x.src, plan)
		}
		rowsSeen += len(ours)
	}
	t.Logf("%d queries, %d in total order, %d rows compared; plans %v", len(qs), ordered, rowsSeen, plans)
	// A third of the queries group rows and have WHERE less often, so
	// one in twenty is the floor.
	if plans["SEARCH"] < len(qs)/20 || plans["GROUP"] < len(qs)/8 || plans["JOIN"] < len(qs)/8 {
		t.Errorf("of %d queries, %d used a search, %d grouped rows and %d joined tables; the test does not reach them all", len(qs), plans["SEARCH"], plans["GROUP"], plans["JOIN"])
	}
	if rowsSeen < 2000 {
		t.Errorf("only %d rows compared", rowsSeen)
	}
}
