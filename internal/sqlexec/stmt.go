package sqlexec

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// HiddenKey is the name of the key column of a table without a PRIMARY
// KEY. The name is reserved: no column of SQL can have it.
const HiddenKey = "rowid"

// Result is what a statement that writes reports.
type Result struct {
	RowsAffected int64
	// LastInsertID is the key of the last row an INSERT wrote, if the
	// table has an INTEGER key of one column or a hidden key. Else 0.
	LastInsertID int64
	scanned      int64 // rows an UPDATE or DELETE read, for the tests
}

// ErrStatement matches a statement that Exec does not run: SELECT,
// which Query runs, and BEGIN, COMMIT and ROLLBACK, which belong to a
// session.
var ErrStatement = errors.New("sqlexec: statement not supported here")

// Exec runs a statement that writes, in tx. A statement that fails
// changes nothing, and tx can go on (L-7): Exec rolls tx back to a
// savepoint taken before the statement. params[0] is ?1.
func Exec(tx *table.Tx, st sqlparse.Statement, params []any, lim Limits) (Result, error) {
	switch st.(type) {
	case *sqlparse.Select:
		return Result{}, wrap(st.Pos(), ErrStatement, "SELECT returns rows; run it with Query")
	case *sqlparse.Begin, *sqlparse.Commit, *sqlparse.Rollback:
		return Result{}, wrap(st.Pos(), ErrStatement, "%s: begin, commit and roll back a transaction with its methods", st)
	}
	sp := tx.Savepoint()
	r, err := run(tx, st, params, lim)
	if err != nil {
		if rb := tx.RollbackTo(sp); rb != nil {
			return Result{}, errors.Join(err, rb)
		}
		return Result{}, err
	}
	return r, nil
}

// wrap makes an error at a position with a cause.
func wrap(at sqlparse.At, cause error, format string, a ...any) error {
	return &sqlparse.Error{At: at, Msg: fmt.Sprintf(format, a...), Err: cause}
}

// engineErr turns an error of the table layer into an error with the
// position of the statement. The cause still matches with errors.Is.
func engineErr(at sqlparse.At, err error) error {
	if err == nil {
		return nil
	}
	var pe *sqlparse.Error
	if errors.As(err, &pe) {
		return err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, table.ErrExists):
		msg = "a row with this primary key exists: " + msg
	case errors.Is(err, table.ErrUnique):
		msg = "UNIQUE: " + msg
	}
	return &sqlparse.Error{At: at, Msg: msg, Err: err}
}

func run(tx *table.Tx, st sqlparse.Statement, params []any, lim Limits) (Result, error) {
	switch s := st.(type) {
	case *sqlparse.CreateTable:
		return Result{}, createTable(tx, s)
	case *sqlparse.CreateIndex:
		return Result{}, createIndex(tx, s)
	case *sqlparse.DropTable:
		return Result{}, dropTable(tx, s)
	case *sqlparse.DropIndex:
		return Result{}, dropIndex(tx, s)
	case *sqlparse.AddColumn:
		return Result{}, addColumn(tx, s)
	case *sqlparse.Insert:
		return insert(tx, s, params)
	case *sqlparse.Update:
		return update(tx, s, params, lim)
	case *sqlparse.Delete:
		return deleteRows(tx, s, params, lim)
	}
	return Result{}, wrap(st.Pos(), ErrStatement, "%s", st)
}

// hidden reports whether a table has the hidden key.
func hidden(t *table.Table) bool {
	return len(t.Key) == 1 && t.Columns[t.Key[0]].Name == HiddenKey
}

// autoKey returns the column that takes the next key when an INSERT
// gives it no value. It is the hidden key, or an INTEGER key of one
// column.
func autoKey(t *table.Table) (int, bool) {
	if len(t.Key) == 1 && t.Columns[t.Key[0]].Type == table.Int64 {
		return t.Key[0], true
	}
	return 0, false
}

// visible returns the columns that * and an INSERT without a column list
// name: all but the hidden key.
func visible(t *table.Table) []int {
	var cs []int
	for i, c := range t.Columns {
		if !(c.Name == HiddenKey && hidden(t)) {
			cs = append(cs, i)
		}
	}
	return cs
}

// tableScope resolves the columns of one table.
type tableScope struct{ t *table.Table }

func (s tableScope) Column(tbl, name string) (int, table.Type, error) {
	if tbl != "" && tbl != s.t.Name {
		return 0, 0, fmt.Errorf("no such column: %s.%s", tbl, name)
	}
	for i, c := range s.t.Columns {
		if c.Name == name {
			return i, c.Type, nil
		}
	}
	return 0, 0, fmt.Errorf("no such column: %s", name)
}

func findTable(tx *table.Tx, at sqlparse.At, name string) (*table.Table, error) {
	t, ok := tx.Schema().Table(name)
	if !ok {
		return nil, wrap(at, table.ErrNoTable, "no such table: %s", name)
	}
	return t, nil
}

// indexOwner returns the table that has an index of this name. Index
// names are unique across tables, as in SQLite.
func indexOwner(s *table.Schema, name string) (*table.Table, bool) {
	for i := range s.Tables {
		if _, ok := s.Tables[i].Index(name); ok {
			return &s.Tables[i], true
		}
	}
	return nil, false
}

func checkColumnName(at sqlparse.At, name string) error {
	if strings.EqualFold(name, HiddenKey) {
		return errAt(at, "the column name %s is reserved for the key of a table without PRIMARY KEY", HiddenKey)
	}
	return nil
}

func createTable(tx *table.Tx, s *sqlparse.CreateTable) error {
	sc := tx.Schema()
	if _, ok := sc.Table(s.Name); ok {
		if s.IfNotExists {
			return nil
		}
		return wrap(s.At, table.ErrSchema, "table %s exists", s.Name)
	}
	if _, ok := indexOwner(sc, s.Name); ok {
		return wrap(s.At, table.ErrSchema, "an index is called %s", s.Name)
	}
	inKey := map[string]bool{}
	for _, k := range s.Key {
		inKey[k] = true
	}
	d := table.Def{Name: s.Name, Key: s.Key}
	if len(s.Key) == 0 {
		d.Columns = append(d.Columns, table.Column{Name: HiddenKey, Type: table.Int64})
		d.Key = []string{HiddenKey}
	}
	for _, c := range s.Columns {
		if err := checkColumnName(c.At, c.Name); err != nil {
			return err
		}
		d.Columns = append(d.Columns, table.Column{Name: c.Name, Type: sqlTypes[c.Type], Null: !c.NotNull && !inKey[c.Name]})
	}
	return engineErr(s.At, tx.CreateTable(d))
}

func createIndex(tx *table.Tx, s *sqlparse.CreateIndex) error {
	sc := tx.Schema()
	if _, ok := indexOwner(sc, s.Name); ok {
		if s.IfNotExists {
			return nil
		}
		return wrap(s.At, table.ErrSchema, "index %s exists", s.Name)
	}
	if _, ok := sc.Table(s.Name); ok {
		return wrap(s.At, table.ErrSchema, "a table is called %s", s.Name)
	}
	if _, err := findTable(tx, s.At, s.Table); err != nil {
		return err
	}
	return engineErr(s.At, tx.CreateIndex(s.Table, table.IndexDef{Name: s.Name, Columns: s.Columns, Unique: s.Unique}))
}

func dropTable(tx *table.Tx, s *sqlparse.DropTable) error {
	if _, ok := tx.Schema().Table(s.Name); !ok {
		if s.IfExists {
			return nil
		}
		return wrap(s.At, table.ErrNoTable, "no such table: %s", s.Name)
	}
	return engineErr(s.At, tx.DropTable(s.Name))
}

func dropIndex(tx *table.Tx, s *sqlparse.DropIndex) error {
	t, ok := indexOwner(tx.Schema(), s.Name)
	if !ok {
		if s.IfExists {
			return nil
		}
		return wrap(s.At, table.ErrNoIndex, "no such index: %s", s.Name)
	}
	return engineErr(s.At, tx.DropIndex(t.Name, s.Name))
}

func addColumn(tx *table.Tx, s *sqlparse.AddColumn) error {
	if _, err := findTable(tx, s.At, s.Table); err != nil {
		return err
	}
	if err := checkColumnName(s.Column.At, s.Column.Name); err != nil {
		return err
	}
	if s.Column.NotNull {
		return errAt(s.Column.At, "column %s: an added column must allow NULL; the rows that exist have no value for it", s.Column.Name)
	}
	return engineErr(s.At, tx.AddColumn(s.Table, table.Column{Name: s.Column.Name, Type: sqlTypes[s.Column.Type], Null: true}))
}

// assignable reports whether a value of type from can go into a column
// of type to. It can if the type is the same, or if an INTEGER goes into
// a REAL column.
func assignable(from, to table.Type) bool {
	return from == to || from == table.Int64 && to == table.Float64
}

// target is a column that a statement writes, with the expression that
// gives its value.
type target struct {
	col  int
	expr *Expr
	at   sqlparse.At
	desc string
}

func newTarget(t *table.Table, col int, e sqlparse.Expr, r Resolver) (target, error) {
	x, err := Compile(e, r)
	if err != nil {
		return target{}, err
	}
	c := t.Columns[col]
	if typ, ok := x.Type(); ok && !assignable(typ, c.Type) {
		return target{}, errAt(e.Pos(), "column %s is %s, and %s is %s", c.Name, TypeName(c.Type), e, TypeName(typ))
	}
	return target{col: col, expr: x, at: e.Pos(), desc: e.String()}, nil
}

// value runs the expression of a target and fits the value to its
// column. An INTEGER goes into a REAL column only if a REAL holds it
// exactly.
func (tg target) value(t *table.Table, row, params []any) (any, error) {
	v, err := tg.expr.Eval(row, params)
	if err != nil || v == nil {
		return v, err
	}
	c := t.Columns[tg.col]
	from, _ := typeOf(v)
	if !assignable(from, c.Type) {
		return nil, errAt(tg.at, "column %s is %s, and %s is %s", c.Name, TypeName(c.Type), tg.desc, TypeName(from))
	}
	if i, ok := v.(int64); ok && c.Type == table.Float64 {
		f := float64(i)
		if f >= 0x1p63 || int64(f) != i {
			return nil, errAt(tg.at, "column %s is REAL, and %d has no exact REAL value", c.Name, i)
		}
		return f, nil
	}
	return v, nil
}

// checkNull fails for a NULL in a column that does not allow it.
func checkNull(t *table.Table, row []any, at sqlparse.At) error {
	for i, c := range t.Columns {
		if row[i] == nil && !c.Null {
			return wrap(at, table.ErrValue, "column %s.%s is NOT NULL", t.Name, c.Name)
		}
	}
	return nil
}

// nextKey returns the largest key plus one, or 1 in an empty table. As
// in SQLite, the largest key -5 gives -4.
func nextKey(tx *table.Tx, t *table.Table, col int, at sqlparse.At) (int64, error) {
	rows, err := tx.Scan(t.Name, table.Options{Reverse: true})
	if err != nil {
		return 0, engineErr(at, err)
	}
	if !rows.Next() {
		return 1, engineErr(at, rows.Err())
	}
	last := rows.Row()[col].(int64)
	if last == math.MaxInt64 {
		return 0, errAt(at, "table %s: the largest key is %d; there is no next one", t.Name, last)
	}
	return last + 1, nil
}

func insert(tx *table.Tx, s *sqlparse.Insert, params []any) (Result, error) {
	t, err := findTable(tx, s.At, s.Table)
	if err != nil {
		return Result{}, err
	}
	cols := visible(t)
	if len(s.Columns) > 0 {
		cols = cols[:0:0]
		seen := map[int]bool{}
		for _, name := range s.Columns {
			i, _, err := tableScope{t}.Column("", name)
			if err != nil {
				return Result{}, errAt(s.At, "table %s: %v", t.Name, err)
			}
			if seen[i] {
				return Result{}, errAt(s.At, "column %s twice", name)
			}
			seen[i] = true
			cols = append(cols, i)
		}
	}
	rows := make([][]target, len(s.Rows))
	for i, r := range s.Rows {
		if len(r) != len(cols) {
			return Result{}, errAt(r[0].Pos(), "%d values for %d columns", len(r), len(cols))
		}
		for j, e := range r {
			tg, err := newTarget(t, cols[j], e, nil)
			if err != nil {
				return Result{}, err
			}
			rows[i] = append(rows[i], tg)
		}
	}
	var res Result
	auto, hasAuto := autoKey(t)
	for i, targets := range rows {
		row := make([]any, len(t.Columns))
		for _, tg := range targets {
			if row[tg.col], err = tg.value(t, nil, params); err != nil {
				return Result{}, err
			}
		}
		at := s.Rows[i][0].Pos()
		if hasAuto && row[auto] == nil {
			if row[auto], err = nextKey(tx, t, auto, at); err != nil {
				return Result{}, err
			}
		}
		if err := checkNull(t, row, at); err != nil {
			return Result{}, err
		}
		if err := tx.Insert(t.Name, row); err != nil {
			return Result{}, engineErr(at, err)
		}
		res.RowsAffected++
		if hasAuto {
			res.LastInsertID = row[auto].(int64)
		}
	}
	return res, nil
}

// where compiles a WHERE clause over a table. nil keeps every row.
func where(e sqlparse.Expr, t *table.Table) (*Expr, error) {
	if e == nil {
		return nil, nil
	}
	x, err := Compile(e, tableScope{t})
	if err != nil {
		return nil, err
	}
	if typ, ok := x.Type(); ok && typ != table.Bool {
		return nil, errAt(e.Pos(), "WHERE needs BOOLEAN, and %s is %s", e, TypeName(typ))
	}
	return x, nil
}

// keeps runs a WHERE clause on a row. Only TRUE keeps it.
func keeps(w *Expr, e sqlparse.Expr, row, params []any) (bool, error) {
	if w == nil {
		return true, nil
	}
	v, err := w.Eval(row, params)
	if err != nil || v == nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		t, _ := typeOf(v)
		return false, errAt(e.Pos(), "WHERE needs BOOLEAN, and %s is %s", e, TypeName(t))
	}
	return b, nil
}

// matching reads the rows of a table that WHERE keeps. It reads all of
// them before the statement writes one: a write during a scan leaves the
// scan undefined. The plan narrows the rows it reads, and the bound of
// memory counts the rows it keeps.
func matching(tx *table.Tx, t *table.Table, w *Expr, e sqlparse.Expr, at sqlparse.At, params []any, lim Limits, scanned *int64) ([][]any, error) {
	var a *access
	if !lim.NoIndex {
		a = choose(t, constraints(e, tableScope{t}))
	}
	read, err := readRows(tx, t, a, nil, params, at, scanned)
	if err != nil {
		return nil, err
	}
	mem := memory{max: lim.maxMemory(), at: at}
	var out [][]any
	for {
		row, ok, err := read()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		keep, err := keeps(w, e, row, params)
		if err != nil {
			return nil, err
		}
		if keep {
			if err := mem.add(row); err != nil {
				return nil, err
			}
			out = append(out, row)
		}
	}
}

func keyOf(t *table.Table, row []any) []any {
	k := make([]any, len(t.Key))
	for i, c := range t.Key {
		k[i] = row[c]
	}
	return k
}

func sameKey(t *table.Table, a, b []any) bool {
	for _, c := range t.Key {
		d, ok := compare(a[c], b[c])
		if !ok || d != 0 {
			return false
		}
	}
	return true
}

func update(tx *table.Tx, s *sqlparse.Update, params []any, lim Limits) (Result, error) {
	t, err := findTable(tx, s.At, s.Table)
	if err != nil {
		return Result{}, err
	}
	scope := tableScope{t}
	var sets []target
	seen := map[int]bool{}
	for _, a := range s.Set {
		col, _, err := scope.Column("", a.Column)
		if err != nil {
			return Result{}, errAt(a.At, "table %s: %v", t.Name, err)
		}
		if seen[col] {
			return Result{}, errAt(a.At, "column %s set twice", a.Column)
		}
		seen[col] = true
		tg, err := newTarget(t, col, a.Value, scope)
		if err != nil {
			return Result{}, err
		}
		sets = append(sets, tg)
	}
	w, err := where(s.Where, t)
	if err != nil {
		return Result{}, err
	}
	var scanned int64
	old, err := matching(tx, t, w, s.Where, s.At, params, lim, &scanned)
	if err != nil {
		return Result{}, err
	}
	// Every new row comes from its old row, before any row is written.
	news := make([][]any, len(old))
	for i, row := range old {
		n := append([]any(nil), row...)
		for _, tg := range sets {
			if n[tg.col], err = tg.value(t, row, params); err != nil {
				return Result{}, err
			}
		}
		if err := checkNull(t, n, s.At); err != nil {
			return Result{}, err
		}
		news[i] = n
	}
	for i, n := range news {
		if sameKey(t, old[i], n) {
			err = tx.Update(t.Name, n)
		} else if _, err = tx.Delete(t.Name, keyOf(t, old[i])...); err == nil {
			err = tx.Insert(t.Name, n)
		}
		if err != nil {
			return Result{}, engineErr(s.At, err)
		}
	}
	return Result{RowsAffected: int64(len(old)), scanned: scanned}, nil
}

func deleteRows(tx *table.Tx, s *sqlparse.Delete, params []any, lim Limits) (Result, error) {
	t, err := findTable(tx, s.At, s.Table)
	if err != nil {
		return Result{}, err
	}
	w, err := where(s.Where, t)
	if err != nil {
		return Result{}, err
	}
	var scanned int64
	rows, err := matching(tx, t, w, s.Where, s.At, params, lim, &scanned)
	if err != nil {
		return Result{}, err
	}
	for _, row := range rows {
		if _, err := tx.Delete(t.Name, keyOf(t, row)...); err != nil {
			return Result{}, engineErr(s.At, err)
		}
	}
	return Result{RowsAffected: int64(len(rows)), scanned: scanned}, nil
}

// Params turns the Go values of a program into values of SQL: the
// integer types into int64, float32 into float64. Other values pass as
// they are; the run checks them.
func Params(in []any) ([]any, error) {
	out := make([]any, len(in))
	for i, v := range in {
		switch x := v.(type) {
		case int:
			out[i] = int64(x)
		case int8:
			out[i] = int64(x)
		case int16:
			out[i] = int64(x)
		case int32:
			out[i] = int64(x)
		case uint8:
			out[i] = int64(x)
		case uint16:
			out[i] = int64(x)
		case uint32:
			out[i] = int64(x)
		case uint:
			if uint64(x) > math.MaxInt64 {
				return nil, fmt.Errorf("parameter ?%d: %d is larger than INTEGER holds", i+1, x)
			}
			out[i] = int64(x)
		case uint64:
			if x > math.MaxInt64 {
				return nil, fmt.Errorf("parameter ?%d: %d is larger than INTEGER holds", i+1, x)
			}
			out[i] = int64(x)
		case float32:
			out[i] = float64(x)
		default:
			out[i] = v
		}
	}
	return out, nil
}
