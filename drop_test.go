package datumujo_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo"
)

var keepDef = datumujo.Def{Name: "keep", Columns: []datumujo.Column{
	{Name: "id", Type: datumujo.Int64},
	{Name: "name", Type: datumujo.String},
}, Key: []string{"id"}}

// fillIssues writes n issues. Every fifth body is longer than a page, so
// the tree has overflow pages too.
func fillIssues(tx *datumujo.Tx, n int) error {
	for i := 0; i < n; i++ {
		body := strings.Repeat("b", 100)
		if i%5 == 0 {
			body = strings.Repeat("o", 6000)
		}
		state := "open"
		if i%3 == 0 {
			state = "closed"
		}
		if err := tx.Insert("issue", datumujo.Row{int64(1), int64(i), state, body}); err != nil {
			return err
		}
	}
	return nil
}

// checkAccounted runs the check. The check must find nothing. Every page
// must be the header, free, or owned by the catalog, a table or an
// index. The check also reports a page that nothing owns.
func checkAccounted(t *testing.T, path string) *datumujo.Report {
	t.Helper()
	r, err := datumujo.Check(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		t.Errorf("finding: %v", f)
	}
	used := uint64(1) + r.Stats.FreePages + uint64(r.Stats.CatalogPages)
	for _, ts := range r.Stats.Tables {
		used += uint64(ts.Pages)
		for _, is := range ts.Indexes {
			used += uint64(is.Pages)
		}
	}
	if used != r.Stats.Pages {
		t.Errorf("pages: %d accounted for, the file has %d", used, r.Stats.Pages)
	}
	return r
}

func TestDropTableAndIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, datumujo.Options{})
	if err := db.Update(func(tx *datumujo.Tx) error {
		for _, d := range []datumujo.Def{issueDef, keepDef} {
			if err := tx.CreateTable(d); err != nil {
				return err
			}
		}
		for _, ix := range []datumujo.IndexDef{
			{Name: "by_state", Columns: []string{"repo", "state"}},
			{Name: "by_number", Columns: []string{"number"}},
		} {
			if err := tx.CreateIndex("issue", ix); err != nil {
				return err
			}
		}
		if err := tx.Insert("keep", datumujo.Row{int64(1), "kept"}); err != nil {
			return err
		}
		return fillIssues(tx, 400)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := checkAccounted(t, path)

	db = open(t, path, datumujo.Options{})
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.DropIndex("issue", "by_number"); err != nil {
			return err
		}
		if err := tx.DropIndex("issue", "by_number"); !errors.Is(err, datumujo.ErrNoIndex) {
			t.Errorf("second drop of the index: %v, want ErrNoIndex", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mid := checkAccounted(t, path)
	if len(mid.Stats.Tables[0].Indexes) != 1 || mid.Stats.FreePages <= before.Stats.FreePages {
		t.Errorf("after the index drop: indexes %v, free pages %d, before %d", mid.Stats.Tables[0].Indexes, mid.Stats.FreePages, before.Stats.FreePages)
	}

	db = open(t, path, datumujo.Options{})
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.DropTable("issue"); err != nil {
			return err
		}
		if err := tx.DropTable("issue"); !errors.Is(err, datumujo.ErrNoTable) {
			t.Errorf("second drop of the table: %v, want ErrNoTable", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after := checkAccounted(t, path)
	if len(after.Stats.Tables) != 1 || after.Stats.Tables[0].Name != "keep" {
		t.Fatalf("tables after the drop: %v", after.Stats.Tables)
	}
	// Only the catalog and the table keep are in use. Every other page is
	// free, and the file did not grow.
	if want := after.Stats.Pages - 1 - uint64(after.Stats.CatalogPages) - uint64(after.Stats.Tables[0].Pages); after.Stats.FreePages != want {
		t.Errorf("free pages %d, want %d", after.Stats.FreePages, want)
	}
	if after.Stats.Pages != before.Stats.Pages {
		t.Errorf("pages %d after the drop, %d before", after.Stats.Pages, before.Stats.Pages)
	}

	// The name is free again, and the new table starts empty. It reuses
	// the free pages.
	db = open(t, path, datumujo.Options{})
	defer db.Close()
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(issueDef); err != nil {
			return err
		}
		if err := tx.CreateIndex("issue", datumujo.IndexDef{Name: "by_state", Columns: []string{"repo", "state"}}); err != nil {
			return err
		}
		return fillIssues(tx, 10)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(func(v *datumujo.View) error {
		if n := count(t, v, "issue", datumujo.ScanOptions{}); n != 10 {
			t.Errorf("new table issue: %d rows, want 10", n)
		}
		row, ok, err := v.Get("keep", int64(1))
		if err != nil || !ok || row[1] != "kept" {
			t.Errorf("table keep: %v %v %v", row, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// scanner is a View or a Tx.
type scanner interface {
	Scan(table string, o datumujo.ScanOptions) (*datumujo.Rows, error)
}

func count(t *testing.T, v scanner, table string, o datumujo.ScanOptions) int {
	t.Helper()
	rows, err := v.Scan(table, o)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSavepoint rolls a transaction back to a savepoint after rows,
// schema changes, a drop and a counter. What came before the savepoint
// stays, what came after is gone, and the transaction goes on and
// commits a file that the check accepts.
func TestSavepoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, datumujo.Options{})
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(issueDef); err != nil {
			return err
		}
		return fillIssues(tx, 50)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.Insert("issue", datumujo.Row{int64(2), int64(1), "open", "before"}); err != nil {
			return err
		}
		first, err := tx.Next("c")
		if err != nil {
			return err
		}
		sp := tx.Savepoint()
		for round := 0; round < 2; round++ {
			if err := tx.Insert("issue", datumujo.Row{int64(2), int64(2), "open", strings.Repeat("x", 9000)}); err != nil {
				return err
			}
			if err := tx.CreateTable(keepDef); err != nil {
				return err
			}
			if err := tx.CreateIndex("issue", datumujo.IndexDef{Name: "by_state", Columns: []string{"repo", "state"}}); err != nil {
				return err
			}
			if n, err := tx.Next("c"); err != nil || n != first+1 {
				t.Errorf("round %d: counter %d %v, want %d", round, n, err, first+1)
			}
			if err := tx.DropTable("issue"); err != nil {
				return err
			}
			if err := tx.RollbackTo(sp); err != nil {
				return err
			}
			if _, ok := tx.Schema().Table("keep"); ok {
				t.Errorf("round %d: table keep after the rollback", round)
			}
			if _, ok := tx.Schema().Table("issue"); !ok {
				t.Fatalf("round %d: table issue is gone after the rollback", round)
			}
			if n := count(t, tx, "issue", datumujo.ScanOptions{}); n != 51 {
				t.Errorf("round %d: %d rows, want 51", round, n)
			}
		}
		// The transaction goes on after the rollback.
		return tx.Insert("issue", datumujo.Row{int64(2), int64(3), "open", strings.Repeat("y", 9000)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r := checkAccounted(t, path)
	if len(r.Stats.Tables) != 1 || r.Stats.Tables[0].Rows != 52 || len(r.Stats.Tables[0].Indexes) != 0 {
		t.Errorf("after commit: %+v", r.Stats.Tables)
	}
}
