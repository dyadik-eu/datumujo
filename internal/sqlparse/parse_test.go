package sqlparse

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// strip sets every position in the tree to zero, so two trees of the
// same statement in different text compare equal.
func strip(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			strip(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(At{}) {
			v.Set(reflect.Zero(v.Type()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			strip(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			strip(v.Index(i))
		}
	}
}

func stripped(all []Statement) []Statement {
	for _, s := range all {
		strip(reflect.ValueOf(s))
	}
	return all
}

func print(all []Statement) string {
	parts := make([]string, len(all))
	for i, s := range all {
		parts[i] = s.String()
	}
	return strings.Join(parts, ";\n")
}

// roundTrip parses the printed form of src. It must give the same tree,
// and the same text when printed again.
func roundTrip(t *testing.T, src string) {
	t.Helper()
	all, err := ParseAll(src)
	if err != nil {
		return
	}
	text := print(all)
	again, err := ParseAll(text)
	if err != nil {
		t.Fatalf("%q printed as %q, which does not parse: %v", src, text, err)
	}
	if !reflect.DeepEqual(stripped(all), stripped(again)) {
		t.Fatalf("%q printed as %q, which parses to another tree", src, text)
	}
	if text2 := print(again); text2 != text {
		t.Fatalf("%q printed as %q, then as %q", src, text, text2)
	}
}

func expr(t *testing.T, src string) string {
	t.Helper()
	s, err := Parse("SELECT " + src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return s.(*Select).Items[0].Expr.String()
}

func TestPrecedence(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"1 * 2 + 3", "((1 * 2) + 3)"},
		{"1 - 2 - 3", "((1 - 2) - 3)"},
		{"a || b * c", "((a || b) * c)"},
		{"-a || b", "((- a) || b)"},
		{"-5 * 2", "(-5 * 2)"},
		{"-(5)", "(- (5))"},
		{"-(1.5)", "(- (1.5))"},
		{"- -5", "(- (-5))"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"1 < 2 = 3 < 4", "((1 < 2) = (3 < 4))"},
		{"a = b AND c = d OR e", "(((a = b) AND (c = d)) OR e)"},
		{"a OR b AND c", "(a OR (b AND c))"},
		{"NOT a = b", "(NOT (a = b))"},
		{"NOT NOT a", "(NOT (NOT a))"},
		{"a == b", "(a = b)"},
		{"a <> b", "(a != b)"},
		{"a IS NOT NULL", "(a IS NOT NULL)"},
		{"a IS NULL = b", "((a IS NULL) = b)"},
		{"a NOT LIKE 'x%'", "(a NOT LIKE 'x%')"},
		{"a BETWEEN 1 AND 2 + 3", "(a BETWEEN 1 AND (2 + 3))"},
		{"a NOT BETWEEN b AND c AND d", "((a NOT BETWEEN b AND c) AND d)"},
		{"a IN (1, 2 + 3)", "(a IN (1, (2 + 3)))"},
		{"a NOT IN (1)", "(a NOT IN (1))"},
		{"a IN (1) + 2 * 3 < 4", "(((a IN (1)) + (2 * 3)) < 4)"},
		{"a IN (1) * 2 + 3 = 4", "((((a IN (1)) * 2) + 3) = 4)"},
		{"t.a + \"B c\"", "(t.a + \"B c\")"},
		{"count(*)", "count(*)"},
		{"COUNT(DISTINCT a)", "count(DISTINCT a)"},
		{"lower(a, b)", "lower(a, b)"},
		{"now()", "now()"},
		{"CAST(a AS varchar)", "CAST(a AS TEXT)"},
		{"CAST(a AS double precision)", "CAST(a AS REAL)"},
		{"CASE WHEN a THEN 1 ELSE 2 END", "CASE WHEN a THEN 1 ELSE 2 END"},
		{"CASE a WHEN 1 THEN 'x' WHEN 2 THEN 'y' END", "CASE a WHEN 1 THEN 'x' WHEN 2 THEN 'y' END"},
		{"'it''s'", "'it''s'"},
		{"x'00FF'", "X'00ff'"},
		{"X''", "X''"},
		{"1.5e3", "1500.0"},
		{".5", "0.5"},
		{"1e300", "1e+300"},
		{"-0.0", "-0.0"},
		{"TRUE AND false", "(TRUE AND FALSE)"},
		{"null", "NULL"},
		{"key + \"index\"", "(\"key\" + \"index\")"},
		{"\"Mixed\"", "\"Mixed\""},
		{"\"1a\" + a1", "(\"1a\" + a1)"},
		{"\"select\"", "\"select\""},
	} {
		if got := expr(t, c.src); got != c.want {
			t.Errorf("%s: %s, want %s", c.src, got, c.want)
		}
		roundTrip(t, "SELECT "+c.src)
	}
}

func TestParams(t *testing.T) {
	all, err := ParseAll("SELECT ?, ?, ?5, ?; SELECT ?2, ?")
	if err != nil {
		t.Fatal(err)
	}
	if got := print(all); got != "SELECT ?1, ?2, ?5, ?6;\nSELECT ?2, ?3" {
		t.Errorf("params: %s", got)
	}
}

// statements are the forms of L-1 and L-2, with the printed form that
// each must have.
var statements = []struct{ src, want string }{
	{"create table issue (repo integer not null, number bigint, state text, body varchar, primary key (repo, number))",
		"CREATE TABLE issue (repo INTEGER NOT NULL, number INTEGER, state TEXT, body TEXT, PRIMARY KEY (repo, number))"},
	{"CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, x REAL NULL, b BOOL, d BLOB, at TIMESTAMP, f DOUBLE PRECISION)",
		"CREATE TABLE IF NOT EXISTS t (id INTEGER, x REAL, b BOOLEAN, d BLOB, at TIMESTAMP, f REAL, PRIMARY KEY (id))"},
	{"CREATE TABLE key (index INTEGER, text TEXT)", `CREATE TABLE "key" ("index" INTEGER, text TEXT)`},
	{"create table d (default integer default -5 not null, current_timestamp timestamp null default current_timestamp, s text default 'it''s', b blob default x'00ff', r real default +1e3, n int default null, t bool default true)",
		`CREATE TABLE d ("default" INTEGER NOT NULL DEFAULT -5, "current_timestamp" TIMESTAMP DEFAULT CURRENT_TIMESTAMP, s TEXT DEFAULT 'it''s', b BLOB DEFAULT X'00ff', r REAL DEFAULT 1000.0, n INTEGER DEFAULT NULL, t BOOLEAN DEFAULT TRUE)`},
	{"ALTER TABLE t ADD c REAL NOT NULL DEFAULT -0.0", "ALTER TABLE t ADD COLUMN c REAL NOT NULL DEFAULT -0.0"},
	{"create table c (check integer check (check > 0) check(check < 9), b text, check (length(b) < check), primary key (check))",
		`CREATE TABLE c ("check" INTEGER CHECK (("check" > 0)) CHECK (("check" < 9)), b TEXT, PRIMARY KEY ("check"), CHECK ((length(b) < "check")))`},
	{"ALTER TABLE t ADD COLUMN n INTEGER DEFAULT 1 CHECK (n BETWEEN 0 AND 5)", "ALTER TABLE t ADD COLUMN n INTEGER DEFAULT 1 CHECK ((n BETWEEN 0 AND 5))"},
	{"create table u (id integer primary key, a text unique not null, b int, c int, unique (b, c), unique(a), check (b > 0))",
		"CREATE TABLE u (id INTEGER, a TEXT NOT NULL UNIQUE, b INTEGER, c INTEGER, PRIMARY KEY (id), UNIQUE (b, c), UNIQUE (a), CHECK ((b > 0)))"},
	{"ALTER TABLE t ADD COLUMN u TEXT UNIQUE", "ALTER TABLE t ADD COLUMN u TEXT UNIQUE"},
	{"CREATE UNIQUE INDEX by_name ON account (name)", "CREATE UNIQUE INDEX by_name ON account (name)"},
	{"create index if not exists s on issue(repo, state)", "CREATE INDEX IF NOT EXISTS s ON issue (repo, state)"},
	{"DROP TABLE issue", "DROP TABLE issue"},
	{"drop table if exists issue", "DROP TABLE IF EXISTS issue"},
	{"DROP INDEX IF EXISTS s", "DROP INDEX IF EXISTS s"},
	{"ALTER TABLE issue ADD label TEXT", "ALTER TABLE issue ADD COLUMN label TEXT"},
	{"INSERT INTO t VALUES (1, 'a'), (2, NULL)", "INSERT INTO t VALUES (1, 'a'), (2, NULL)"},
	{"INSERT INTO t (a, b) VALUES (?, ?)", "INSERT INTO t (a, b) VALUES (?1, ?2)"},
	{"UPDATE t SET a = a + 1, b = 'x' WHERE id = ?", "UPDATE t SET a = (a + 1), b = 'x' WHERE (id = ?1)"},
	{"DELETE FROM t", "DELETE FROM t"},
	{"DELETE FROM t WHERE a IS NULL", "DELETE FROM t WHERE (a IS NULL)"},
	{"SELECT * FROM t", "SELECT * FROM t"},
	{"select distinct a as x, b y, t.* from t as u where a > 1 order by a desc, b asc limit 10 offset 5",
		"SELECT DISTINCT a AS x, b AS y, t.* FROM t AS u WHERE (a > 1) ORDER BY a DESC, b LIMIT 10 OFFSET 5"},
	{"SELECT state, count(*) FROM issue GROUP BY state HAVING count(*) > 2",
		"SELECT state, count(*) FROM issue GROUP BY state HAVING (count(*) > 2)"},
	{"SELECT i.number, c.body FROM issue i JOIN comment c ON c.issue = i.number LEFT OUTER JOIN label l ON l.issue = i.number INNER JOIN x ON TRUE",
		"SELECT i.number, c.body FROM issue AS i JOIN comment AS c ON (c.issue = i.number) LEFT JOIN label AS l ON (l.issue = i.number) JOIN x ON TRUE"},
	{"SELECT 1 + 1", "SELECT (1 + 1)"},
	{"begin", "BEGIN"},
	{"BEGIN TRANSACTION", "BEGIN"},
	{"commit", "COMMIT"},
	{"ROLLBACK", "ROLLBACK"},
	{"SELECT a -- a comment\nFROM /* another */ t", "SELECT a FROM t"},
}

func TestStatements(t *testing.T) {
	for _, c := range statements {
		s, err := Parse(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got := s.String(); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.src, got, c.want)
		}
		roundTrip(t, c.src)
	}
	all, err := ParseAll(";; SELECT 1; SELECT 2;")
	if err != nil || len(all) != 2 {
		t.Errorf("two statements with empty ones around: %d, %v", len(all), err)
	}
}

// TestErrors checks the message and the position of errors (L-6).
func TestErrors(t *testing.T) {
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"SELEC 1", 1, 1, "selec, want a statement"},
		{"SELECT", 1, 7, "the end, want an expression"},
		{"SELECT 1 +", 1, 11, "want an expression"},
		{"SELECT a FROM", 1, 14, "want a table name"},
		{"SELECT a FROM select", 1, 15, "SELECT is a keyword"},
		{"SELECT 'abc", 1, 8, "' without its closing '"},
		{"SELECT \"", 1, 8, "\" without its closing \""},
		{"SELECT \"\"", 1, 8, "empty name"},
		{"SELECT \"a\x00b\"", 1, 8, "cannot hold the character NUL"},
		{"SELECT \"\x00derived\".x", 1, 8, "cannot hold the character NUL"},
		{"SELECT 1 /* x", 1, 10, "comment without */"},
		{"SELECT X'0'", 1, 8, "even number of hex digits"},
		{"SELECT 12abc", 1, 8, "runs into"},
		{"SELECT 1.2.3", 1, 8, "runs into"},
		{"SELECT 1e", 1, 8, "no digits in the exponent"},
		{"SELECT 1e999", 1, 8, "out of range"},
		{"SELECT 9223372036854775808", 1, 8, "out of the range of INTEGER"},
		{"SELECT -(9223372036854775808)", 1, 10, "out of the range of INTEGER"},
		{"SELECT 99999999999999999999", 1, 8, "out of the range of INTEGER"},
		{"SELECT ?0", 1, 8, "from 1 to 32766"},
		{"SELECT ?32767", 1, 8, "from 1 to 32766"},
		{"SELECT a\n  FROM t\n  WHERE a = = 1", 3, 13, "=, want an expression"},
		{"SELECT a;;b", 1, 11, "b, want a statement"},
		{"SELECT 1 2", 1, 10, "2, want ; or the end"},
		{"SELECT # ", 1, 8, "unexpected character '#'"},
		{"SELECT 'ü' ä", 1, 12, "unexpected character 'ä'"},
		{"SELECT \xff", 1, 8, "not UTF-8"},
		{"CREATE TABLE t ()", 1, 17, "want a column name"},
		{"CREATE TABLE t (a)", 1, 18, "want a type"},
		{"CREATE TABLE t (a STRING)", 1, 19, "unknown type string"},
		{"CREATE TABLE t (a VARCHAR(20))", 1, 26, "takes no length"},
		{"CREATE TABLE t (a INTEGER PRIMARY KEY, b INTEGER PRIMARY KEY)", 1, 50, "a second primary key; the first is at line 1, column 27"},
		{"CREATE TABLE t (a INTEGER, PRIMARY KEY (a), PRIMARY KEY (a))", 1, 45, "a second primary key"},
		{"CREATE TABLE t (a INTEGER PRIMARY KEY PRIMARY KEY)", 1, 39, "PRIMARY KEY twice"},
		{"CREATE TABLE if (a INTEGER)", 1, 17, "want NOT"},
		{"ALTER TABLE t ADD COLUMN a INTEGER PRIMARY KEY", 1, 36, "cannot be part of the primary key"},
		{"CREATE TABLE t (a INTEGER DEFAULT 1 DEFAULT 2)", 1, 37, "column a: DEFAULT twice"},
		{"CREATE TABLE t (a INTEGER CHECK a > 0)", 1, 33, "a, want ("},
		{"CREATE TABLE t (a INTEGER UNIQUE UNIQUE)", 1, 34, "column a: UNIQUE twice"},
		{"CREATE TABLE t (a INTEGER, UNIQUE a)", 1, 35, "a, want ("},
		{"CREATE TABLE t (a INTEGER, UNIQUE ())", 1, 36, "want a column name"},
		{"CREATE TABLE t (a INTEGER CHECK (a >))", 1, 37, "want an expression"},
		{"CREATE TABLE t (a INTEGER, CHECK (a > 0)", 1, 41, "want )"},
		{"CREATE TABLE t (a INTEGER, CHECK)", 1, 33, "want a type"},
		{"CREATE TABLE t (a INTEGER DEFAULT (1))", 1, 35, "(, want a constant or CURRENT_TIMESTAMP after DEFAULT"},
		{"CREATE TABLE t (a INTEGER DEFAULT b)", 1, 35, "want a constant or CURRENT_TIMESTAMP"},
		{"CREATE TABLE t (a INTEGER DEFAULT ?)", 1, 35, "want a constant or CURRENT_TIMESTAMP"},
		{"CREATE TABLE t (a INTEGER DEFAULT - 'x')", 1, 37, "want a number after the sign of a DEFAULT"},
		{"CREATE TABLE t (a INTEGER DEFAULT -9223372036854775809)", 1, 36, "out of the range of INTEGER"},
		{"CREATE TABLE t (a INTEGER DEFAULT)", 1, 34, "), want a constant"},
		{"INSERT INTO t VALUES ()", 1, 23, "want an expression"},
		{"UPDATE t SET a", 1, 15, "want ="},
		{"SELECT a NOT 1", 1, 14, "want IN, BETWEEN or LIKE after NOT"},
		{"SELECT a BETWEEN 1 OR 2", 1, 20, "want AND"},
		{"SELECT CASE END", 1, 13, "want WHEN"},
		{"SELECT CAST(a TEXT)", 1, 15, "want AS"},
		{"SELECT a FROM t LEFT x", 1, 22, "want JOIN"},
		{"SELECT a FROM t JOIN u", 1, 23, "want ON"},
		{"", 1, 1, "0 statements, want exactly one"},
	} {
		_, err := Parse(c.src)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q: %v, want an *Error", c.src, err)
			continue
		}
		if e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Msg, c.msg) {
			t.Errorf("%q: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

func TestPositions(t *testing.T) {
	s, err := Parse("SELECT\n  a,\n\tb + ?\nFROM t")
	if err != nil {
		t.Fatal(err)
	}
	sel := s.(*Select)
	bin := sel.Items[1].Expr.(*Binary)
	for _, c := range []struct {
		n         Node
		line, col int
	}{
		{sel, 1, 1},
		{sel.Items[0].Expr, 2, 3},
		{bin.L, 3, 2},
		{bin, 3, 4},
		{bin.R, 3, 6},
		{sel.From, 4, 6},
	} {
		if at := c.n.Pos(); at.Line != c.line || at.Col != c.col {
			t.Errorf("%s at %v, want %d:%d", c.n, at, c.line, c.col)
		}
	}
}

// TestDepth: deep nesting is an error, and a nesting at the bound
// parses.
func TestDepth(t *testing.T) {
	for _, c := range []struct {
		open, close string
	}{
		{"(", ")"}, {"NOT ", ""}, {"- ", ""}, {"abs(", ")"}, {"CAST(", " AS INTEGER)"},
	} {
		deep := "SELECT " + strings.Repeat(c.open, 5000) + "1" + strings.Repeat(c.close, 5000)
		if _, err := Parse(deep); err == nil || !strings.Contains(err.Error(), "nested more than") {
			t.Errorf("%s nested 5000 times: %v", c.open, err)
		}
		ok := "SELECT " + strings.Repeat(c.open, MaxDepth/2) + "1" + strings.Repeat(c.close, MaxDepth/2)
		if _, err := Parse(ok); err != nil {
			t.Errorf("%s nested %d times: %v", c.open, MaxDepth/2, err)
		}
	}
}

// TestParseDefault reads back what Default.String writes, as the schema
// keeps it, and refuses text that is more than one default.
func TestParseDefault(t *testing.T) {
	for _, d := range []*Default{
		{Value: int64(math.MinInt64)}, {Value: int64(7)}, {Value: math.Copysign(0, -1)},
		{Value: 1e300}, {Value: 0.1}, {Value: "it's"}, {Value: []byte{}}, {Value: []byte{0, 255}},
		{Value: true}, {Value: false}, {}, {Now: true},
	} {
		text := d.String()
		got, err := ParseDefault(text)
		if err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		strip(reflect.ValueOf(got))
		if !reflect.DeepEqual(got, d) || math.Signbit(toFloat(got.Value)) != math.Signbit(toFloat(d.Value)) {
			t.Errorf("%s reads back as %#v, want %#v", text, got, d)
		}
	}
	for _, text := range []string{"", "1 2", "(1)", "1 + 1", "CURRENT_TIMESTAMP()", "x"} {
		if d, err := ParseDefault(text); err == nil {
			t.Errorf("%q reads as %v", text, d)
		}
	}
}

// TestParseExpr reads back what Expr.String writes, as the schema keeps
// a CHECK, and refuses text that is more than one expression.
func TestParseExpr(t *testing.T) {
	for _, src := range []string{
		"a > 0", "length(b) < 3 AND c IS NOT NULL", "x BETWEEN -1 AND 2.5",
		"CASE WHEN a THEN 'x' ELSE NULL END = 'x'", `"check" IN (1, 2)`, "NOT (a OR b)",
	} {
		e, err := ParseExpr(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		again, err := ParseExpr(e.String())
		if err != nil {
			t.Errorf("%s printed as %s, which does not parse: %v", src, e, err)
			continue
		}
		strip(reflect.ValueOf(e))
		strip(reflect.ValueOf(again))
		if !reflect.DeepEqual(e, again) {
			t.Errorf("%s printed as %s, which parses to another tree", src, e)
		}
	}
	for _, src := range []string{"", "a >", "a > 0 b", "a > 0;", "(a"} {
		if e, err := ParseExpr(src); err == nil {
			t.Errorf("%q reads as %v", src, e)
		}
	}
}

func toFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

func FuzzParse(f *testing.F) {
	for _, c := range statements {
		f.Add(c.src)
	}
	for _, s := range []string{
		"SELECT -9223372036854775808 - -1", "SELECT (- (5)), - -5.5, +1",
		"SELECT a NOT BETWEEN -1 AND 2 IS NOT NULL", "SELECT 'a' || X'ff' || \"q\"\"x\"",
		"SELECT CASE WHEN NOT a IN (1, ?3) THEN -0.0 END",
		"CREATE TABLE t (a INTEGER DEFAULT -9223372036854775808, b TIMESTAMP DEFAULT current_timestamp, c BLOB DEFAULT X'')",
		"CREATE TABLE t (check INTEGER CHECK (check > 0), CHECK (check < 9))",
		"CREATE TABLE t (a INTEGER UNIQUE, b TEXT, UNIQUE (a, b), UNIQUE (b))",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		roundTrip(t, src)
	})
}
