package sqlparse

import (
	"encoding/hex"
	"math"
	"strconv"
)

// MaxDepth bounds the nesting of expressions. A deeper expression is an
// error, not a stack overflow.
const MaxDepth = 200

// MaxParam is the largest parameter number, as in SQLite.
const MaxParam = 32766

// typeNames maps the type names of the SQL text to the column types. A
// name is a word, not a keyword, so a column can be called "text".
var typeNames = map[string]string{
	"integer": TypeInteger, "int": TypeInteger, "bigint": TypeInteger,
	"real": TypeReal, "double": TypeReal, "float": TypeReal,
	"boolean": TypeBoolean, "bool": TypeBoolean,
	"text": TypeText, "varchar": TypeText,
	"blob": TypeBlob, "bytea": TypeBlob,
	"timestamp": TypeTimestamp,
}

type parser struct {
	lx     lexer
	tok    token
	depth  int
	params int // the largest parameter number of the statement so far
}

// Parse reads exactly one statement. A semicolon after it is allowed.
func Parse(src string) (Statement, error) {
	all, err := ParseAll(src)
	if err != nil {
		return nil, err
	}
	if len(all) != 1 {
		return nil, errAt(At{1, 1}, "%d statements, want exactly one", len(all))
	}
	return all[0], nil
}

// ParseAll reads the statements of src, separated by semicolons.
func ParseAll(src string) ([]Statement, error) {
	p := &parser{lx: lexer{src: src, line: 1, col: 1}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	var all []Statement
	for {
		for p.isOp(";") {
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		if p.tok.kind == tEOF {
			return all, nil
		}
		p.params = 0
		s, err := p.statement()
		if err != nil {
			return nil, err
		}
		all = append(all, s)
		if p.tok.kind != tEOF && !p.isOp(";") {
			return nil, p.unexpected("; or the end")
		}
	}
}

func (p *parser) advance() error {
	t, err := p.lx.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

// peek returns the token after the current one, without moving.
func (p *parser) peek() token {
	lx := p.lx
	t, err := lx.next()
	if err != nil {
		return token{kind: tEOF}
	}
	return t
}

func (p *parser) isKw(k string) bool { return p.tok.kind == tKeyword && p.tok.text == k }
func (p *parser) isOp(o string) bool { return p.tok.kind == tOp && p.tok.text == o }

// isWord reports a word that is a keyword only in its place, such as KEY
// or INDEX. Everywhere else it is a name.
func (p *parser) isWord(w string) bool { return p.tok.kind == tIdent && p.tok.text == w }

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "the end"
	case tString:
		return "a string"
	case tBlob:
		return "a blob"
	case tInt, tFloat:
		return "number " + t.text
	case tParam:
		return "a parameter"
	case tQuoted:
		return strconv.Quote(t.text)
	}
	return t.text
}

func (p *parser) unexpected(want string) error {
	return errAt(p.tok.at, "%s, want %s", describe(p.tok), want)
}

// accept moves past keyword k if it is the current token.
func (p *parser) accept(k string) (bool, error) {
	if !p.isKw(k) {
		return false, nil
	}
	return true, p.advance()
}

func (p *parser) acceptWord(w string) (bool, error) {
	if !p.isWord(w) {
		return false, nil
	}
	return true, p.advance()
}

func (p *parser) expect(k string) error {
	if !p.isKw(k) {
		return p.unexpected(k)
	}
	return p.advance()
}

func (p *parser) expectWord(w string) error {
	if !p.isWord(w) {
		return p.unexpected(w)
	}
	return p.advance()
}

func (p *parser) expectOp(o string) error {
	if !p.isOp(o) {
		return p.unexpected(o)
	}
	return p.advance()
}

// name reads a table, column, index or alias name.
func (p *parser) name(what string) (string, error) {
	if p.tok.kind != tIdent && p.tok.kind != tQuoted {
		if p.tok.kind == tKeyword {
			return "", errAt(p.tok.at, "%s is a keyword, want %s; write it in double quotes to use it as a name", p.tok.text, what)
		}
		return "", p.unexpected(what)
	}
	n := p.tok.text
	return n, p.advance()
}

// names reads ( name, ... ).
func (p *parser) names(what string) ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var ns []string
	for {
		n, err := p.name(what)
		if err != nil {
			return nil, err
		}
		ns = append(ns, n)
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	return ns, p.expectOp(")")
}

func (p *parser) statement() (Statement, error) {
	at := p.tok.at
	switch {
	case p.isKw("SELECT"):
		return p.selectStmt()
	case p.isKw("INSERT"):
		return p.insert()
	case p.isKw("UPDATE"):
		return p.update()
	case p.isKw("DELETE"):
		return p.delete()
	case p.isKw("CREATE"):
		return p.create()
	case p.isKw("DROP"):
		return p.drop()
	case p.isKw("ALTER"):
		return p.alter()
	case p.isWord("begin"), p.isWord("commit"), p.isWord("rollback"):
		w := p.tok.text
		if err := p.advance(); err != nil {
			return nil, err
		}
		if _, err := p.acceptWord("transaction"); err != nil {
			return nil, err
		}
		switch w {
		case "begin":
			return &Begin{at}, nil
		case "commit":
			return &Commit{at}, nil
		}
		return &Rollback{at}, nil
	}
	return nil, p.unexpected("a statement")
}

// ifExists reads IF EXISTS, or IF NOT EXISTS with not set.
func (p *parser) ifExists(not bool) (bool, error) {
	if !p.isWord("if") {
		return false, nil
	}
	if err := p.advance(); err != nil {
		return false, err
	}
	if not {
		if err := p.expect("NOT"); err != nil {
			return false, err
		}
	}
	return true, p.expectWord("exists")
}

func (p *parser) create() (Statement, error) {
	at := p.tok.at
	if err := p.advance(); err != nil {
		return nil, err
	}
	unique, err := p.accept("UNIQUE")
	if err != nil {
		return nil, err
	}
	if unique || p.isWord("index") {
		if err := p.expectWord("index"); err != nil {
			return nil, err
		}
		ix := &CreateIndex{At: at, Unique: unique}
		if ix.IfNotExists, err = p.ifExists(true); err != nil {
			return nil, err
		}
		if ix.Name, err = p.name("an index name"); err != nil {
			return nil, err
		}
		if err := p.expect("ON"); err != nil {
			return nil, err
		}
		if ix.Table, err = p.name("a table name"); err != nil {
			return nil, err
		}
		if ix.Columns, err = p.names("a column name"); err != nil {
			return nil, err
		}
		return ix, nil
	}
	if err := p.expect("TABLE"); err != nil {
		return nil, err
	}
	ct := &CreateTable{At: at}
	if ct.IfNotExists, err = p.ifExists(true); err != nil {
		return nil, err
	}
	if ct.Name, err = p.name("a table name"); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var keyAt At
	setKey := func(at At, cols []string) error {
		if ct.Key != nil {
			return errAt(at, "a second primary key; the first is at line %d, column %d", keyAt.Line, keyAt.Col)
		}
		ct.Key, keyAt = cols, at
		return nil
	}
	for {
		// CHECK ( starts a check of the table. A column called check
		// has a type after its name, never (.
		if p.isWord("check") && p.peek().kind == tOp && p.peek().text == "(" {
			e, err := p.check()
			if err != nil {
				return nil, err
			}
			ct.Checks = append(ct.Checks, e)
		} else if p.isKw("PRIMARY") {
			at := p.tok.at
			if err := p.advance(); err != nil {
				return nil, err
			}
			if err := p.expectWord("key"); err != nil {
				return nil, err
			}
			cols, err := p.names("a column name")
			if err != nil {
				return nil, err
			}
			if err := setKey(at, cols); err != nil {
				return nil, err
			}
		} else {
			c, pk, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			if pk != nil {
				if err := setKey(*pk, []string{c.Name}); err != nil {
					return nil, err
				}
			}
			ct.Columns = append(ct.Columns, c)
		}
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if len(ct.Columns) == 0 {
		return nil, errAt(at, "table %s has no column", ct.Name)
	}
	return ct, p.expectOp(")")
}

// columnDef reads name type [NOT NULL | NULL | PRIMARY KEY | DEFAULT d |
// CHECK (e)] .... It returns the position of PRIMARY KEY if the column
// has it.
func (p *parser) columnDef() (ColumnDef, *At, error) {
	c := ColumnDef{At: p.tok.at}
	var err error
	if c.Name, err = p.name("a column name"); err != nil {
		return c, nil, err
	}
	if c.Type, err = p.typeName(); err != nil {
		return c, nil, err
	}
	var pk *At
	for {
		at := p.tok.at
		switch {
		case p.isKw("NOT"):
			if err := p.advance(); err != nil {
				return c, nil, err
			}
			if err := p.expect("NULL"); err != nil {
				return c, nil, err
			}
			c.NotNull = true
		case p.isKw("NULL"):
			if err := p.advance(); err != nil {
				return c, nil, err
			}
		case p.isKw("PRIMARY"):
			if err := p.advance(); err != nil {
				return c, nil, err
			}
			if err := p.expectWord("key"); err != nil {
				return c, nil, err
			}
			if pk != nil {
				return c, nil, errAt(at, "column %s: PRIMARY KEY twice", c.Name)
			}
			pk = &at
		case p.isWord("default"):
			if c.Default != nil {
				return c, nil, errAt(at, "column %s: DEFAULT twice", c.Name)
			}
			if err := p.advance(); err != nil {
				return c, nil, err
			}
			if c.Default, err = p.defaultValue(); err != nil {
				return c, nil, err
			}
		case p.isWord("check"):
			e, err := p.check()
			if err != nil {
				return c, nil, err
			}
			c.Checks = append(c.Checks, e)
		default:
			return c, pk, nil
		}
	}
}

// check reads CHECK (e) and returns e.
func (p *parser) check() (Expr, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	return e, p.expectOp(")")
}

// ParseExpr reads one expression, as Expr.String writes it. The schema
// keeps a CHECK as this text.
func ParseExpr(src string) (Expr, error) {
	p := &parser{lx: lexer{src: src, line: 1, col: 1}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	e, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tEOF {
		return nil, p.unexpected("the end of the expression")
	}
	return e, nil
}

// defaultValue reads the value after DEFAULT: a number with an optional
// sign, a string, a blob, NULL, TRUE, FALSE or CURRENT_TIMESTAMP. An
// expression, also in parentheses, is not a default here (L-13).
func (p *parser) defaultValue() (*Default, error) {
	at := p.tok.at
	lit := func(e Expr, err error) (*Default, error) {
		if err != nil {
			return nil, err
		}
		return &Default{At: at, Value: e.(*Literal).Value}, nil
	}
	switch {
	case p.isOp("-") || p.isOp("+"):
		negative := p.isOp("-")
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind != tInt && p.tok.kind != tFloat {
			return nil, p.unexpected("a number after the sign of a DEFAULT")
		}
		return lit(p.number(at, negative))
	case p.tok.kind == tInt || p.tok.kind == tFloat || p.tok.kind == tString || p.tok.kind == tBlob,
		p.isKw("NULL") || p.isKw("TRUE") || p.isKw("FALSE"):
		return lit(p.primary())
	case p.isWord("current_timestamp"):
		return &Default{At: at, Now: true}, p.advance()
	}
	return nil, p.unexpected("a constant or CURRENT_TIMESTAMP after DEFAULT")
}

// ParseDefault reads the text of a default, as Default.String writes it.
// The schema keeps a default as this text.
func ParseDefault(src string) (*Default, error) {
	p := &parser{lx: lexer{src: src, line: 1, col: 1}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	d, err := p.defaultValue()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tEOF {
		return nil, p.unexpected("the end of the default")
	}
	return d, nil
}

// typeName reads a type name and returns its column type.
func (p *parser) typeName() (string, error) {
	at := p.tok.at
	if p.tok.kind != tIdent {
		return "", p.unexpected("a type: INTEGER, REAL, BOOLEAN, TEXT, BLOB or TIMESTAMP")
	}
	w := p.tok.text
	t, ok := typeNames[w]
	if !ok {
		return "", errAt(at, "unknown type %s, want INTEGER, REAL, BOOLEAN, TEXT, BLOB or TIMESTAMP", w)
	}
	if err := p.advance(); err != nil {
		return "", err
	}
	if w == "double" {
		if _, err := p.acceptWord("precision"); err != nil {
			return "", err
		}
	}
	if p.isOp("(") {
		return "", errAt(p.tok.at, "type %s takes no length or size; the column holds any %s value", w, t)
	}
	return t, nil
}

func (p *parser) drop() (Statement, error) {
	at := p.tok.at
	if err := p.advance(); err != nil {
		return nil, err
	}
	index := p.isWord("index")
	if index {
		if err := p.advance(); err != nil {
			return nil, err
		}
	} else if err := p.expect("TABLE"); err != nil {
		return nil, err
	}
	ifExists, err := p.ifExists(false)
	if err != nil {
		return nil, err
	}
	if index {
		n, err := p.name("an index name")
		return &DropIndex{At: at, Name: n, IfExists: ifExists}, err
	}
	n, err := p.name("a table name")
	return &DropTable{At: at, Name: n, IfExists: ifExists}, err
}

func (p *parser) alter() (Statement, error) {
	at := p.tok.at
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expect("TABLE"); err != nil {
		return nil, err
	}
	t, err := p.name("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectWord("add"); err != nil {
		return nil, err
	}
	if _, err := p.acceptWord("column"); err != nil {
		return nil, err
	}
	c, pk, err := p.columnDef()
	if err != nil {
		return nil, err
	}
	if pk != nil {
		return nil, errAt(*pk, "an added column cannot be part of the primary key")
	}
	return &AddColumn{At: at, Table: t, Column: c}, nil
}

func (p *parser) insert() (Statement, error) {
	ins := &Insert{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expect("INTO"); err != nil {
		return nil, err
	}
	var err error
	if ins.Table, err = p.name("a table name"); err != nil {
		return nil, err
	}
	if p.isOp("(") {
		if ins.Columns, err = p.names("a column name"); err != nil {
			return nil, err
		}
	}
	if err := p.expect("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		row, err := p.exprList()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if !p.isOp(",") {
			return ins, nil
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
}

func (p *parser) exprList() ([]Expr, error) {
	var list []Expr
	for {
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if !p.isOp(",") {
			return list, nil
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
}

// where reads WHERE expr if it comes.
func (p *parser) where() (Expr, error) {
	if ok, err := p.accept("WHERE"); !ok || err != nil {
		return nil, err
	}
	return p.expr()
}

func (p *parser) update() (Statement, error) {
	up := &Update{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	var err error
	if up.Table, err = p.name("a table name"); err != nil {
		return nil, err
	}
	if err := p.expect("SET"); err != nil {
		return nil, err
	}
	for {
		a := Assign{At: p.tok.at}
		if a.Column, err = p.name("a column name"); err != nil {
			return nil, err
		}
		if !p.isOp("=") && !p.isOp("==") {
			return nil, p.unexpected("=")
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		if a.Value, err = p.expr(); err != nil {
			return nil, err
		}
		up.Set = append(up.Set, a)
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	up.Where, err = p.where()
	return up, err
}

func (p *parser) delete() (Statement, error) {
	del := &Delete{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expect("FROM"); err != nil {
		return nil, err
	}
	var err error
	if del.Table, err = p.name("a table name"); err != nil {
		return nil, err
	}
	del.Where, err = p.where()
	return del, err
}

// alias reads [AS] name. Without AS, only a word that is not a keyword
// can be an alias.
func (p *parser) alias() (string, error) {
	as, err := p.accept("AS")
	if err != nil {
		return "", err
	}
	if as || p.tok.kind == tIdent || p.tok.kind == tQuoted {
		return p.name("an alias")
	}
	return "", nil
}

func (p *parser) tableRef() (TableRef, error) {
	r := TableRef{At: p.tok.at}
	var err error
	if r.Name, err = p.name("a table name"); err != nil {
		return r, err
	}
	r.Alias, err = p.alias()
	return r, err
}

func (p *parser) selectStmt() (Statement, error) {
	s := &Select{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	var err error
	if s.Distinct, err = p.accept("DISTINCT"); err != nil {
		return nil, err
	}
	for {
		item, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if ok, err := p.accept("FROM"); err != nil {
		return nil, err
	} else if ok {
		from, err := p.tableRef()
		if err != nil {
			return nil, err
		}
		s.From = &from
		for p.isKw("JOIN") || p.isKw("INNER") || p.isKw("LEFT") {
			j := Join{At: p.tok.at, Left: p.isKw("LEFT")}
			if !p.isKw("JOIN") {
				if err := p.advance(); err != nil {
					return nil, err
				}
				if j.Left {
					if _, err := p.accept("OUTER"); err != nil {
						return nil, err
					}
				}
			}
			if err := p.expect("JOIN"); err != nil {
				return nil, err
			}
			if j.Table, err = p.tableRef(); err != nil {
				return nil, err
			}
			if err := p.expect("ON"); err != nil {
				return nil, err
			}
			if j.On, err = p.expr(); err != nil {
				return nil, err
			}
			s.Joins = append(s.Joins, j)
		}
	}
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	if p.isKw("GROUP") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		if s.GroupBy, err = p.exprList(); err != nil {
			return nil, err
		}
	}
	if ok, err := p.accept("HAVING"); err != nil {
		return nil, err
	} else if ok {
		if s.Having, err = p.expr(); err != nil {
			return nil, err
		}
	}
	if p.isKw("ORDER") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.expect("BY"); err != nil {
			return nil, err
		}
		for {
			var o Order
			if o.Expr, err = p.expr(); err != nil {
				return nil, err
			}
			if o.Desc, err = p.accept("DESC"); err != nil {
				return nil, err
			}
			if !o.Desc {
				if _, err := p.accept("ASC"); err != nil {
					return nil, err
				}
			}
			s.OrderBy = append(s.OrderBy, o)
			if !p.isOp(",") {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	if ok, err := p.accept("LIMIT"); err != nil {
		return nil, err
	} else if ok {
		if s.Limit, err = p.expr(); err != nil {
			return nil, err
		}
		if ok, err := p.accept("OFFSET"); err != nil {
			return nil, err
		} else if ok {
			if s.Offset, err = p.expr(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (p *parser) selectItem() (SelectItem, error) {
	item := SelectItem{At: p.tok.at}
	if p.isOp("*") {
		item.Star = true
		return item, p.advance()
	}
	if p.tok.kind == tIdent || p.tok.kind == tQuoted {
		lx := p.lx
		if dot, err := lx.next(); err == nil && dot.kind == tOp && dot.text == "." {
			if star, err := lx.next(); err == nil && star.kind == tOp && star.text == "*" {
				item.Star, item.Table = true, p.tok.text
				p.lx = lx
				return item, p.advance()
			}
		}
	}
	var err error
	if item.Expr, err = p.expr(); err != nil {
		return item, err
	}
	item.Alias, err = p.alias()
	return item, err
}

// The expressions, from the lowest precedence to the highest: OR, AND,
// NOT, the equality level (= != IS IN BETWEEN LIKE), < <= > >=, + -,
// * / %, || and the unary operators. This is the order of SQLite.

func (p *parser) expr() (Expr, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > MaxDepth {
		return nil, errAt(p.tok.at, "the expression is nested more than %d levels deep", MaxDepth)
	}
	return p.or()
}

func (p *parser) or() (Expr, error) {
	l, err := p.and()
	for err == nil && p.isKw("OR") {
		at := p.tok.at
		var r Expr
		if err = p.advance(); err == nil {
			if r, err = p.and(); err == nil {
				l = &Binary{At: at, Op: "OR", L: l, R: r}
			}
		}
	}
	return l, err
}

func (p *parser) and() (Expr, error) {
	l, err := p.not()
	for err == nil && p.isKw("AND") {
		at := p.tok.at
		var r Expr
		if err = p.advance(); err == nil {
			if r, err = p.not(); err == nil {
				l = &Binary{At: at, Op: "AND", L: l, R: r}
			}
		}
	}
	return l, err
}

func (p *parser) not() (Expr, error) {
	if !p.isKw("NOT") {
		return p.equality()
	}
	at := p.tok.at
	if err := p.advance(); err != nil {
		return nil, err
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > MaxDepth {
		return nil, errAt(at, "the expression is nested more than %d levels deep", MaxDepth)
	}
	x, err := p.not()
	if err != nil {
		return nil, err
	}
	return &Unary{At: at, Op: "NOT", X: x}, nil
}

func (p *parser) equality() (Expr, error) {
	l, err := p.relational()
	if err != nil {
		return nil, err
	}
	for {
		at := p.tok.at
		switch {
		case p.isOp("=") || p.isOp("==") || p.isOp("!=") || p.isOp("<>"):
			op := p.tok.text
			switch op {
			case "==":
				op = "="
			case "<>":
				op = "!="
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			r, err := p.relational()
			if err != nil {
				return nil, err
			}
			l = &Binary{At: at, Op: op, L: l, R: r}
		case p.isKw("IS"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			op := "IS"
			if ok, err := p.accept("NOT"); err != nil {
				return nil, err
			} else if ok {
				op = "IS NOT"
			}
			r, err := p.relational()
			if err != nil {
				return nil, err
			}
			l = &Binary{At: at, Op: op, L: l, R: r}
		case p.isKw("NOT") || p.isKw("IN") || p.isKw("BETWEEN") || p.isKw("LIKE"):
			not := p.isKw("NOT")
			if not {
				if err := p.advance(); err != nil {
					return nil, err
				}
			}
			switch {
			case p.isKw("IN"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				list, err := p.exprList()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				// The list ends in a parenthesis. So an operator that
				// binds more tightly takes the whole IN as its left
				// operand. As in SQLite, a IN (1) + 2 is (a IN (1)) + 2.
				if l, err = p.climb(&In{At: at, X: l, List: list, Not: not}); err != nil {
					return nil, err
				}
			case p.isKw("BETWEEN"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				lo, err := p.relational()
				if err != nil {
					return nil, err
				}
				if err := p.expect("AND"); err != nil {
					return nil, err
				}
				hi, err := p.relational()
				if err != nil {
					return nil, err
				}
				l = &Between{At: at, X: l, Lo: lo, Hi: hi, Not: not}
			case p.isKw("LIKE"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				r, err := p.relational()
				if err != nil {
					return nil, err
				}
				op := "LIKE"
				if not {
					op = "NOT LIKE"
				}
				l = &Binary{At: at, Op: op, L: l, R: r}
			default:
				return nil, p.unexpected("IN, BETWEEN or LIKE after NOT")
			}
		default:
			return l, nil
		}
	}
}

// binaryLevel reads operands of next joined by the operators ops, from
// the left.
func (p *parser) binaryLevel(next func() (Expr, error), ops ...string) (Expr, error) {
	l, err := next()
	if err != nil {
		return nil, err
	}
	for {
		op := ""
		for _, o := range ops {
			if p.isOp(o) {
				op = o
			}
		}
		if op == "" {
			return l, nil
		}
		at := p.tok.at
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := next()
		if err != nil {
			return nil, err
		}
		l = &Binary{At: at, Op: op, L: l, R: r}
	}
}

// levels are the operators above the equality level, from the lowest.
var levels = [][]string{{"<", "<=", ">", ">="}, {"+", "-"}, {"*", "/", "%"}, {"||"}}

// climb continues an expression l that is complete at the equality level
// with the operators of levels. Each right operand takes the operators
// that bind more tightly than its own.
func (p *parser) climb(l Expr) (Expr, error) {
	next := []func() (Expr, error){p.additive, p.multiplicative, p.concat, p.unary}
	for {
		level := -1
		for i, ops := range levels {
			for _, o := range ops {
				if p.isOp(o) {
					level = i
				}
			}
		}
		if level < 0 {
			return l, nil
		}
		at, op := p.tok.at, p.tok.text
		if err := p.advance(); err != nil {
			return nil, err
		}
		r, err := next[level]()
		if err != nil {
			return nil, err
		}
		l = &Binary{At: at, Op: op, L: l, R: r}
	}
}

func (p *parser) relational() (Expr, error) {
	return p.binaryLevel(p.additive, "<", "<=", ">", ">=")
}

func (p *parser) additive() (Expr, error) {
	return p.binaryLevel(p.multiplicative, "+", "-")
}

func (p *parser) multiplicative() (Expr, error) {
	return p.binaryLevel(p.concat, "*", "/", "%")
}

func (p *parser) concat() (Expr, error) {
	return p.binaryLevel(p.unary, "||")
}

// unary reads a unary minus or plus before an operand. A minus right before a number token
// makes a negative literal, so -9223372036854775808 is an int64.
func (p *parser) unary() (Expr, error) {
	if !p.isOp("-") && !p.isOp("+") {
		return p.primary()
	}
	at, op := p.tok.at, p.tok.text
	if err := p.advance(); err != nil {
		return nil, err
	}
	if op == "-" && (p.tok.kind == tInt || p.tok.kind == tFloat) {
		return p.number(at, true)
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > MaxDepth {
		return nil, errAt(at, "the expression is nested more than %d levels deep", MaxDepth)
	}
	x, err := p.unary()
	if err != nil {
		return nil, err
	}
	return &Unary{At: at, Op: op, X: x}, nil
}

// number turns the current number token into a literal at position at.
func (p *parser) number(at At, negative bool) (Expr, error) {
	t := p.tok
	if err := p.advance(); err != nil {
		return nil, err
	}
	if t.kind == tFloat {
		f, _ := strconv.ParseFloat(t.text, 64) // the lexer checked it
		if negative {
			f = -f
		}
		return &Literal{At: at, Value: f}, nil
	}
	u, err := strconv.ParseUint(t.text, 10, 64)
	switch {
	case err == nil && u <= math.MaxInt64:
		v := int64(u)
		if negative {
			v = -v
		}
		return &Literal{At: at, Value: v}, nil
	case err == nil && negative && u == 1<<63:
		return &Literal{At: at, Value: int64(math.MinInt64)}, nil
	}
	return nil, errAt(t.at, "integer %s is out of the range of INTEGER; write it as a REAL with a decimal point", t.text)
}

func (p *parser) primary() (Expr, error) {
	t := p.tok
	at := t.at
	switch t.kind {
	case tInt, tFloat:
		return p.number(at, false)
	case tString:
		return &Literal{At: at, Value: t.text}, p.advance()
	case tBlob:
		b, _ := hex.DecodeString(t.text) // the lexer checked it
		if b == nil {
			b = []byte{}
		}
		return &Literal{At: at, Value: b}, p.advance()
	case tParam:
		n := p.params + 1
		if t.text != "" {
			v, err := strconv.Atoi(t.text)
			if err != nil || v < 1 || v > MaxParam {
				return nil, errAt(at, "parameter ?%s: the number must be from 1 to %d", t.text, MaxParam)
			}
			n = v
		}
		if n > MaxParam {
			return nil, errAt(at, "more than %d parameters", MaxParam)
		}
		p.params = max(p.params, n)
		return &Param{At: at, N: n}, p.advance()
	case tKeyword:
		switch t.text {
		case "NULL":
			return &Literal{At: at}, p.advance()
		case "TRUE", "FALSE":
			return &Literal{At: at, Value: t.text == "TRUE"}, p.advance()
		case "CAST":
			return p.cast()
		case "CASE":
			return p.caseExpr()
		}
	case tOp:
		if t.text == "(" {
			if err := p.advance(); err != nil {
				return nil, err
			}
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		}
	case tIdent, tQuoted:
		if err := p.advance(); err != nil {
			return nil, err
		}
		if t.kind == tIdent && p.isOp("(") {
			return p.call(t)
		}
		if p.isOp(".") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			col, err := p.name("a column name")
			if err != nil {
				return nil, err
			}
			return &ColumnRef{At: at, Table: t.text, Name: col}, nil
		}
		return &ColumnRef{At: at, Name: t.text}, nil
	}
	return nil, p.unexpected("an expression")
}

func (p *parser) call(name token) (Expr, error) {
	c := &Call{At: name.at, Name: name.text}
	if err := p.advance(); err != nil { // (
		return nil, err
	}
	var err error
	switch {
	case p.isOp("*"):
		c.Star = true
		if err := p.advance(); err != nil {
			return nil, err
		}
	case p.isOp(")"):
	default:
		if c.Distinct, err = p.accept("DISTINCT"); err != nil {
			return nil, err
		}
		if c.Args, err = p.exprList(); err != nil {
			return nil, err
		}
	}
	return c, p.expectOp(")")
}

func (p *parser) cast() (Expr, error) {
	c := &Cast{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var err error
	if c.X, err = p.expr(); err != nil {
		return nil, err
	}
	if err := p.expect("AS"); err != nil {
		return nil, err
	}
	if c.Type, err = p.typeName(); err != nil {
		return nil, err
	}
	return c, p.expectOp(")")
}

func (p *parser) caseExpr() (Expr, error) {
	c := &Case{At: p.tok.at}
	if err := p.advance(); err != nil {
		return nil, err
	}
	var err error
	if !p.isKw("WHEN") && !p.isKw("END") {
		if c.Operand, err = p.expr(); err != nil {
			return nil, err
		}
	}
	for p.isKw("WHEN") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		var w When
		if w.Cond, err = p.expr(); err != nil {
			return nil, err
		}
		if err := p.expect("THEN"); err != nil {
			return nil, err
		}
		if w.Result, err = p.expr(); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.unexpected("WHEN")
	}
	if ok, err := p.accept("ELSE"); err != nil {
		return nil, err
	} else if ok {
		if c.Else, err = p.expr(); err != nil {
			return nil, err
		}
	}
	return c, p.expect("END")
}
