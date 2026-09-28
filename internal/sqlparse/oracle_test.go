package sqlparse

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// sqlite3 returns the path of the sqlite3 program, the test oracle of
// P-4. In CI a missing oracle fails the test; elsewhere it skips.
func sqlite3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sqlite3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("sqlite3 not found; CI must have the oracle (P-4)")
		}
		t.Skip("sqlite3 not found")
	}
	return path
}

// randomExpr writes an expression over small integers and NULL, with
// every operator of the parser, and parentheses only now and then.
func randomExpr(r *rand.Rand, depth int) string {
	var b strings.Builder
	term := func() {
		if depth > 0 && r.Intn(5) == 0 {
			b.WriteString("(" + randomExpr(r, depth-1) + ")")
			return
		}
		switch r.Intn(8) {
		case 0:
			b.WriteString("- ")
		case 1:
			b.WriteString("+")
		case 2:
			b.WriteString("-")
		}
		if r.Intn(10) == 0 {
			b.WriteString("NULL")
		} else {
			b.WriteString(strconv.Itoa(r.Intn(10)))
		}
	}
	if r.Intn(6) == 0 {
		b.WriteString("NOT ")
	}
	term()
	ops := []string{"+", "-", "*", "/", "%", "||", "=", "==", "!=", "<>", "<", "<=", ">", ">=",
		"AND", "OR", "IS", "IS NOT", "LIKE", "NOT LIKE", "AND NOT", "OR NOT"}
	for n := r.Intn(5); n > 0; n-- {
		switch r.Intn(12) {
		case 0:
			b.WriteString([]string{" BETWEEN ", " NOT BETWEEN "}[r.Intn(2)])
			term()
			b.WriteString(" AND ")
			term()
		case 1:
			b.WriteString([]string{" IN (", " NOT IN ("}[r.Intn(2)])
			term()
			b.WriteString(", ")
			term()
			b.WriteString(")")
		default:
			b.WriteString(" " + ops[r.Intn(len(ops))] + " ")
			term()
		}
	}
	return b.String()
}

// TestPrecedenceAgainstSQLite: SQLite computes each random expression
// twice, as written and as printed by this package, where every operator
// has parentheses. If this parser groups the operators as SQLite does,
// both give the same value.
func TestPrecedenceAgainstSQLite(t *testing.T) {
	bin := sqlite3(t)
	r := rand.New(rand.NewSource(16))
	type pair struct{ src, printed string }
	var cases []pair
	rejected := 0
	for len(cases) < 3000 {
		src := randomExpr(r, 3)
		s, err := Parse("SELECT " + src)
		if err != nil {
			rejected++
			continue
		}
		cases = append(cases, pair{src, s.(*Select).Items[0].Expr.String()})
	}
	// Positive controls: a wrong grouping must show as a difference.
	controls := []pair{{"1 + 2 * 3", "((1 + 2) * 3)"}, {"NOT 1 = 2", "((NOT 1) = 2)"}, {"1 OR 0 AND 0", "((1 OR 0) AND 0)"}}
	var script strings.Builder
	for i, c := range append(cases, controls...) {
		fmt.Fprintf(&script, "SELECT %d, quote(%s) IS quote(%s), quote(%s), quote(%s);\n", i, c.src, c.printed, c.src, c.printed)
	}
	cmd := exec.Command(bin, "-batch", "-separator", "\t", ":memory:")
	cmd.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("sqlite3: %v\n%s", err, stderr.String())
	}
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		i, err := strconv.Atoi(f[0])
		if err != nil || len(f) != 4 || i != seen {
			t.Fatalf("sqlite3 output line %d: %q", seen, line)
		}
		seen++
		same := f[1] == "1"
		if i >= len(cases) {
			if same {
				t.Errorf("control %s: SQLite gives the same value for %s; the comparison proves nothing", controls[i-len(cases)].src, controls[i-len(cases)].printed)
			}
			continue
		}
		if !same {
			t.Errorf("%s\n printed as %s\n SQLite: %s, then %s", cases[i].src, cases[i].printed, f[2], f[3])
		}
	}
	if seen != len(cases)+len(controls) {
		t.Fatalf("sqlite3 answered %d of %d statements", seen, len(cases)+len(controls))
	}
	t.Logf("%d expressions compared, %d controls, %d rejected by this parser", len(cases), len(controls), rejected)
}
