package sqlexec

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// A UNIQUE of a column or a table is a unique index of the table (L-15).
// It has no mark of its own in the schema, since the index alone keeps
// the rule. So a file with UNIQUE and no DEFAULT or CHECK stays at
// format version 1. Its name is <table>_unique_<n>, with the smallest n that no
// index and no table has yet.

// uniqueSet is the columns of one UNIQUE, and the place to name in an
// error.
type uniqueSet struct {
	at      sqlparse.At
	columns []string
}

// uniqueSets collects the UNIQUE of the columns, in column order, then
// those of the table. It checks that each names columns of the
// statement, each once; the hidden key rowid is none of them, as in
// SQLite. A set that the primary key or an earlier set already has adds
// nothing: both make the same rows unique.
func uniqueSets(s *sqlparse.CreateTable, key []string) ([]uniqueSet, error) {
	var all []uniqueSet
	for _, c := range s.Columns {
		if c.Unique {
			all = append(all, uniqueSet{c.UniqueAt, []string{c.Name}})
		}
	}
	for _, u := range s.Uniques {
		all = append(all, uniqueSet{u.At, u.Columns})
	}
	have := map[string]bool{}
	for _, c := range s.Columns {
		have[c.Name] = true
	}
	seen := map[string]bool{setKey(key): true}
	var out []uniqueSet
	for _, u := range all {
		in := map[string]bool{}
		for _, c := range u.columns {
			if !have[c] {
				return nil, errAt(u.at, "UNIQUE: no such column: %s", c)
			}
			if in[c] {
				return nil, errAt(u.at, "UNIQUE: column %s twice", c)
			}
			in[c] = true
		}
		k := setKey(u.columns)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	return out, nil
}

// setKey is the same text for the same columns in any order.
func setKey(cols []string) string {
	sorted := slices.Clone(cols)
	slices.Sort(sorted)
	return strings.Join(sorted, "\x00")
}

// uniqueName returns the name for the next UNIQUE index of table t. It
// is the first <t>_unique_<n> from n = 1 that no index and no table has.
func uniqueName(sc *table.Schema, t string, at sqlparse.At) (string, error) {
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s_unique_%d", t, n)
		if len(name) > table.MaxName {
			return "", wrap(at, table.ErrSchema, "the name %s of the index for this UNIQUE is longer than %d bytes; a shorter table name or CREATE UNIQUE INDEX with a name of its own fits", name, table.MaxName)
		}
		_, isIndex := indexOwner(sc, name)
		_, isTable := sc.Table(name)
		if !isIndex && !isTable {
			return name, nil
		}
	}
}

// createUniques makes the unique index of each set on table t, which
// is new and holds no row.
func createUniques(tx *table.Tx, t string, sets []uniqueSet) error {
	for _, u := range sets {
		name, err := uniqueName(tx.Schema(), t, u.at)
		if err != nil {
			return err
		}
		if err := tx.CreateIndex(t, table.IndexDef{Name: name, Columns: u.columns, Unique: true}); err != nil {
			return engineErr(u.at, err)
		}
	}
	return nil
}
