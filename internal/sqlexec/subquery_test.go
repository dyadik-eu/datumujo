package sqlexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
)

// TestSubqueryNotYet: the parser reads subqueries before the engine
// runs them. Until it does, each form fails as not supported. IN
// (SELECT ...) must not run as IN with an empty list, which is FALSE for
// every row.
func TestSubqueryNotYet(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER)")
	ss.must("INSERT INTO t VALUES (1)")
	for _, src := range []string{
		"SELECT a FROM t WHERE a IN (SELECT a FROM t)",
		"SELECT a FROM t WHERE a NOT IN (SELECT a FROM t)",
		"SELECT (SELECT 1)",
		"SELECT EXISTS (SELECT 1)",
	} {
		_, err := ss.query(src)
		if !errors.Is(err, ErrStatement) || !strings.Contains(err.Error(), "roadmap step 31") {
			t.Errorf("%s: %v", src, err)
		}
	}
}
