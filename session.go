package datumujo

import (
	"errors"

	"github.com/dyadik-eu/datumujo/internal/sqlexec"
	"github.com/dyadik-eu/datumujo/internal/sqlparse"
)

// ErrSession matches a statement that does not fit the state of a
// Session: BEGIN in a transaction, COMMIT or ROLLBACK outside one, or a
// write in a read-only transaction.
var ErrSession = errors.New("datumujo: the statement does not fit the state of the session")

// Session runs SQL text as the connection of a client does. BEGIN,
// COMMIT and ROLLBACK in the text start and end a transaction of the
// session. Outside one, each statement is a transaction of its own, as
// in SQLite. A Session is for one goroutine at a time. Close rolls back
// a transaction that is still open.
type Session struct {
	db   *DB
	tx   *Tx
	view *View // the snapshot of a read-only transaction
}

// Session returns a new session on the database.
func (db *DB) Session() *Session { return &Session{db: db} }

// InTx reports whether a transaction of the session is open.
func (s *Session) InTx() bool { return s.tx != nil || s.view != nil }

// Begin starts a write transaction. It waits while another one runs.
func (s *Session) Begin() error {
	if s.InTx() {
		return ErrSession
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	s.tx = tx
	return nil
}

// BeginRead starts a read-only transaction: the queries read the last
// commit, and a write is an error. It never waits.
func (s *Session) BeginRead() error {
	if s.InTx() {
		return ErrSession
	}
	v, err := s.db.View()
	if err != nil {
		return err
	}
	s.view = v
	return nil
}

// Commit ends the transaction of the session and keeps its changes.
func (s *Session) Commit() error {
	switch {
	case s.tx != nil:
		tx := s.tx
		s.tx = nil
		return tx.Commit()
	case s.view != nil:
		s.view.Close()
		s.view = nil
		return nil
	}
	return ErrSession
}

// Rollback ends the transaction of the session without its changes.
func (s *Session) Rollback() error {
	switch {
	case s.tx != nil:
		s.tx.Rollback()
		s.tx = nil
		return nil
	case s.view != nil:
		s.view.Close()
		s.view = nil
		return nil
	}
	return ErrSession
}

// Close rolls back an open transaction. A Session is done after it.
func (s *Session) Close() error {
	if s.InTx() {
		return s.Rollback()
	}
	return nil
}

// Exec runs the statements of the text in order. The result sums the
// rows of all of them. At the first error it stops, and the statements
// before it keep what they did: in a transaction, until it ends; outside
// one, for good.
func (s *Session) Exec(query string, params ...any) (Result, error) {
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
		r, err := s.exec(st, ps)
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

func (s *Session) exec(st sqlparse.Statement, ps []any) (Result, error) {
	var err error
	switch st.(type) {
	case *sqlparse.Begin:
		err = s.Begin()
	case *sqlparse.Commit:
		err = s.Commit()
	case *sqlparse.Rollback:
		err = s.Rollback()
	default:
		switch {
		case s.view != nil:
			return Result{}, &SQLError{At: st.Pos(), Msg: "a read-only transaction does not write", Err: ErrSession}
		case s.tx != nil:
			return sqlexec.Exec(s.tx.Tx, st, ps, s.db.limits())
		}
		var r Result
		err := s.db.Update(func(tx *Tx) error {
			var err error
			r, err = sqlexec.Exec(tx.Tx, st, ps, s.db.limits())
			return err
		})
		return r, err
	}
	if errors.Is(err, ErrSession) {
		return Result{}, &SQLError{At: st.Pos(), Msg: st.String() + " does not fit the state of the session", Err: ErrSession}
	}
	return Result{}, err
}

// Query runs one SELECT: in the transaction of the session if one is
// open, else on the last commit.
func (s *Session) Query(q string, params ...any) (*SQLRows, error) {
	switch {
	case s.tx != nil:
		return s.tx.Query(q, params...)
	case s.view != nil:
		return s.view.Query(q, params...)
	}
	return s.db.Query(q, params...)
}

// Script runs the statements of the text in order, as Exec does, and
// SELECT too. For a SELECT, it calls fn with its rows and closes them
// after; the rows are valid only in the call. An error, of a statement
// or of fn, stops the script. Errors in the text name the line and
// column in the whole text.
func (s *Session) Script(text string, fn func(*SQLRows) error, params ...any) (Result, error) {
	sts, err := sqlparse.ParseAll(text)
	if err != nil {
		return Result{}, err
	}
	ps, err := sqlexec.Params(params)
	if err != nil {
		return Result{}, err
	}
	var total Result
	for _, st := range sts {
		if sel, ok := st.(*sqlparse.Select); ok {
			if err := s.query(sel, ps, fn); err != nil {
				return total, err
			}
			continue
		}
		r, err := s.exec(st, ps)
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

// query runs a parsed SELECT in the state of the session and hands its
// rows to fn.
func (s *Session) query(sel *sqlparse.Select, ps []any, fn func(*SQLRows) error) error {
	var rows *sqlexec.Rows
	var err error
	switch {
	case s.tx != nil:
		rows, err = runSelect(s.tx.Tx, s.db.limits(), sel, ps)
	case s.view != nil:
		rows, err = runSelect(s.view.View, s.db.limits(), sel, ps)
	default:
		var v *View
		if v, err = s.db.View(); err != nil {
			return err
		}
		defer v.Close()
		rows, err = runSelect(v.View, s.db.limits(), sel, ps)
	}
	if err != nil {
		return err
	}
	if err := fn(&SQLRows{Rows: rows}); err != nil {
		return err
	}
	// An error in a row ends the rows. fn may not have asked for it; it
	// must not get lost.
	return rows.Err()
}
