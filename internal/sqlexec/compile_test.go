package sqlexec

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// TestCompileIsLinear compiles statements of n and of 2n terms (L-21).
// The parser reads such a chain in a loop, so MaxDepth does not bound
// it.
//
// The test counts the bytes that the parse and the compile allocate. A
// count of bytes does not change with the load of the machine, and a
// time does. For a linear compile, 2n terms allocate about twice the
// bytes of n terms, and for a quadratic one about four times. The test
// fails above three times, and it logs the times too.
func TestCompileIsLinear(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER, b TEXT)")
	const n = 1000
	for _, tc := range []struct{ name, src, term, op string }{
		{"literals", "SELECT %s", "1", " + "},
		{"columns", "SELECT %s FROM t", "a", " + "},
		{"parameters", "SELECT %s", "?1", " + "},
		{"subqueries", "SELECT %s", "(SELECT 1)", " + "},
		{"conditions", "SELECT a FROM t WHERE %s", "a > 0", " AND "},
		{"texts", "SELECT %s FROM t", "b", " || "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := func(k int) string {
				return strings.Replace(tc.src, "%s", strings.TrimSuffix(strings.Repeat(tc.term+tc.op, k), tc.op), 1)
			}
			compile := func(k int) (uint64, time.Duration) {
				text := src(k)
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				start := time.Now()
				st, err := sqlparse.Parse(text)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), ss.lim); err != nil {
					t.Fatal(err)
				}
				took := time.Since(start)
				runtime.ReadMemStats(&after)
				return after.TotalAlloc - before.TotalAlloc, took
			}
			compile(n) // the first run fills caches that later runs reuse
			b1, d1 := compile(n)
			b2, d2 := compile(2 * n)
			ratio := float64(b2) / float64(b1)
			t.Logf("%d terms: %d bytes, %v; %d terms: %d bytes, %v; ratio of bytes %.2f", n, b1, d1, 2*n, b2, d2, ratio)
			if ratio > 3 {
				t.Errorf("2n terms allocate %.2f times the bytes of n terms; a linear compile gives about 2", ratio)
			}
		})
	}
}
