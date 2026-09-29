package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// TestExitCodes checks the three exit codes of check, and what it prints.
func TestExitCodes(t *testing.T) {
	fs := vfs.NewSim()
	exit := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(fs, args, nil, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	for _, args := range [][]string{nil, {"check"}, {"fix", "db"}, {"check", "a", "b"}, {"backup", "a"}, {"restore", "a", "b", "c"}} {
		if code, _, stderr := exit(args...); code != 2 || !strings.Contains(stderr, "usage") {
			t.Errorf("%q: exit %d, %q", args, code, stderr)
		}
	}
	if code, _, stderr := exit("check", "db"); code != 2 || !strings.Contains(stderr, "no such file") {
		t.Fatalf("no file: exit %d, %q", code, stderr)
	}

	s, err := store.Open(fs, "db", store.Options{PageSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	stx, _ := s.Begin()
	tx, _ := table.Begin(stx, 1024)
	if err := tx.CreateTable(table.Def{Name: "t", Columns: []table.Column{{Name: "k", Type: table.Int64}}, Key: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("t", table.IndexDef{Name: "i", Columns: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	for k := int64(0); k < 3; k++ {
		if err := tx.Insert("t", table.Row{k}); err != nil {
			t.Fatal(err)
		}
	}
	// A page that no tree uses: the one fault this test can make on the
	// simulated disk.
	lost, _ := stx.Allocate()
	if err := stx.Write(lost, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, stdout, _ := exit("check", "db")
	if code != 1 || !strings.Contains(stdout, "belongs to no tree") || !strings.Contains(stdout, "1 faults") {
		t.Fatalf("a fault: exit %d, %q", code, stdout)
	}

	s, _ = store.Open(fs, "db", store.Options{})
	stx, _ = s.Begin()
	if err := stx.Free(lost); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, stdout, _ = exit("check", "db")
	for _, want := range []string{"intact", "table t: 3 rows", "index i: 3 entries", "1 free", "format version: 1", "schema version: 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("intact: output lacks %q:\n%s", want, stdout)
		}
	}
	if code != 0 {
		t.Fatalf("intact: exit %d", code)
	}

	// A log with a damaged frame and a later commit behind it. The
	// database does not open, and no count is printed as if it were read.
	s, _ = store.Open(fs, "db", store.Options{})
	for k := int64(10); k < 12; k++ {
		stx, _ = s.Begin()
		tx, _ = table.Begin(stx, 1024)
		if err := tx.Insert("t", table.Row{k}); err != nil {
			t.Fatal(err)
		}
		if err := stx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	f, _ := fs.Open("db-log")
	b := make([]byte, 1)
	f.ReadAt(b, 28+28+100)
	b[0] ^= 1
	f.WriteAt(b, 28+28+100)
	f.Sync()
	f.Close()
	code, stdout, _ = exit("check", "db")
	if code != 1 || !strings.Contains(stdout, "log is damaged") || !strings.Contains(stdout, "could not be opened") || strings.Contains(stdout, "pages:") {
		t.Fatalf("damaged log: exit %d, %q", code, stdout)
	}
}

// TestBackupAndRestore copies a database with backup, restores it with
// restore, and checks that neither replaces a file.
func TestBackupAndRestore(t *testing.T) {
	fs := vfs.NewSim()
	s, err := store.Open(fs, "db", store.Options{PageSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	stx, _ := s.Begin()
	tx, _ := table.Begin(stx, 1024)
	if err := tx.CreateTable(table.Def{Name: "t", Columns: []table.Column{{Name: "k", Type: table.Int64}}, Key: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	for k := int64(0); k < 5; k++ {
		if err := tx.Insert("t", table.Row{k}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	exit := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := run(fs, args, nil, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := exit("backup", "db", "copy"); code != 0 || !strings.Contains(out, "copy written") {
		t.Fatalf("backup: exit %d, %q", code, out)
	}
	if code, out := exit("backup", "db", "copy"); code != 1 || !strings.Contains(out, "exists") {
		t.Fatalf("backup over a copy: exit %d, %q", code, out)
	}
	if code, out := exit("restore", "copy", "db2"); code != 0 {
		t.Fatalf("restore: exit %d, %q", code, out)
	}
	if code, out := exit("check", "db2"); code != 0 || !strings.Contains(out, "table t: 5 rows") {
		t.Fatalf("check of the restored database: exit %d, %q", code, out)
	}
	if code, out := exit("restore", "nope", "db3"); code != 1 {
		t.Fatalf("restore from no file: exit %d, %q", code, out)
	}
}

// TestSQL runs the command sql: its output, its exit codes, and what it
// leaves in the file (L-12).
func TestSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	sql := func(stdin string, args ...string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := run(vfs.OS{}, append([]string{"sql", path}, args...), strings.NewReader(stdin), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	code, stdout, stderr := sql(`CREATE TABLE issue (id INTEGER PRIMARY KEY, title TEXT, score REAL, open BOOLEAN);
		INSERT INTO issue (title, score, open) VALUES ('crash', 1.5, TRUE), ('NULL', NULL, FALSE),
			('tab	here', 2.0, NULL), ('''quoted''', 3.0, TRUE);
		SELECT id, title, score, open FROM issue ORDER BY id;
		SELECT count(*) AS n FROM issue`)
	want := "id\ttitle\tscore\topen\n" +
		"1\tcrash\t1.5\tTRUE\n" +
		"2\t'NULL'\tNULL\tFALSE\n" +
		"3\t'tab\there'\t2.0\tNULL\n" +
		"4\t'''quoted'''\t3.0\tTRUE\n" +
		"n\n4\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("exit %d\n%s\nstderr %q\nwant\n%s", code, stdout, stderr, want)
	}
	// The SQL as an argument; the input is not read.
	if code, stdout, _ := sql("this is no SQL", "SELECT title FROM issue WHERE id = 1"); code != 0 || stdout != "title\ncrash\n" {
		t.Errorf("SQL as argument: exit %d, %q", code, stdout)
	}
	// The first failing statement stops the text, with its position.
	// The transaction it is in rolls back; what ran before it in its own
	// transaction stays.
	code, stdout, stderr = sql("INSERT INTO issue (title) VALUES ('kept');\nBEGIN;\nINSERT INTO issue (title) VALUES ('gone');\nINSERT INTO issue (id) VALUES (1);\nSELECT 1")
	if code != 1 || stdout != "" || stderr != "line 4, column 32: a row with this key exists: table issue\n" {
		t.Errorf("failing statement: exit %d, %q, %q", code, stdout, stderr)
	}
	if code, stdout, _ := sql("SELECT title FROM issue WHERE id > 4 ORDER BY id"); code != 0 || stdout != "title\nkept\n" {
		t.Errorf("after the failure: exit %d, %q", code, stdout)
	}
	// An error in a row, after rows were printed.
	if code, _, stderr := sql("SELECT 10 / (2 - id) FROM issue ORDER BY id"); code != 1 || !strings.Contains(stderr, "division by zero") {
		t.Errorf("error in a row: exit %d, %q", code, stderr)
	}
	if code, _, stderr := sql("SELEC 1"); code != 1 || !strings.Contains(stderr, "line 1, column 1") {
		t.Errorf("syntax error: exit %d, %q", code, stderr)
	}
	// A file that cannot open: exit 2.
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := sql("SELECT 1"); code != 2 || stderr == "" {
		t.Errorf("locked file: exit %d, %q", code, stderr)
	}
	db.Close()
	var out, errOut bytes.Buffer
	if code := run(vfs.OS{}, []string{"sql", t.TempDir()}, strings.NewReader("SELECT 1"), &out, &errOut); code != 2 {
		t.Errorf("a directory as the file: exit %d, %q", code, errOut.String())
	}
	if code := run(vfs.OS{}, []string{"sql"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Errorf("no file: exit %d", code)
	}
}
