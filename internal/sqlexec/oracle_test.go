package sqlexec

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/sqlparse"
)

// pair is an expression twice: as this package runs it, and as SQLite
// runs it. The two differ only where the requirements name a difference
// to SQLite, and the SQLite text makes up for it.
type pair struct{ o, s string }

// cat joins strings and pairs into one pair.
func cat(parts ...any) pair {
	var o, s strings.Builder
	for _, p := range parts {
		switch x := p.(type) {
		case string:
			o.WriteString(x)
			s.WriteString(x)
		case pair:
			o.WriteString(x.o)
			s.WriteString(x.s)
		}
	}
	return pair{o.String(), s.String()}
}

func lit(x string) pair { return pair{x, x} }

// gen writes random expressions of a given type that this package
// accepts. Each fits the rules of L-3, so SQLite and this package must
// give the same value, except where the requirements name a difference.
type gen struct{ r *rand.Rand }

func (g gen) pick(opts ...func() pair) pair { return opts[g.r.Intn(len(opts))]() }

func (g gen) one(xs ...string) string { return xs[g.r.Intn(len(xs))] }

func (g gen) null(f func() pair) pair {
	if g.r.Intn(12) == 0 {
		return lit("NULL")
	}
	return f()
}

func (g gen) intLit() pair { return lit(strconv.Itoa(g.r.Intn(41) - 20)) }

func (g gen) realLit() pair {
	return lit(g.one("1.5", "-0.25", "3.0", "1e3", "0.1", "2.675", "-7.5", "0.125", "1.005", "100.0", "-0.0"))
}

func (g gen) textLit() pair {
	chars := []string{"a", "B", "ä", "%", "_", " ", "x", "b", "A"}
	var b strings.Builder
	for n := g.r.Intn(5); n > 0; n-- {
		b.WriteString(chars[g.r.Intn(len(chars))])
	}
	return lit(sqlparse.Quote(b.String()))
}

func (g gen) blobLit() pair {
	var b strings.Builder
	for n := g.r.Intn(4); n > 0; n-- {
		b.WriteString(g.one("00", "01", "61", "ff"))
	}
	return lit("X'" + b.String() + "'")
}

func (g gen) i(d int) pair {
	if d <= 0 {
		return g.null(g.intLit)
	}
	i, r, b, t, x := func() pair { return g.i(d - 1) }, func() pair { return g.f(d - 1) }, func() pair { return g.b(d - 1) }, func() pair { return g.t(d - 1) }, func() pair { return g.x(d - 1) }
	return g.null(func() pair {
		return g.pick(
			g.intLit,
			func() pair { return cat("(", i(), " ", g.one("+", "-", "*", "/", "%"), " ", i(), ")") },
			func() pair { return cat("-(", i(), ")") },
			func() pair { return cat("abs(", i(), ")") },
			func() pair { return cat("length(", t(), ")") },
			func() pair { return cat("length(", x(), ")") },
			func() pair { return cat("instr(", t(), ", ", t(), ")") },
			func() pair { return cat("CASE WHEN ", b(), " THEN ", i(), " ELSE ", i(), " END") },
			func() pair { return cat("CASE ", i(), " WHEN ", i(), " THEN ", i(), " END") },
			func() pair { return cat("coalesce(", i(), ", ", i(), ")") },
			func() pair { return cat("nullif(", i(), ", ", i(), ")") },
			func() pair { return cat("min(", i(), ", ", i(), ")") },
			func() pair { return cat("max(", i(), ", ", i(), ", ", i(), ")") },
			func() pair { return cat("CAST(", r(), " AS INTEGER)") },
			func() pair { return cat("CAST(", b(), " AS INTEGER)") },
			func() pair { return cat("CAST('", g.one(" 12 ", "-5", "+7", "0"), "' AS INTEGER)") },
		)
	})
}

func (g gen) f(d int) pair {
	if d <= 0 {
		return g.null(g.realLit)
	}
	i, r, b := func() pair { return g.i(d - 1) }, func() pair { return g.f(d - 1) }, func() pair { return g.b(d - 1) }
	return g.null(func() pair {
		return g.pick(
			g.realLit,
			func() pair { return cat("(", r(), " ", g.one("+", "-", "*", "/"), " ", r(), ")") },
			func() pair { return cat("(", i(), " ", g.one("+", "-", "*", "/"), " ", r(), ")") },
			func() pair { return cat("(", r(), " / ", i(), ")") },
			func() pair { return cat("-(", r(), ")") },
			func() pair { return cat("abs(", r(), ")") },
			func() pair { return cat("round(", r(), ")") },
			func() pair { return cat("round(", r(), ", ", strconv.Itoa(g.r.Intn(5)), ")") },
			func() pair { return cat("round(", i(), ")") },
			func() pair { return cat("CAST(", i(), " AS REAL)") },
			func() pair { return cat("CAST('", g.one("1.5", " 2e2 ", "-0.5", "3"), "' AS REAL)") },
			func() pair { return cat("coalesce(", r(), ", ", r(), ")") },
			func() pair { return cat("CASE WHEN ", b(), " THEN ", r(), " END") },
		)
	})
}

func (g gen) b(d int) pair {
	if d <= 0 {
		return g.null(func() pair { return lit(g.one("TRUE", "FALSE")) })
	}
	i, r, b, t, x := func() pair { return g.i(d - 1) }, func() pair { return g.f(d - 1) }, func() pair { return g.b(d - 1) }, func() pair { return g.t(d - 1) }, func() pair { return g.x(d - 1) }
	cmp := func() string { return g.one("=", "!=", "<", "<=", ">", ">=", "IS", "IS NOT") }
	return g.null(func() pair {
		return g.pick(
			func() pair { return lit("TRUE") },
			func() pair { return cat("(", i(), " ", cmp(), " ", i(), ")") },
			func() pair { return cat("(", i(), " ", cmp(), " ", r(), ")") },
			func() pair { return cat("(", r(), " ", cmp(), " ", r(), ")") },
			func() pair { return cat("(", t(), " ", cmp(), " ", t(), ")") },
			func() pair { return cat("(", x(), " ", cmp(), " ", x(), ")") },
			func() pair { return cat("(", b(), " ", cmp(), " ", b(), ")") },
			func() pair { return cat("(", b(), " ", g.one("AND", "OR"), " ", b(), ")") },
			func() pair { return cat("(NOT ", b(), ")") },
			func() pair { return cat("(", i(), g.one(" BETWEEN ", " NOT BETWEEN "), i(), " AND ", i(), ")") },
			func() pair { return cat("(", i(), g.one(" IN (", " NOT IN ("), i(), ", ", i(), ", ", i(), "))") },
			func() pair { return cat("(", t(), g.one(" LIKE ", " NOT LIKE "), t(), ")") },
			func() pair { return cat("CAST(", g.one("0", "1"), " AS BOOLEAN)") },
		)
	})
}

func (g gen) t(d int) pair {
	if d <= 0 {
		return g.null(g.textLit)
	}
	i, b, t := func() pair { return g.i(d - 1) }, func() pair { return g.b(d - 1) }, func() pair { return g.t(d - 1) }
	return g.null(func() pair {
		return g.pick(
			g.textLit,
			func() pair { return cat("(", t(), " || ", t(), ")") },
			func() pair { return cat("lower(", t(), ")") },
			func() pair { return cat("upper(", t(), ")") },
			func() pair { return cat("substr(", t(), ", ", i(), ")") },
			func() pair { return cat("substr(", t(), ", ", i(), ", ", i(), ")") },
			func() pair { return cat("trim(", t(), ")") },
			func() pair { return cat("ltrim(", t(), ", ", t(), ")") },
			func() pair { return cat("rtrim(", t(), ", ", t(), ")") },
			func() pair { return cat("replace(", t(), ", ", t(), ", ", t(), ")") },
			func() pair { return cat("CAST(", i(), " AS TEXT)") },
			func() pair { return cat("coalesce(", t(), ", ", t(), ")") },
			func() pair { return cat("nullif(", t(), ", ", t(), ")") },
			func() pair { return cat("min(", t(), ", ", t(), ")") },
			func() pair { return cat("CASE WHEN ", b(), " THEN ", t(), " ELSE ", t(), " END") },
		)
	})
}

func (g gen) x(d int) pair {
	if d <= 0 {
		return g.null(g.blobLit)
	}
	i, t, x := func() pair { return g.i(d - 1) }, func() pair { return g.t(d - 1) }, func() pair { return g.x(d - 1) }
	return g.null(func() pair {
		return g.pick(
			g.blobLit,
			func() pair {
				// substr of an empty BLOB is NULL in SQLite and an empty BLOB
				// here. The SQLite text keeps the empty BLOB.
				v, a, n := x(), i(), i()
				return pair{
					o: cat("substr(", v, ", ", a, ", ", n, ")").o,
					s: cat("(SELECT CASE WHEN length(v) = 0 AND a IS NOT NULL AND n IS NOT NULL THEN v ELSE substr(v, a, n) END FROM (SELECT ", v, " AS v, ", a, " AS a, ", n, " AS n))").s,
				}
			},
			func() pair { return cat("CAST(", t(), " AS BLOB)") },
			func() pair { return cat("coalesce(", x(), ", ", x(), ")") },
		)
	})
}

// oracleForm writes a value as the SQLite query of the test prints it:
// the storage class, and the value. A REAL is its bits, TEXT and BLOB
// are hex. BOOLEAN is an integer in SQLite.
func oracleForm(v any) (string, string) {
	switch x := v.(type) {
	case nil:
		return "null", ""
	case int64:
		return "integer", strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "integer", "1"
		}
		return "integer", "0"
	case float64:
		return "real", fmt.Sprintf("%016X", math.Float64bits(x))
	case string:
		return "text", fmt.Sprintf("%X", x)
	case []byte:
		return "blob", fmt.Sprintf("%X", x)
	}
	return "?", fmt.Sprint(v)
}

// deliberate reports an error that the requirements name as a difference
// to SQLite: SQLite gives NULL or converts, this package refuses (L-3).
func deliberate(err error) bool {
	for _, s := range []string{"division by zero", "integer overflow", "REAL overflow", "does not convert"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// TestExpressionsAgainstSQLite runs random expressions of every type in
// this package and in SQLite and compares the values (P-4).
func TestExpressionsAgainstSQLite(t *testing.T) {
	r := rand.New(rand.NewSource(17))
	g := gen{r}
	type item struct{ ours, theirs, kind string }
	var items []item
	for len(items) < 6000 {
		kind := "ifbtxm"[r.Intn(6)]
		var p pair
		switch kind {
		case 'i':
			p = g.i(3)
		case 'f':
			p = g.f(3)
		case 'b':
			p = g.b(3)
		case 't':
			p = g.t(3)
		case 'x':
			p = g.x(3)
		case 'm':
			// INTEGER and REAL mixed: which value comes back matters,
			// so these stand at the top, where the storage class counts.
			p = cat(g.one("min(", "max("), g.i(2), ", ", g.f(2), ", ", g.i(1), ")")
		}
		items = append(items, item{p.o, p.s, string(kind)})
	}
	// Positive controls: SQLite runs other text, and the comparison must
	// see the difference in each kind of value.
	controls := []item{
		{"1 + 1", "1 + 2", "control"}, {"0.1 + 0.2", "0.3", "control"}, {"'a'", "'A'", "control"},
		{"X'00'", "X'0000'", "control"}, {"NULL", "0", "control"}, {"TRUE", "0", "control"},
	}
	type result struct{ class, value string }
	ours := make([]result, 0, len(items)+len(controls))
	var srcs []string
	var script strings.Builder
	skipped := map[string]int{}
	compared := 0
	for _, it := range append(items, controls...) {
		v, err := eval(t, it.ours)
		if err != nil {
			var e *sqlparse.Error
			if !errors.As(err, &e) || !deliberate(err) {
				t.Fatalf("%s: %v", it.ours, err)
			}
			skipped[e.Msg[:strings.IndexAny(e.Msg+":", ":")]]++
			continue
		}
		class, value := oracleForm(v)
		ours = append(ours, result{class, value})
		srcs = append(srcs, it.ours)
		fmt.Fprintf(&script, "SELECT %d, typeof(v), CASE typeof(v) WHEN 'real' THEN hex(ieee754_to_blob(v)) WHEN 'text' THEN hex(v) WHEN 'blob' THEN hex(v) ELSE v END FROM (SELECT %s AS v);\n", len(ours)-1, it.theirs)
	}
	lines := sqlitetest.Run(t, script.String())
	if len(lines) != len(ours) {
		t.Fatalf("sqlite3 answered %d of %d statements", len(lines), len(ours))
	}
	// The items of ours are in the order of items then controls, less the
	// skipped ones; the controls are never skipped.
	nControls := len(controls)
	for n, f := range lines {
		if len(f) != 3 || f[0] != strconv.Itoa(n) {
			t.Fatalf("sqlite3 line %d: %q", n, f)
		}
		same := f[1] == ours[n].class && f[2] == ours[n].value
		if n >= len(ours)-nControls {
			if same {
				t.Errorf("control %d gives the same result in both; the comparison proves nothing", n-(len(ours)-nControls))
			}
			continue
		}
		compared++
		if !same {
			t.Errorf("%s\n SQLite %s %s, this package %s %s", srcs[n], f[1], f[2], ours[n].class, ours[n].value)
		}
	}
	t.Logf("%d expressions compared, %d controls, skipped as named differences: %v", compared, nControls, skipped)
	if compared < 5000 {
		t.Errorf("only %d of 6000 expressions compared", compared)
	}
}
