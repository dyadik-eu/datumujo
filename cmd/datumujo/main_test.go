package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// TestExitCodes checks the three exit codes of check, and what it prints.
func TestExitCodes(t *testing.T) {
	fs := vfs.NewSim()
	exit := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(fs, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	for _, args := range [][]string{nil, {"check"}, {"fix", "db"}, {"check", "a", "b"}} {
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
	for _, want := range []string{"intact", "table t: 3 rows", "index i: 3 entries", "1 free", "schema version: 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("intact: output lacks %q:\n%s", want, stdout)
		}
	}
	if code != 0 {
		t.Fatalf("intact: exit %d", code)
	}
}
