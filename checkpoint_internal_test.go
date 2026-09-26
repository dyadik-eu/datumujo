package datumujo

import (
	"errors"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// TestCheckpointFailureIsReported lets a commit succeed and the
// checkpoint after it fail. Commit returns ErrCheckpoint, and the row is
// there after a power loss: the commit was durable.
func TestCheckpointFailureIsReported(t *testing.T) {
	def := Def{Name: "t", Columns: []Column{{Name: "k", Type: Int64}}, Key: []string{"k"}}
	setup := func(fs *vfs.Sim) *DB {
		// A checkpoint after every commit.
		db, err := open(fs, "db", Options{PageSize: 1024, CheckpointBytes: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *Tx) error { return tx.CreateTable(def) }); err != nil {
			t.Fatal(err)
		}
		return db
	}
	insert := func(db *DB) error {
		return db.Update(func(tx *Tx) error { return tx.Insert("t", Row{int64(7)}) })
	}
	// Count the calls of the commit alone, with checkpoints off.
	probe := vfs.NewSim()
	db := setup(probe)
	db.checkpointSize = -1
	before := probe.Calls()
	if err := insert(db); err != nil {
		t.Fatal(err)
	}
	commitCalls := probe.Calls() - before
	db.Close()

	fs := vfs.NewSim()
	db = setup(fs)
	fs.SetBudget(commitCalls)
	err := insert(db)
	if !errors.Is(err, ErrCheckpoint) || !errors.Is(err, vfs.ErrCrashed) {
		t.Fatalf("a checkpoint that fails after the commit: %v", err)
	}
	after, err := open(fs.Crash(nil), "db", Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if err := after.Read(func(v *View) error {
		_, ok, err := v.Get("t", int64(7))
		if err == nil && !ok {
			err = errors.New("the row of the durable commit is gone")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
