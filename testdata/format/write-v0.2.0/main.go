// Command write-v0.2.0 writes the file testdata/format/v0.2.0.db. It is
// built against the tag v0.2.0, not against the tree it lies in:
// tests/format.test.sh copies it into a worktree of the tag and runs it
// there. The file it writes is the same in each run.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/dyadik-eu/datumujo"
)

func main() {
	db, err := datumujo.Open(os.Args[1], datumujo.Options{})
	if err != nil {
		fail(err)
	}
	born := time.Date(1815, 12, 10, 0, 0, 0, 0, time.UTC)
	for _, st := range []struct {
		sql    string
		params []any
	}{
		{"CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT NOT NULL, born TIMESTAMP)", nil},
		{"CREATE UNIQUE INDEX by_name ON people (name)", nil},
		{"INSERT INTO people VALUES (1, 'Ada', ?), (2, 'Grace', NULL)", []any{born}},
		// The rows before hold no value for the added column.
		{"ALTER TABLE people ADD COLUMN note TEXT", nil},
		{"INSERT INTO people VALUES (3, 'Linus', NULL, 'added')", nil},
		// A table without a primary key has the hidden key rowid.
		{"CREATE TABLE log (at INTEGER, what TEXT)", nil},
		{"INSERT INTO log VALUES (10, 'start'), (20, 'stop')", nil},
	} {
		if _, err := db.Exec(st.sql, st.params...); err != nil {
			fail(fmt.Errorf("%s: %w", st.sql, err))
		}
	}
	// A checkpoint moves every page into the file, so the file alone is
	// the database.
	if _, err := db.Checkpoint(); err != nil {
		fail(err)
	}
	if err := db.Close(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
