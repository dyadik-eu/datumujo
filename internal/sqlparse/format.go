package sqlparse

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"
)

// The String methods print a tree as SQL that parses to the same tree.
// Every operator gets parentheses, so the text shows how the parser
// grouped it.

// softWords are words the parser knows by their place. A name with such
// a word gets double quotes, so it cannot be read as the word.
var softWords = map[string]bool{
	"add": true, "begin": true, "column": true, "commit": true, "exists": true,
	"if": true, "index": true, "key": true, "rollback": true, "transaction": true,
	"precision": true,
}

// Name prints a name, in double quotes where a bare word would read
// differently.
func Name(n string) string {
	bare := n != "" && !keywords[strings.ToUpper(n)] && !softWords[n]
	for i := 0; i < len(n) && bare; i++ {
		c := n[i]
		bare = c == '_' || c >= 'a' && c <= 'z' || i > 0 && isDigit(c)
	}
	if bare {
		return n
	}
	return `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
}

func names(ns []string) string {
	q := make([]string, len(ns))
	for i, n := range ns {
		q[i] = Name(n)
	}
	return strings.Join(q, ", ")
}

func exprs(es []Expr) string {
	q := make([]string, len(es))
	for i, e := range es {
		q[i] = e.String()
	}
	return strings.Join(q, ", ")
}

// Quote prints a string literal.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (e *Literal) String() string {
	switch v := e.Value.(type) {
	case nil:
		return "NULL"
	case bool:
		if v {
			return "TRUE"
		}
		return "FALSE"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		s := strconv.FormatFloat(v, 'g', -1, 64)
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return "CAST(" + Quote(s) + " AS REAL)" // no literal in the tree has these
		}
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return s
	case string:
		return Quote(v)
	case []byte:
		return "X'" + hex.EncodeToString(v) + "'"
	}
	return "?" // no literal in the tree has another type
}

func (e *Param) String() string { return "?" + strconv.Itoa(e.N) }

func (e *ColumnRef) String() string {
	if e.Table == Derived {
		return e.Name
	}
	if e.Table != "" {
		return Name(e.Table) + "." + Name(e.Name)
	}
	return Name(e.Name)
}

func (e *Unary) String() string {
	x := e.X.String()
	if l, ok := e.X.(*Literal); ok && e.Op == "-" {
		// A minus right before a number would join it: -(5) is not -5.
		switch l.Value.(type) {
		case int64, float64:
			x = "(" + x + ")"
		}
	}
	return "(" + e.Op + " " + x + ")"
}

func (e *Binary) String() string {
	return "(" + e.L.String() + " " + e.Op + " " + e.R.String() + ")"
}

func not(n bool) string {
	if n {
		return "NOT "
	}
	return ""
}

func (e *In) String() string {
	return "(" + e.X.String() + " " + not(e.Not) + "IN (" + exprs(e.List) + "))"
}

func (e *Between) String() string {
	return "(" + e.X.String() + " " + not(e.Not) + "BETWEEN " + e.Lo.String() + " AND " + e.Hi.String() + ")"
}

func (e *Call) String() string {
	switch {
	case e.Star:
		return e.Name + "(*)"
	case e.Distinct:
		return e.Name + "(DISTINCT " + exprs(e.Args) + ")"
	}
	return e.Name + "(" + exprs(e.Args) + ")"
}

func (e *Cast) String() string { return "CAST(" + e.X.String() + " AS " + e.Type + ")" }

func (e *Case) String() string {
	var b strings.Builder
	b.WriteString("CASE ")
	if e.Operand != nil {
		b.WriteString(e.Operand.String() + " ")
	}
	for _, w := range e.Whens {
		b.WriteString("WHEN " + w.Cond.String() + " THEN " + w.Result.String() + " ")
	}
	if e.Else != nil {
		b.WriteString("ELSE " + e.Else.String() + " ")
	}
	b.WriteString("END")
	return b.String()
}

func (c ColumnDef) String() string {
	s := Name(c.Name) + " " + c.Type
	if c.NotNull {
		s += " NOT NULL"
	}
	return s
}

func ifNotExists(b bool) string {
	if b {
		return "IF NOT EXISTS "
	}
	return ""
}

func ifExists(b bool) string {
	if b {
		return "IF EXISTS "
	}
	return ""
}

func (s *CreateTable) String() string {
	parts := make([]string, 0, len(s.Columns)+1)
	for _, c := range s.Columns {
		parts = append(parts, c.String())
	}
	if len(s.Key) > 0 {
		parts = append(parts, "PRIMARY KEY ("+names(s.Key)+")")
	}
	return "CREATE TABLE " + ifNotExists(s.IfNotExists) + Name(s.Name) + " (" + strings.Join(parts, ", ") + ")"
}

func (s *CreateIndex) String() string {
	u := ""
	if s.Unique {
		u = "UNIQUE "
	}
	return "CREATE " + u + "INDEX " + ifNotExists(s.IfNotExists) + Name(s.Name) + " ON " + Name(s.Table) + " (" + names(s.Columns) + ")"
}

func (s *DropTable) String() string { return "DROP TABLE " + ifExists(s.IfExists) + Name(s.Name) }
func (s *DropIndex) String() string { return "DROP INDEX " + ifExists(s.IfExists) + Name(s.Name) }

func (s *AddColumn) String() string {
	return "ALTER TABLE " + Name(s.Table) + " ADD COLUMN " + s.Column.String()
}

func (s *Insert) String() string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + Name(s.Table))
	if len(s.Columns) > 0 {
		b.WriteString(" (" + names(s.Columns) + ")")
	}
	b.WriteString(" VALUES ")
	for i, r := range s.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(" + exprs(r) + ")")
	}
	return b.String()
}

func where(e Expr) string {
	if e == nil {
		return ""
	}
	return " WHERE " + e.String()
}

func (s *Update) String() string {
	set := make([]string, len(s.Set))
	for i, a := range s.Set {
		set[i] = Name(a.Column) + " = " + a.Value.String()
	}
	return "UPDATE " + Name(s.Table) + " SET " + strings.Join(set, ", ") + where(s.Where)
}

func (s *Delete) String() string { return "DELETE FROM " + Name(s.Table) + where(s.Where) }

func (r TableRef) String() string {
	if r.Alias != "" {
		return Name(r.Name) + " AS " + Name(r.Alias)
	}
	return Name(r.Name)
}

func (i SelectItem) String() string {
	switch {
	case i.Star && i.Table != "":
		return Name(i.Table) + ".*"
	case i.Star:
		return "*"
	case i.Alias != "":
		return i.Expr.String() + " AS " + Name(i.Alias)
	}
	return i.Expr.String()
}

func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if s.Distinct {
		b.WriteString("DISTINCT ")
	}
	for i, it := range s.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.String())
	}
	if s.From != nil {
		b.WriteString(" FROM " + s.From.String())
	}
	for _, j := range s.Joins {
		if j.Left {
			b.WriteString(" LEFT")
		}
		b.WriteString(" JOIN " + j.Table.String() + " ON " + j.On.String())
	}
	b.WriteString(where(s.Where))
	if len(s.GroupBy) > 0 {
		b.WriteString(" GROUP BY " + exprs(s.GroupBy))
	}
	if s.Having != nil {
		b.WriteString(" HAVING " + s.Having.String())
	}
	for i, o := range s.OrderBy {
		if i == 0 {
			b.WriteString(" ORDER BY ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(o.Expr.String())
		if o.Desc {
			b.WriteString(" DESC")
		}
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT " + s.Limit.String())
		if s.Offset != nil {
			b.WriteString(" OFFSET " + s.Offset.String())
		}
	}
	return b.String()
}

func (*Begin) String() string    { return "BEGIN" }
func (*Commit) String() string   { return "COMMIT" }
func (*Rollback) String() string { return "ROLLBACK" }
