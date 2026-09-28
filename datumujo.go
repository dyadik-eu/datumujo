package datumujo

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dyadik-eu/datumujo/internal/backup"
	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/sqlexec"
	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// The types of tables, rows and scans. They are the types of the engine,
// under names a program can use.
type (
	Type        = table.Type
	Column      = table.Column
	Def         = table.Def
	IndexDef    = table.IndexDef
	Row         = table.Row
	Schema      = table.Schema
	Table       = table.Table
	Index       = table.Index
	ScanOptions = table.Options
	Rows        = table.Rows
	Savepoint   = table.Savepoint
	Result      = sqlexec.Result
	SQLColumn   = sqlexec.Column
	SQLError    = sqlparse.Error
	Report      = check.Report
	Finding     = check.Finding
	Stats       = check.Stats
)

// The column types.
const (
	Int64   = table.Int64
	Float64 = table.Float64
	Bool    = table.Bool
	String  = table.String
	Bytes   = table.Bytes
	Time    = table.Time
)

// Errors that a program can act on. Each matches with errors.Is.
var (
	ErrSchema     = table.ErrSchema
	ErrValue      = table.ErrValue
	ErrNoTable    = table.ErrNoTable
	ErrNoIndex    = table.ErrNoIndex
	ErrExists     = table.ErrExists
	ErrNotFound   = table.ErrNotFound
	ErrUnique     = table.ErrUnique
	ErrDamaged    = table.ErrDamaged
	ErrTxTooLarge = store.ErrTxTooLarge
	ErrReadOnly   = store.ErrReadOnly
	ErrLocked     = vfs.ErrLocked
	ErrBackup     = backup.ErrExists
	// ErrQueryMemory means that a SQL statement would hold more rows in
	// memory than Options.QueryMemory allows.
	ErrQueryMemory = sqlexec.ErrMemory
	// ErrCheckpoint means that a commit is durable and the checkpoint
	// after it failed. The data is safe; the log grows until a checkpoint
	// works.
	ErrCheckpoint = errors.New("datumujo: the commit is durable, the checkpoint after it failed")
)

// DefaultCheckpointBytes is the log size after which a commit starts a
// checkpoint, when Options.CheckpointBytes is 0.
const DefaultCheckpointBytes = 4 << 20

// Options are the options of Open. The zero value is a database with the
// defaults.
type Options struct {
	PageSize   int   // for a new file; 0 means 4096
	MaxTxBytes int64 // bound on the changed pages of a transaction; 0 means 16 MiB
	ReadOnly   bool  // open an existing file and write nothing
	// CheckpointBytes starts a checkpoint after a commit that leaves the
	// log at least this large. 0 means DefaultCheckpointBytes, and a
	// negative value means never: the program calls Checkpoint.
	CheckpointBytes int64
	// QueryMemory bounds the bytes of rows that one SQL statement holds
	// in memory: to sort them, to drop repeated ones, or to change them.
	// A statement past it fails with ErrQueryMemory and returns or
	// changes nothing. 0 means DefaultQueryMemory.
	QueryMemory int64
}

// DefaultQueryMemory is the bound of Options.QueryMemory when it is 0.
const DefaultQueryMemory = sqlexec.DefaultMaxMemory

// DB is an open database. It is safe for use by several goroutines. There
// is one write transaction at a time; readers never wait for it.
type DB struct {
	s              *store.Store
	fs             vfs.FS
	checkpointSize int64
	queryMemory    int64
	// mu guards closed. A Close waits for no transaction; the caller ends
	// them first.
	mu     sync.Mutex
	closed bool
}

// Open opens the database in the file path. Without ReadOnly, it creates
// a missing file. The log is path+"-log", the lock path+"-lock".
func Open(path string, opt Options) (*DB, error) {
	return open(vfs.OS{}, path, opt)
}

func open(fs vfs.FS, path string, opt Options) (*DB, error) {
	s, err := store.Open(fs, path, store.Options{PageSize: opt.PageSize, MaxTxBytes: opt.MaxTxBytes, ReadOnly: opt.ReadOnly})
	if err != nil {
		return nil, err
	}
	size := opt.CheckpointBytes
	if size == 0 {
		size = DefaultCheckpointBytes
	}
	return &DB{s: s, fs: fs, checkpointSize: size, queryMemory: opt.QueryMemory}, nil
}

// Close closes the database. Transactions and views must be done before.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	return db.s.Close()
}

// View is a reader of one commit. Its reads never wait for a writer and
// never see a later commit. It must be closed; while it is open, no
// checkpoint runs.
type View struct {
	*table.View
	snap *store.Snapshot
	lim  sqlexec.Limits
}

// View starts a reader of the last commit.
func (db *DB) View() (*View, error) {
	snap, err := db.s.Snapshot()
	if err != nil {
		return nil, err
	}
	v, err := table.Open(snap, db.s.PageSize())
	if err != nil {
		snap.Close()
		return nil, err
	}
	return &View{View: v, snap: snap, lim: db.limits()}, nil
}

// limits are the bounds of a SQL statement.
func (db *DB) limits() sqlexec.Limits { return sqlexec.Limits{MaxMemory: db.queryMemory} }

// Close ends the view. A second Close does nothing.
func (v *View) Close() { v.snap.Close() }

// Read runs fn with a view and closes it after.
func (db *DB) Read(fn func(v *View) error) error {
	v, err := db.View()
	if err != nil {
		return err
	}
	defer v.Close()
	return fn(v)
}

// Tx is the one write transaction. It sees the last commit and its own
// changes. It ends with Commit or Rollback. After an error that does not
// match ErrValue, ErrSchema, ErrNoTable, ErrNoIndex, ErrExists,
// ErrNotFound or ErrUnique, only Rollback is left.
type Tx struct {
	*table.Tx
	db  *DB
	stx *store.Tx
}

// Begin starts the write transaction. It waits while another one runs.
func (db *DB) Begin() (*Tx, error) {
	stx, err := db.s.Begin()
	if err != nil {
		return nil, err
	}
	tx, err := table.Begin(stx, db.s.PageSize())
	if err != nil {
		stx.Rollback()
		return nil, err
	}
	return &Tx{Tx: tx, db: db, stx: stx}, nil
}

// Commit makes the changes durable. When it returns nil, or an error
// that matches ErrCheckpoint, they are on stable storage. A commit that
// leaves the log at the checkpoint size starts a checkpoint. While a view
// of an earlier commit is open, the checkpoint waits for a later commit.
func (tx *Tx) Commit() error {
	if err := tx.stx.Commit(); err != nil {
		return err
	}
	db := tx.db
	if db.checkpointSize < 0 || db.s.LogBytes() < db.checkpointSize {
		return nil
	}
	if _, err := db.s.Checkpoint(); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpoint, err)
	}
	return nil
}

// Rollback ends the transaction without a change. After Commit it does
// nothing.
func (tx *Tx) Rollback() { tx.stx.Rollback() }

// Changed returns the number of pages the transaction has changed. The
// bound times the page size is Options.MaxTxBytes.
func (tx *Tx) Changed() int { return tx.stx.Changed() }

// Update runs fn in a write transaction. It commits if fn returns nil and
// rolls back otherwise, and returns the error of fn or of the commit.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Checkpoint copies the log into the file and starts a new log. It
// reports false, and does nothing, while a view of an earlier commit is
// open. Commits start checkpoints by themselves; see Options.
func (db *DB) Checkpoint() (bool, error) { return db.s.Checkpoint() }

// LogBytes returns the size of the committed part of the log.
func (db *DB) LogBytes() int64 { return db.s.LogBytes() }

// Backup writes the database, as the last commit has it, to the new file
// dest. Writes go on meanwhile. The check must accept the copy before it
// gets its name. It fails with ErrBackup if dest or dest+"-log" exists.
func (db *DB) Backup(dest string) error { return backup.Backup(db.s, db.fs, dest) }

// Check reads the database in the file path and reports each damaged
// page and each fault in its structure. No program may have it open. An
// error means it could not check; findings are in the Report.
func Check(path string) (*Report, error) { return check.Run(vfs.OS{}, path) }

// Restore makes the database to from from, a copy or a database that no
// program has open. It fails with ErrBackup if to or to+"-log" exists.
func Restore(from, to string) error { return backup.Restore(vfs.OS{}, from, to) }

// Exec runs SQL statements that write, in one transaction. It commits
// if all of them succeed and changes nothing otherwise. SELECT, BEGIN,
// COMMIT and ROLLBACK are not for Exec. An error in the SQL or in its
// run is a *SQLError with the position. The error of the table layer
// behind it, such as ErrUnique, matches with errors.Is. The result sums the rows
// of all statements, and LastInsertID is the one of the last INSERT.
func (db *DB) Exec(query string, params ...any) (Result, error) {
	var r Result
	err := db.Update(func(tx *Tx) error {
		var err error
		r, err = tx.Exec(query, params...)
		return err
	})
	return r, err
}

// Exec runs SQL statements that write, in the transaction. A statement
// that fails changes nothing, and the transaction goes on (L-7). The
// statements before it keep their changes. params[0] is ?1. A Go int
// and the other integer types become int64.
func (tx *Tx) Exec(query string, params ...any) (Result, error) {
	sts, err := sqlparse.ParseAll(query)
	if err != nil {
		return Result{}, err
	}
	ps, err := sqlexec.Params(params)
	if err != nil {
		return Result{}, err
	}
	var total Result
	for _, st := range sts {
		r, err := sqlexec.Exec(tx.Tx, st, ps, tx.db.limits())
		if err != nil {
			return total, err
		}
		total.RowsAffected += r.RowsAffected
		if _, ok := st.(*sqlparse.Insert); ok {
			total.LastInsertID = r.LastInsertID
		}
	}
	return total, nil
}

// SQLRows are the rows of a query. Next moves to the next row, Row
// returns it, and Err tells why Next returned false. Close ends the
// rows; for a query of a DB it also ends the view of the query.
type SQLRows struct {
	*sqlexec.Rows
	view *View
}

// Close ends the rows. A second Close does nothing.
func (r *SQLRows) Close() {
	if r.view != nil {
		r.view.Close()
		r.view = nil
	}
}

// query parses one SELECT and runs it on src.
func query(src sqlexec.Source, lim sqlexec.Limits, q string, params []any) (*sqlexec.Rows, error) {
	st, err := sqlparse.Parse(q)
	if err != nil {
		return nil, err
	}
	sel, ok := st.(*sqlparse.Select)
	if !ok {
		return nil, &SQLError{At: st.Pos(), Msg: st.String() + " returns no rows; run it with Exec", Err: sqlexec.ErrStatement}
	}
	ps, err := sqlexec.Params(params)
	if err != nil {
		return nil, err
	}
	pq, err := sqlexec.Prepare(src.Schema(), sel, lim)
	if err != nil {
		return nil, err
	}
	return pq.Run(src, ps)
}

// Query runs one SELECT on the view.
func (v *View) Query(q string, params ...any) (*SQLRows, error) {
	rows, err := query(v.View, v.lim, q, params)
	if err != nil {
		return nil, err
	}
	return &SQLRows{Rows: rows}, nil
}

// Query runs one SELECT in the transaction. It sees the changes of the
// transaction. A write while the rows are open leaves them undefined.
func (tx *Tx) Query(q string, params ...any) (*SQLRows, error) {
	rows, err := query(tx.Tx, tx.db.limits(), q, params)
	if err != nil {
		return nil, err
	}
	return &SQLRows{Rows: rows}, nil
}

// Query runs one SELECT on the last commit. The rows hold a view of that
// commit until Close; while it is open, no checkpoint runs.
func (db *DB) Query(q string, params ...any) (*SQLRows, error) {
	v, err := db.View()
	if err != nil {
		return nil, err
	}
	rows, err := query(v.View, v.lim, q, params)
	if err != nil {
		v.Close()
		return nil, err
	}
	return &SQLRows{Rows: rows, view: v}, nil
}
