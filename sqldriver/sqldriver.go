// Package sqldriver is the driver of datumujo for database/sql (L-11).
//
//	import _ "github.com/dyadik-eu/datumujo/sqldriver"
//
//	db, err := sql.Open("datumujo", "forge.db")
//
// The name of the source is the path of the database file. The pool of
// database/sql opens several connections; they share one open database,
// because a file of datumujo is open once per process. A program that
// has the database open already passes it to NewConnector and sql.OpenDB.
//
// Each connection is a datumujo.Session. Outside a transaction, each
// statement is a transaction of its own. Tx starts a write transaction,
// or a read-only one with TxOptions.ReadOnly; there is one write
// transaction at a time, and a second one waits. BEGIN, COMMIT and
// ROLLBACK in the SQL text work on a connection of sql.Conn.
//
// Parameters are ?, ?N or positional arguments; a named argument is an
// error. A context is checked before a statement starts, not while it
// runs.
package sqldriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"sync"

	"github.com/dyadik-eu/datumujo"
	"github.com/dyadik-eu/datumujo/internal/sqlexec"
)

func init() { sql.Register("datumujo", Driver{}) }

// Driver is the driver that database/sql knows as "datumujo".
type Driver struct{}

// Open opens a connection to the database in the file name. The pool of
// database/sql uses OpenConnector instead.
func (d Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector returns a connector for the file name.
func (Driver) OpenConnector(name string) (driver.Connector, error) {
	path, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	return &fileConnector{path: path}, nil
}

// shared is a database that the connections of all connectors of one
// file share, with the number of connectors that use it.
type shared struct {
	db    *datumujo.DB
	users int
}

var (
	mu    sync.Mutex
	files = map[string]*shared{}
)

// fileConnector opens the file on its first connection and closes it
// when database/sql closes the last connector of the file.
type fileConnector struct {
	path   string
	mu     sync.Mutex
	db     *datumujo.DB
	closed bool
}

// Connect opens a connection. database/sql checks the context before it
// calls Connect.
func (c *fileConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("sqldriver: the connector is closed")
	}
	if c.db == nil {
		mu.Lock()
		s, ok := files[c.path]
		if !ok {
			db, err := datumujo.Open(c.path, datumujo.Options{})
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			s = &shared{db: db}
			files[c.path] = s
		}
		s.users++
		mu.Unlock()
		c.db = s.db
	}
	return &conn{s: c.db.Session()}, nil
}

func (c *fileConnector) Driver() driver.Driver { return Driver{} }

// Close gives up the database. The last connector of a file closes it.
// database/sql calls Close when the program closes its sql.DB.
func (c *fileConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.db == nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	s := files[c.path]
	s.users--
	if s.users > 0 {
		return nil
	}
	delete(files, c.path)
	return s.db.Close()
}

// NewConnector returns a connector over a database that the program has
// open. Close of the sql.DB leaves it open.
func NewConnector(db *datumujo.DB) driver.Connector { return dbConnector{db} }

type dbConnector struct{ db *datumujo.DB }

func (c dbConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &conn{s: c.db.Session()}, nil
}

func (dbConnector) Driver() driver.Driver { return Driver{} }

// conn is a connection: a session.
type conn struct {
	s *datumujo.Session
}

var (
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
)

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext keeps the text. The statement is parsed and planned
// when it runs, so a change of the schema in between counts.
func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, ctx.Err()
}

// Close rolls back an open transaction.
func (c *conn) Close() error { return c.s.Close() }

// ResetSession runs before database/sql hands the connection to another
// user. A transaction that BEGIN in the text left open ends here, so it
// does not pass to that user.
func (c *conn) ResetSession(ctx context.Context) error {
	if c.s.InTx() {
		if err := c.s.Rollback(); err != nil {
			return err
		}
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx starts a transaction. Every level of isolation is served by a
// serializable transaction (T-1), which is the strongest.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	if opts.ReadOnly {
		err = c.s.BeginRead()
	} else {
		err = c.s.Begin()
	}
	if err != nil {
		return nil, err
	}
	return tx{c.s}, nil
}

type tx struct{ s *datumujo.Session }

func (t tx) Commit() error   { return t.s.Commit() }
func (t tx) Rollback() error { return t.s.Rollback() }

// values turns the arguments of database/sql into parameters.
func values(args []driver.NamedValue) ([]any, error) {
	out := make([]any, len(args))
	for _, a := range args {
		if a.Name != "" {
			return nil, errors.New("sqldriver: named parameter " + a.Name + "; use ? or ?N")
		}
		out[a.Ordinal-1] = a.Value
	}
	return out, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ps, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.s.Exec(query, ps...)
	if err != nil {
		return nil, err
	}
	return result{r}, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ps, err := values(args)
	if err != nil {
		return nil, err
	}
	r, err := c.s.Query(query, ps...)
	if err != nil {
		return nil, err
	}
	return &rows{r: r}, nil
}

type result struct{ r datumujo.Result }

func (r result) LastInsertId() (int64, error) { return r.r.LastInsertID, nil }
func (r result) RowsAffected() (int64, error) { return r.r.RowsAffected, nil }

// stmt is a prepared statement: the text, run on its connection.
type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 } // the run checks the parameters

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// rows are the rows of a query.
type rows struct {
	r *datumujo.SQLRows
}

var _ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)

func (r *rows) Columns() []string {
	cols := r.r.Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

func (r *rows) Close() error {
	r.r.Close()
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if !r.r.Next() {
		if err := r.r.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i, v := range r.r.Row() {
		dest[i] = v
	}
	return nil
}

// ColumnTypeDatabaseTypeName returns the SQL type of a column, or "" when
// the query does not know it before the run, as for a parameter.
func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	c := r.r.Columns()[i]
	if !c.Known {
		return ""
	}
	return sqlexec.TypeName(c.Type)
}
