package datumujo_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo"
)

var issueDef = datumujo.Def{Name: "issue", Columns: []datumujo.Column{
	{Name: "repo", Type: datumujo.Int64},
	{Name: "number", Type: datumujo.Int64},
	{Name: "state", Type: datumujo.String},
	{Name: "body", Type: datumujo.String, Null: true},
}, Key: []string{"repo", "number"}}

// Example opens a database and adds a table with an index. It writes a row
// with a number from a counter and reads it back through the index.
func Example() {
	dir, _ := os.MkdirTemp("", "datumujo")
	defer os.RemoveAll(dir)
	db, err := datumujo.Open(filepath.Join(dir, "forge.db"), datumujo.Options{})
	if err != nil {
		panic(err)
	}
	defer db.Close()
	err = db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(issueDef); err != nil {
			return err
		}
		if err := tx.CreateIndex("issue", datumujo.IndexDef{Name: "by_state", Columns: []string{"repo", "state"}}); err != nil {
			return err
		}
		n, err := tx.Next("issues of repo 1")
		if err != nil {
			return err
		}
		return tx.Insert("issue", datumujo.Row{int64(1), int64(n), "open", nil})
	})
	if err != nil {
		panic(err)
	}
	err = db.Read(func(v *datumujo.View) error {
		rows, err := v.Scan("issue", datumujo.ScanOptions{Index: "by_state", Prefix: []any{int64(1), "open"}})
		if err != nil {
			return err
		}
		for rows.Next() {
			fmt.Println(rows.Row()[:3])
		}
		return rows.Err()
	})
	if err != nil {
		panic(err)
	}
	// Output: [1 1 open]
}

// updateSoon runs an Update after one that failed. A failed Update that
// kept the writer would make it wait for ever, so it has a deadline.
func updateSoon(t *testing.T, db *datumujo.DB, fn func(tx *datumujo.Tx) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- db.Update(fn) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the Update waits: the one before kept the writer")
		return nil
	}
}

func open(t *testing.T, path string, opt datumujo.Options) *datumujo.DB {
	t.Helper()
	db, err := datumujo.Open(path, opt)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestAPI goes through the API as a program does: tables, rows, an index,
// a counter, a reopen, a read-only open, check, backup and restore.
func TestAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	db := open(t, path, datumujo.Options{})
	if err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(issueDef); err != nil {
			return err
		}
		if err := tx.CreateIndex("issue", datumujo.IndexDef{Name: "by_state", Columns: []string{"repo", "state"}}); err != nil {
			return err
		}
		for i := 0; i < 20; i++ {
			n, err := tx.Next("repo 1")
			if err != nil {
				return err
			}
			state := "open"
			if i%3 == 0 {
				state = "closed"
			}
			if err := tx.Insert("issue", datumujo.Row{int64(1), int64(n), state, nil}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A failed write: the error comes back, and nothing changes.
	err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.Insert("issue", datumujo.Row{int64(1), int64(21), "open", nil}); err != nil {
			return err
		}
		return tx.Insert("issue", datumujo.Row{int64(1), int64(5), "open", nil})
	})
	if !errors.Is(err, datumujo.ErrExists) {
		t.Fatalf("insert of a key that exists: %v", err)
	}
	// The failed Update rolled back: its first insert is gone, and the
	// next Update gets the writer. Without the rollback it would wait for
	// ever, so it runs with a deadline.
	if err := updateSoon(t, db, func(tx *datumujo.Tx) error {
		_, ok, err := tx.Get("issue", int64(1), int64(21))
		if ok {
			return errors.New("the insert of the failed Update is there")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	read := func(db *datumujo.DB) (rows, open int, counter uint64) {
		t.Helper()
		if err := db.Read(func(v *datumujo.View) error {
			all, err := v.Scan("issue", datumujo.ScanOptions{})
			if err != nil {
				return err
			}
			for all.Next() {
				rows++
			}
			byState, err := v.Scan("issue", datumujo.ScanOptions{Index: "by_state", Prefix: []any{int64(1), "open"}})
			if err != nil {
				return err
			}
			for byState.Next() {
				open++
			}
			counter, err = v.Counter("repo 1")
			return errors.Join(all.Err(), byState.Err(), err)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if rows, open, n := read(db); rows != 20 || open != 13 || n != 20 {
		t.Fatalf("%d rows, %d open, counter %d; want 20, 13, 20", rows, open, n)
	}
	if err := db.Backup(filepath.Join(dir, "copy")); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(filepath.Join(dir, "copy")); !errors.Is(err, datumujo.ErrBackup) {
		t.Fatalf("backup over a copy: %v", err)
	}
	if _, err := datumujo.Check(path); !errors.Is(err, datumujo.ErrLocked) {
		t.Fatalf("check of an open database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}

	r, err := datumujo.Check(path)
	if err != nil || len(r.Findings) != 0 || r.Stats.Tables[0].Rows != 20 {
		t.Fatalf("check: %v %+v", err, r)
	}
	if err := datumujo.Restore(filepath.Join(dir, "copy"), filepath.Join(dir, "restored")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(dir, "restored")} {
		db := open(t, p, datumujo.Options{ReadOnly: true})
		if rows, open, n := read(db); rows != 20 || open != 13 || n != 20 {
			t.Fatalf("%s: %d rows, %d open, counter %d", p, rows, open, n)
		}
		if err := db.Update(func(*datumujo.Tx) error { return nil }); !errors.Is(err, datumujo.ErrReadOnly) {
			t.Fatalf("%s: a write on a read-only database: %v", p, err)
		}
		db.Close()
	}
	if _, err := datumujo.Open(filepath.Join(dir, "missing"), datumujo.Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only open of a missing file")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatal("read-only open created the file")
	}
}

// TestAutomaticCheckpoint writes commits of about 5 KB with a checkpoint
// size of 64 KiB. The log never grows past that plus one commit, and it
// did reach the size, so checkpoints ran.
//
// While a view of an earlier commit is open, no checkpoint runs and the
// log grows. The first commit after the view closes brings it back. With
// the default size and with a negative size, the log only grows here.
func TestAutomaticCheckpoint(t *testing.T) {
	const limit = 64 << 10
	dir := t.TempDir()
	body := strings.Repeat("x", 5000)
	write := func(db *datumujo.DB, from, to int) (max, commit int64) {
		t.Helper()
		for i := from; i < to; i++ {
			before := db.LogBytes()
			if err := db.Update(func(tx *datumujo.Tx) error {
				if i == 0 {
					if err := tx.CreateTable(issueDef); err != nil {
						return err
					}
				}
				return tx.Insert("issue", datumujo.Row{int64(1), int64(i), "open", body})
			}); err != nil {
				t.Fatal(err)
			}
			if l := db.LogBytes(); l > max {
				max = l
			}
			if d := db.LogBytes() - before; d > commit {
				commit = d
			}
		}
		return max, commit
	}
	db := open(t, filepath.Join(dir, "db"), datumujo.Options{CheckpointBytes: limit})
	// A Read closes its view: a view left open would stop every
	// checkpoint below.
	if err := db.Read(func(*datumujo.View) error { return nil }); err != nil {
		t.Fatal(err)
	}
	resets := 0
	last := db.LogBytes()
	for i := 0; i < 100; i++ {
		max, commit := write(db, i, i+1)
		if max > limit+commit {
			t.Fatalf("commit %d: the log has %d bytes; limit %d, commit %d", i, max, limit, commit)
		}
		if db.LogBytes() < last {
			resets++
		}
		last = db.LogBytes()
	}
	if resets == 0 {
		t.Fatal("no checkpoint ran")
	}
	v, err := db.View()
	if err != nil {
		t.Fatal(err)
	}
	max, _ := write(db, 100, 150)
	if max < 2*limit {
		t.Fatalf("with a view open the log has at most %d bytes; a checkpoint ran under the view", max)
	}
	v.Close()
	write(db, 150, 151)
	if l := db.LogBytes(); l >= limit {
		t.Fatalf("after the view closed, the log has %d bytes", l)
	}
	db.Close()

	// The default size is 4 MiB: 40 commits of 5 KB run no checkpoint.
	def := open(t, filepath.Join(dir, "default"), datumujo.Options{})
	max, _ = write(def, 0, 40)
	if def.LogBytes() != max || max < 40*5000 {
		t.Fatalf("with the default size, the log has %d bytes, at most %d", def.LogBytes(), max)
	}
	def.Close()

	manual := open(t, filepath.Join(dir, "manual"), datumujo.Options{CheckpointBytes: -1})
	defer manual.Close()
	max, _ = write(manual, 0, 40)
	if max < 3*limit || manual.LogBytes() != max {
		t.Fatalf("without automatic checkpoints the log has %d bytes, at most %d", manual.LogBytes(), max)
	}
	if ok, err := manual.Checkpoint(); !ok || err != nil || manual.LogBytes() >= limit {
		t.Fatalf("manual checkpoint: %v %v, log %d bytes", ok, err, manual.LogBytes())
	}
	t.Logf("%d checkpoints in 100 commits", resets)
}

// TestOptionsReachTheStore checks that the page size and the memory bound
// of Options take effect.
func TestOptionsReachTheStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	db := open(t, path, datumujo.Options{PageSize: 1024, MaxTxBytes: 8 * 1024})
	err := db.Update(func(tx *datumujo.Tx) error {
		if err := tx.CreateTable(issueDef); err != nil {
			return err
		}
		return tx.Insert("issue", datumujo.Row{int64(1), int64(1), "open", strings.Repeat("x", 20*1024)})
	})
	if !errors.Is(err, datumujo.ErrTxTooLarge) {
		t.Fatalf("a row of 20 KB in a transaction of at most 8 KB: %v", err)
	}
	if err := updateSoon(t, db, func(tx *datumujo.Tx) error { return tx.CreateTable(issueDef) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := datumujo.Check(path)
	if err != nil || r.Stats.PageSize != 1024 {
		t.Fatalf("page size: %v %+v", err, r.Stats)
	}
}

func TestExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, datumujo.Options{})
	r, err := db.Exec(`CREATE TABLE repo (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE UNIQUE INDEX by_name ON repo (name);
		INSERT INTO repo (name) VALUES (?), (?)`, "alpha", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if r.RowsAffected != 2 || r.LastInsertID != 2 {
		t.Errorf("result %+v", r)
	}
	// One Exec is one transaction: the second statement fails, so the
	// first is gone too.
	_, err = db.Exec("INSERT INTO repo (name) VALUES ('gamma'); INSERT INTO repo (name) VALUES ('alpha')")
	var se *datumujo.SQLError
	if !errors.As(err, &se) || !errors.Is(err, datumujo.ErrUnique) || se.At.Line != 1 {
		t.Errorf("unique conflict: %v", err)
	}
	// In a Tx, a failed statement changes nothing and the Tx goes on.
	if err := db.Update(func(tx *datumujo.Tx) error {
		if _, err := tx.Exec("INSERT INTO repo VALUES (7, 'alpha')"); !errors.Is(err, datumujo.ErrUnique) {
			t.Errorf("in a Tx: %v", err)
		}
		r, err := tx.Exec("INSERT INTO repo VALUES (?, ?)", 7, "delta")
		if err == nil && r.LastInsertID != 7 {
			t.Errorf("LastInsertID %d", r.LastInsertID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(func(v *datumujo.View) error {
		if n := count(t, v, "repo", datumujo.ScanOptions{}); n != 3 {
			t.Errorf("%d rows, want 3", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("SELECT 1"); err == nil {
		t.Error("SELECT through Exec: no error")
	}
	if _, err := db.Exec("INSERT INTO repo VALUES (?, 'x')", uint64(1<<63)); err == nil {
		t.Error("parameter 2^63: no error")
	}
}

func TestQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := open(t, path, datumujo.Options{CheckpointBytes: -1})
	if _, err := db.Exec(`CREATE TABLE repo (id INTEGER PRIMARY KEY, name TEXT NOT NULL, stars INTEGER);
		INSERT INTO repo (name, stars) VALUES ('alpha', 5), ('beta', NULL), ('gamma', 12)`); err != nil {
		t.Fatal(err)
	}
	read := func(rows *datumujo.SQLRows) []string {
		t.Helper()
		var out []string
		for rows.Next() {
			out = append(out, strings.TrimSuffix(fmt.Sprintln(rows.Row()...), "\n"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	rows, err := db.Query("SELECT name, stars FROM repo WHERE stars > ? ORDER BY stars DESC", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cols := rows.Columns(); len(cols) != 2 || cols[0].Name != "name" || cols[1].Type != datumujo.Int64 {
		t.Errorf("columns %+v", cols)
	}
	// While the rows hold their view, a checkpoint cannot run.
	if _, err := db.Exec("INSERT INTO repo (name) VALUES ('delta')"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.Checkpoint(); ok || err != nil {
		t.Errorf("checkpoint with open rows: %v %v", ok, err)
	}
	if got := read(rows); strings.Join(got, "|") != "gamma 12|alpha 5" {
		t.Errorf("rows %q", got)
	}
	rows.Close()
	rows.Close()
	if ok, err := db.Checkpoint(); !ok || err != nil {
		t.Errorf("checkpoint after Close: %v %v", ok, err)
	}
	// A Tx sees its own rows; a View sees its commit.
	if err := db.Update(func(tx *datumujo.Tx) error {
		if _, err := tx.Exec("DELETE FROM repo WHERE stars IS NULL"); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT count(*) FROM repo")
		if err != nil {
			return err
		}
		if got := read(rows); strings.Join(got, "|") != "2" {
			t.Errorf("count in the Tx: %q", got)
		}
		rows, err = tx.Query("SELECT id FROM repo")
		if err != nil {
			return err
		}
		defer rows.Close()
		if got := read(rows); strings.Join(got, "|") != "1|3" {
			t.Errorf("in the Tx: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(func(v *datumujo.View) error {
		rows, err := v.Query("SELECT name FROM repo ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		if got := read(rows); strings.Join(got, "|") != "alpha|gamma" {
			t.Errorf("in a View: %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var se *datumujo.SQLError
	if _, err := db.Query("DELETE FROM repo"); !errors.As(err, &se) {
		t.Errorf("DELETE through Query: %v", err)
	}
	if _, err := db.Query("SELECT nosuch FROM repo"); !errors.As(err, &se) || se.At.Col != 8 {
		t.Errorf("unknown column: %v", err)
	}
}
