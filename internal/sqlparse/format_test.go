package sqlparse

import (
	"runtime"
	"strings"
	"testing"
)

// TestStringIsLinear prints expressions of n and of 2n terms. The
// parser builds a chain of terms as a tree that is n levels deep. A
// print that joins the text of each level anew costs time in n squared.
// The test counts the bytes that String allocates, which do not change
// with the load of the machine. For a linear print, 2n terms allocate
// about twice the bytes of n terms. The test fails above three times.
func TestStringIsLinear(t *testing.T) {
	const n = 1000
	for _, tc := range []struct{ name, term, op string }{
		{"sum", "1", " + "},
		{"and", "a > 0", " AND "},
		{"concat", "b", " || "},
		{"calls", "abs(a - 1)", " * "},
		{"between", "a BETWEEN 1 AND 2", " OR "},
		{"equal", "a", " = "},
		{"less", "a", " < "},
		{"subqueries", "(SELECT x FROM t WHERE y = 1)", " - "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			print := func(k int) (string, uint64) {
				src := "SELECT " + strings.TrimSuffix(strings.Repeat(tc.term+tc.op, k), tc.op)
				st, err := Parse(src)
				if err != nil {
					t.Fatal(err)
				}
				e := st.(*Select).Items[0].Expr
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				text := e.String()
				runtime.ReadMemStats(&after)
				return text, after.TotalAlloc - before.TotalAlloc
			}
			text, b1 := print(n)
			_, b2 := print(2 * n)
			ratio := float64(b2) / float64(b1)
			t.Logf("%d terms: %d bytes of text, %d allocated; %d terms: %d allocated; ratio %.2f", n, len(text), b1, 2*n, b2, ratio)
			if ratio > 3 {
				t.Errorf("2n terms allocate %.2f times the bytes of n terms; a linear print gives about 2", ratio)
			}
			// The print must still parse to the same tree.
			st, err := Parse("SELECT " + text)
			if err != nil {
				t.Fatal(err)
			}
			if again := st.(*Select).Items[0].Expr.String(); again != text {
				t.Errorf("the print does not parse to the same tree")
			}
		})
	}
}
