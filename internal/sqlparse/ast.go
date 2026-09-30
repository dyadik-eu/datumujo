// Package sqlparse reads the SQL of stage 2 into a syntax tree and prints
// a tree back to SQL. It knows nothing of tables or values. A name that
// does not exist is an error of a later step, and so is a wrong type.
package sqlparse

// At is a position in the SQL text: line and column, both from 1. The
// column counts characters, not bytes.
type At struct{ Line, Col int }

// Pos returns the position.
func (a At) Pos() At { return a }

// Node is every node of the tree.
type Node interface {
	Pos() At
	String() string
}

// Statement is one statement.
type Statement interface {
	Node
	statement()
}

// Expr is an expression.
type Expr interface {
	Node
	expr()
}

// The column types. A type name in the SQL text becomes one of them; see
// typeNames.
const (
	TypeInteger   = "INTEGER"
	TypeReal      = "REAL"
	TypeBoolean   = "BOOLEAN"
	TypeText      = "TEXT"
	TypeBlob      = "BLOB"
	TypeTimestamp = "TIMESTAMP"
)

// Literal is a constant. Value is nil for NULL, or an int64, float64,
// string, []byte or bool.
type Literal struct {
	At
	Value any
}

// Param is a parameter. N counts from 1.
type Param struct {
	At
	N int
}

// ColumnRef names a column, with its table or alias if Table is set.
// With Table set to Derived, it stands for a value that a later step
// computes, such as an aggregate. Name is then the text of that value.
type ColumnRef struct {
	At
	Table, Name string
}

// Derived is the Table of a ColumnRef that stands for a computed value.
// No name of the SQL text can be it.
const Derived = "\x00derived"

// Unary is an operator before one operand: "-", "+" or "NOT". A minus
// directly before a number is part of the number, see Literal.
type Unary struct {
	At
	Op string
	X  Expr
}

// Binary is an operator between two operands. Op is one of OR, AND, =,
// !=, <, <=, >, >=, IS, IS NOT, LIKE, NOT LIKE, +, -, *, /, % and ||.
// The parser writes == as = and <> as !=.
type Binary struct {
	At
	Op   string
	L, R Expr
}

// In is X [NOT] IN (List...).
type In struct {
	At
	X    Expr
	List []Expr
	Not  bool
}

// Between is X [NOT] BETWEEN Lo AND Hi.
type Between struct {
	At
	X, Lo, Hi Expr
	Not       bool
}

// Call is a function call. Star is count(*). Distinct is f(DISTINCT x).
// Name is in lower case.
type Call struct {
	At
	Name     string
	Args     []Expr
	Star     bool
	Distinct bool
}

// Cast is CAST(X AS Type).
type Cast struct {
	At
	X    Expr
	Type string
}

// Case is CASE [Operand] WHEN ... THEN ... [ELSE Else] END.
type Case struct {
	At
	Operand Expr
	Whens   []When
	Else    Expr
}

// When is one branch of a Case.
type When struct {
	Cond, Result Expr
}

func (*Literal) expr()   {}
func (*Param) expr()     {}
func (*ColumnRef) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*In) expr()        {}
func (*Between) expr()   {}
func (*Call) expr()      {}
func (*Cast) expr()      {}
func (*Case) expr()      {}

// ColumnDef is a column of CREATE TABLE or ALTER TABLE ADD COLUMN.
// Default is nil without DEFAULT. Checks are the CHECK conditions of the
// column, in order.
type ColumnDef struct {
	At
	Name    string
	Type    string
	NotNull bool
	Default *Default
	Checks  []Expr
}

// Default is the DEFAULT of a column: a constant, or CURRENT_TIMESTAMP.
// Value is as the Value of a Literal, nil for NULL; with Now set it is
// nil.
type Default struct {
	At
	Value any
	Now   bool
}

// CreateTable is CREATE TABLE. A PRIMARY KEY on a column and a PRIMARY
// KEY (...) of the table both end in Key. Key is empty without one.
// Checks are the CHECK conditions of the table, not of a column.
type CreateTable struct {
	At
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	Key         []string
	Checks      []Expr
}

// CreateIndex is CREATE [UNIQUE] INDEX.
type CreateIndex struct {
	At
	Name, Table string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

// DropTable is DROP TABLE.
type DropTable struct {
	At
	Name     string
	IfExists bool
}

// DropIndex is DROP INDEX.
type DropIndex struct {
	At
	Name     string
	IfExists bool
}

// AddColumn is ALTER TABLE ... ADD COLUMN.
type AddColumn struct {
	At
	Table  string
	Column ColumnDef
}

// Insert is INSERT INTO ... VALUES. Columns is empty when the statement
// names none.
type Insert struct {
	At
	Table   string
	Columns []string
	Rows    [][]Expr
}

// Update is UPDATE ... SET ... [WHERE].
type Update struct {
	At
	Table string
	Set   []Assign
	Where Expr
}

// Assign is one column = value of an Update.
type Assign struct {
	At
	Column string
	Value  Expr
}

// Delete is DELETE FROM ... [WHERE].
type Delete struct {
	At
	Table string
	Where Expr
}

// Select is a SELECT. From is nil for a SELECT without FROM.
type Select struct {
	At
	Distinct bool
	Items    []SelectItem
	From     *TableRef
	Joins    []Join
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []Order
	Limit    Expr
	Offset   Expr
}

// SelectItem is one item of the select list: *, table.*, or an
// expression with an optional alias.
type SelectItem struct {
	At
	Star  bool
	Table string // for table.*
	Expr  Expr
	Alias string
}

// TableRef is a table in FROM or JOIN, with an optional alias.
type TableRef struct {
	At
	Name, Alias string
}

// Join is [INNER] JOIN or LEFT [OUTER] JOIN.
type Join struct {
	At
	Left  bool
	Table TableRef
	On    Expr
}

// Order is one expression of ORDER BY.
type Order struct {
	Expr Expr
	Desc bool
}

// Begin, Commit and Rollback control the transaction.
type (
	Begin    struct{ At }
	Commit   struct{ At }
	Rollback struct{ At }
)

func (*CreateTable) statement() {}
func (*CreateIndex) statement() {}
func (*DropTable) statement()   {}
func (*DropIndex) statement()   {}
func (*AddColumn) statement()   {}
func (*Insert) statement()      {}
func (*Update) statement()      {}
func (*Delete) statement()      {}
func (*Select) statement()      {}
func (*Begin) statement()       {}
func (*Commit) statement()      {}
func (*Rollback) statement()    {}
