package datumujo_test

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo"
)

// The files of requirement L-19 in testdata/format. v0.2.0.db is what
// the tag v0.2.0 wrote; tests/format.test.sh writes it again from the tag
// and compares. v2.db is what this code writes for a schema with a
// default and a check; tests/format.test.sh shows that v0.2.0 refuses it.
const (
	fileV020 = "testdata/format/v0.2.0.db"
	fileV2   = "testdata/format/v2.db"
)

var updateFormat = flag.Bool("update-format", false, "write "+fileV2+" again")

// copyFile copies a file of testdata to a new directory, so that a test
// that opens it never changes the file in the tree.
func copyFile(t *testing.T, src string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// formatVersion reads the format version from the header in the file.
// After a checkpoint, the file holds the header of the last commit.
func formatVersion(t *testing.T, path string) uint32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 12 {
		t.Fatalf("%s: %d bytes, %v", path, len(b), err)
	}
	return binary.BigEndian.Uint32(b[8:])
}

// rowsText runs a query and prints its rows, one line each.
func rowsText(t *testing.T, db *datumujo.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fields []string
		for _, v := range rows.Row() {
			if tm, ok := v.(time.Time); ok {
				v = tm.Format(time.DateOnly)
			}
			fields = append(fields, fmt.Sprint(v))
		}
		out = append(out, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return strings.Join(out, "; ")
}

// TestFileOfV020 opens the file that v0.2.0 wrote (I-3). Its rows read
// back as v0.2.0 wrote them, the rows from before ALTER TABLE included.
// Writes of the kind v0.2.0 knows keep it at format version 1, so
// v0.2.0 still reads it.
func TestFileOfV020(t *testing.T) {
	path := copyFile(t, fileV020)
	if v := formatVersion(t, path); v != 1 {
		t.Fatalf("%s has format version %d", fileV020, v)
	}
	rep, err := datumujo.Check(path)
	if err != nil || len(rep.Findings) > 0 || rep.Stats.FormatVersion != 1 {
		t.Fatalf("check: %v, %+v", err, rep)
	}
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"SELECT * FROM people ORDER BY id":           "1 Ada 1815-12-10 <nil>; 2 Grace <nil> <nil>; 3 Linus <nil> added",
		"SELECT rowid, * FROM log":                   "1 10 start; 2 20 stop",
		"SELECT id FROM people WHERE name = 'Grace'": "2",
	} {
		if got := rowsText(t, db, q); got != want {
			t.Errorf("%s:\n got  %s\n want %s", q, got, want)
		}
	}
	for _, q := range []string{
		"INSERT INTO people VALUES (4, 'Edsger', NULL, NULL)",
		"UPDATE people SET note = 'changed' WHERE id = 1",
		"CREATE TABLE more (k TEXT PRIMARY KEY, v REAL)",
		"CREATE INDEX by_v ON more (v)",
		"ALTER TABLE log ADD COLUMN who TEXT",
		"DROP TABLE more",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec("INSERT INTO people VALUES (5, 'Ada', NULL, NULL)"); err == nil {
		t.Error("the unique index of v0.2.0 does not hold")
	}
	if _, err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if v := formatVersion(t, path); v != 1 {
		t.Errorf("format version %d after writes that v0.2.0 knows", v)
	}
}

// writeV2 writes a file with a default and a check through the Go API.
// SQL has no DEFAULT and no CHECK yet.
func writeV2(t *testing.T, path string) {
	t.Helper()
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(datumujo.Def{
			Name: "item",
			Columns: []datumujo.Column{
				{Name: "id", Type: datumujo.Int64},
				{Name: "state", Type: datumujo.String, Default: "'open'"},
			},
			Key:    []string{"id"},
			Checks: []string{"id > 0"},
		}); err != nil {
			return err
		}
		return tx.Insert("item", datumujo.Row{int64(1), "open"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestFileOfV2 writes the file of format version 2 and compares it with
// the one in testdata, which tests/format.test.sh gives to v0.2.0. Run
// with -update-format to write it again after a change of the format.
func TestFileOfV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	writeV2(t, path)
	if v := formatVersion(t, path); v != 2 {
		t.Fatalf("format version %d", v)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if *updateFormat {
		if err := os.WriteFile(fileV2, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(fileV2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("this code writes another file than %s; if the format changed on purpose, run go test -run TestFileOfV2 -update-format", fileV2)
	}
	rep, err := datumujo.Check(path)
	if err != nil || len(rep.Findings) > 0 || rep.Stats.FormatVersion != 2 {
		t.Fatalf("check: %v, %+v", err, rep)
	}
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tb *datumujo.Table
	err = db.Read(func(v *datumujo.View) error {
		tb, _ = v.Schema().Table("item")
		return nil
	})
	if err != nil || tb == nil || tb.Columns[1].Default != "'open'" || len(tb.Checks) != 1 || tb.Checks[0] != "id > 0" {
		t.Fatalf("the table after a reopen: %+v, %v", tb, err)
	}
	if got := rowsText(t, db, "SELECT * FROM item"); got != "1 open" {
		t.Errorf("rows: %s", got)
	}
}
