package datumujo_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/dyadik-eu/datumujo"
)

func TestSession(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "db"), datumujo.Options{})
	s := db.Session()
	defer s.Close()
	n := func() int64 {
		rows, err := s.Query("SELECT count(*) FROM t")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		rows.Next()
		return rows.Row()[0].(int64)
	}
	if _, err := s.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	// Outside a transaction, each statement is one. The first INSERT
	// stays, the second fails, the third does not run.
	r, err := s.Exec("INSERT INTO t VALUES (1); INSERT INTO t VALUES (1); INSERT INTO t VALUES (3)")
	if !errors.Is(err, datumujo.ErrExists) || r.RowsAffected != 1 || n() != 1 {
		t.Errorf("autocommit: %+v %v, %d rows", r, err, n())
	}
	// In a transaction, the same statements end with ROLLBACK.
	if _, err := s.Exec("BEGIN; INSERT INTO t VALUES (2); INSERT INTO t VALUES (4)"); err != nil {
		t.Fatal(err)
	}
	if !s.InTx() || n() != 3 {
		t.Errorf("in the transaction: %v, %d rows", s.InTx(), n())
	}
	other := db.Session()
	if rows, err := other.Query("SELECT count(*) FROM t"); err != nil {
		t.Fatal(err)
	} else {
		rows.Next()
		if got := rows.Row()[0].(int64); got != 1 {
			t.Errorf("another session sees %d rows before COMMIT", got)
		}
		rows.Close()
	}
	if _, err := s.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if s.InTx() || n() != 1 {
		t.Errorf("after ROLLBACK: %v, %d rows", s.InTx(), n())
	}
	for _, c := range []struct{ src string }{{"COMMIT"}, {"ROLLBACK"}, {"BEGIN; BEGIN"}} {
		_, err := s.Exec(c.src)
		var se *datumujo.SQLError
		if !errors.Is(err, datumujo.ErrSession) || !errors.As(err, &se) {
			t.Errorf("%s: %v", c.src, err)
		}
		s.Close()
	}
	if err := s.BeginRead(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("INSERT INTO t VALUES (5)"); !errors.Is(err, datumujo.ErrSession) {
		t.Errorf("write in a read-only transaction: %v", err)
	}
	if _, err := s.Exec("SELECT 1"); err == nil {
		t.Error("SELECT through Exec: no error")
	}
	// Close rolls back, and the writer is free for the next session.
	s.Exec("COMMIT")
	s.Exec("BEGIN; INSERT INTO t VALUES (6)")
	s.Close()
	if err := updateSoon(t, db, func(tx *datumujo.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n() != 1 {
		t.Errorf("after Close: %d rows", n())
	}
}
