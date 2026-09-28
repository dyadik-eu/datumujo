package sqlexec

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

func compileExpr(t testing.TB, src string, r Resolver) (*Expr, error) {
	t.Helper()
	s, err := sqlparse.Parse("SELECT " + src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return Compile(s.(*sqlparse.Select).Items[0].Expr, r)
}

func eval(t testing.TB, src string, params ...any) (any, error) {
	t.Helper()
	x, err := compileExpr(t, src, nil)
	if err != nil {
		return nil, err
	}
	return x.Eval(nil, params)
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	}
	return a == b
}

func TestValues(t *testing.T) {
	for _, c := range []struct {
		src    string
		want   any
		params []any
	}{
		// Arithmetic.
		{"7 / 2", int64(3), nil},
		{"-7 / 2", int64(-3), nil},
		{"7 % 3", int64(1), nil},
		{"-7 % 3", int64(-1), nil},
		{"7 % -3", int64(1), nil},
		{"7 / 2.0", 3.5, nil},
		{"1 + 0.5", 1.5, nil},
		{"9223372036854775807 + 0", int64(math.MaxInt64), nil},
		{"-9223372036854775808 % -1", int64(0), nil},
		{"- -9223372036854775807", int64(math.MaxInt64), nil},
		{"+2.5", 2.5, nil},
		{"-(0.0)", 0.0, nil},
		{"-0.0", math.Copysign(0, -1), nil},
		{"1 + NULL", nil, nil},
		{"'a' || 'b'", "ab", nil},
		{"'a' || NULL", nil, nil},
		// Comparisons, exact across INTEGER and REAL.
		{"9007199254740993 = 9007199254740992.0", false, nil},
		{"9007199254740993 > 9007199254740992.0", true, nil},
		{"1 = 1.0", true, nil},
		{"'a' < 'b'", true, nil},
		{"'B' < 'a'", true, nil},
		{"X'00' < X'01'", true, nil},
		{"FALSE < TRUE", true, nil},
		{"1 = NULL", nil, nil},
		{"NULL = NULL", nil, nil},
		{"NULL IS NULL", true, nil},
		{"1 IS NULL", false, nil},
		{"1 IS NOT 2", true, nil},
		{"1.0 IS 1", true, nil},
		// Three-valued logic.
		{"TRUE AND NULL", nil, nil},
		{"FALSE AND NULL", false, nil},
		{"NULL AND FALSE", false, nil},
		{"TRUE OR NULL", true, nil},
		{"NULL OR TRUE", true, nil},
		{"FALSE OR NULL", nil, nil},
		{"NOT NULL", nil, nil},
		{"NOT FALSE", true, nil},
		// The right side does not run when the left decides.
		{"FALSE AND 1 / 0 = 1", false, nil},
		{"TRUE OR 1 / 0 = 1", true, nil},
		// IN and BETWEEN.
		{"1 IN (2, 1)", true, nil},
		{"1 IN (2, NULL)", nil, nil},
		{"1 IN (1, NULL)", true, nil},
		{"1 NOT IN (2, NULL)", nil, nil},
		{"1 NOT IN (2, 3)", true, nil},
		{"NULL IN (1)", nil, nil},
		{"2 BETWEEN 1 AND 3", true, nil},
		{"3 BETWEEN 1 AND NULL", nil, nil},
		{"5 BETWEEN 1 AND NULL", nil, nil},
		{"5 BETWEEN 6 AND NULL", false, nil},
		{"5 NOT BETWEEN 6 AND NULL", true, nil},
		{"2 NOT BETWEEN 1 AND 3", false, nil},
		// LIKE: A to Z ignore case, other letters do not.
		{"'abc' LIKE 'A%'", true, nil},
		{"'ÄB' LIKE 'äb'", false, nil},
		{"'a_c' LIKE 'abc'", false, nil},
		{"'abc' LIKE 'a_c'", true, nil},
		{"'äbc' LIKE '_b%'", true, nil},
		{"'' LIKE '%'", true, nil},
		{"'abc' LIKE '%%b%'", true, nil},
		{"'abc' NOT LIKE 'b%'", true, nil},
		{"'x' LIKE NULL", nil, nil},
		// CASE.
		{"CASE WHEN FALSE THEN 1 WHEN TRUE THEN 2 END", int64(2), nil},
		{"CASE WHEN NULL THEN 1 ELSE 3 END", int64(3), nil},
		{"CASE WHEN FALSE THEN 1 END", nil, nil},
		{"CASE 2 WHEN 1 THEN 'a' WHEN 2 THEN 'b' END", "b", nil},
		{"CASE NULL WHEN NULL THEN 'a' ELSE 'n' END", "n", nil},
		// Functions.
		{"abs(-3)", int64(3), nil},
		{"abs(-2.5)", 2.5, nil},
		{"length('äbc')", int64(3), nil},
		{"length(X'00ff')", int64(2), nil},
		{"lower('ÄBC')", "Äbc", nil},
		{"upper('äbc')", "äBC", nil},
		{"trim('  a  ')", "a", nil},
		{"ltrim('xxa', 'x')", "a", nil},
		{"rtrim('a  ')", "a", nil},
		{"trim('xxaxx', 'x')", "a", nil},
		{"replace('abab', 'b', 'cd')", "acdacd", nil},
		{"replace('abc', '', 'x')", "abc", nil},
		{"instr('hello', 'l')", int64(3), nil},
		{"instr('äl', 'l')", int64(2), nil},
		{"instr('a', '')", int64(1), nil},
		{"instr(X'0102', X'02')", int64(2), nil},
		{"substr('hello', 2)", "ello", nil},
		{"substr('hello', -3, 2)", "ll", nil},
		{"substr('hello', 0, 2)", "h", nil},
		{"substr('hello', 2, -1)", "h", nil},
		{"substr('hello', 3, -5)", "he", nil},
		{"substr('hello', -10, 3)", "", nil},
		{"substr('hello', 6)", "", nil},
		{"substr('hello', 9223372036854775807, -9223372036854775808)", "hello", nil},
		{"substr(X'010203', 2, 1)", []byte{2}, nil},
		{"coalesce(NULL, 2, 3)", int64(2), nil},
		{"coalesce(NULL, NULL)", nil, nil},
		{"ifnull(NULL, 'x')", "x", nil},
		{"nullif(1, 1)", nil, nil},
		{"nullif(1, 2)", int64(1), nil},
		{"min(3, 1, 2)", int64(1), nil},
		{"max('a', 'b')", "b", nil},
		{"min(1, NULL)", nil, nil},
		{"max(1, 2.5)", 2.5, nil},
		{"min(1, 1.0)", 1.0, nil},
		{"min(1.0, 1)", int64(1), nil},
		{"max(1.0, 1)", 1.0, nil},
		{"max(1, 1.0)", int64(1), nil},
		{"round(2.5)", 3.0, nil},
		{"round(-2.5)", -3.0, nil},
		{"round(2.675, 2)", 2.67, nil},
		{"round(0.125, 2)", 0.13, nil},
		{"round(1.005, 2)", 1.0, nil},
		{"round(123.456, -1)", 123.0, nil},
		{"round(5)", 5.0, nil},
		{"round(123456789.123456789, 5)", 123456789.12346, nil},
		{"round(NULL)", nil, nil},
		{"round(-0.4)", 0.0, nil},
		{"round(-0.001, 2)", math.Copysign(0, -1), nil},
		{"abs(-0.0)", math.Copysign(0, -1), nil},
		{"replace('ab', '', NULL)", "ab", nil},
		{"replace('ab', 'a', NULL)", nil, nil},
		{"substr(X'', 1, 1)", []byte{}, nil},
		// CAST.
		{"CAST(3.9 AS INTEGER)", int64(3), nil},
		{"CAST(-3.9 AS INTEGER)", int64(-3), nil},
		{"CAST(' 12 ' AS INTEGER)", int64(12), nil},
		{"CAST('-5' AS INTEGER)", int64(-5), nil},
		{"CAST('1.5e3' AS REAL)", 1500.0, nil},
		{"CAST(1 AS REAL)", 1.0, nil},
		{"CAST(0.1 AS TEXT)", "0.1", nil},
		{"CAST(1.0 / 3 AS TEXT)", "0.3333333333333333", nil},
		{"CAST(1e20 AS TEXT)", "1.0e+20", nil},
		{"CAST(1e-7 AS TEXT)", "1.0e-07", nil},
		{"CAST(-0.0 AS TEXT)", "0.0", nil},
		{"CAST(123456789012345.0 AS TEXT)", "123456789012345.0", nil},
		{"CAST(1234567890123456.0 AS TEXT)", "1.234567890123456e+15", nil},
		{"CAST(TRUE AS TEXT)", "true", nil},
		{"CAST(TRUE AS INTEGER)", int64(1), nil},
		{"CAST(0 AS BOOLEAN)", false, nil},
		{"CAST('False' AS BOOLEAN)", false, nil},
		{"CAST('ab' AS BLOB)", []byte("ab"), nil},
		{"CAST(X'6162' AS TEXT)", "ab", nil},
		{"CAST('2026-09-28T10:00:00+02:00' AS TIMESTAMP)", time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC), nil},
		{"CAST(CAST('2026-09-28T08:00:00.5Z' AS TIMESTAMP) AS TEXT)", "2026-09-28T08:00:00.5Z", nil},
		{"CAST(NULL AS INTEGER)", nil, nil},
		// Parameters.
		{"?1 + ?2", int64(3), []any{int64(1), int64(2)}},
		{"?1 + ?2", 3.5, []any{int64(1), 2.5}},
		{"?1 || 'x'", "ax", []any{"a"}},
		{"?1 IS NULL", true, []any{nil}},
		{"length(?1)", int64(4), []any{"a\x00bc"}},
		{"?1", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), []any{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}},
	} {
		got, err := eval(t, c.src, c.params...)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if !sameValue(got, c.want) {
			t.Errorf("%s: %#v, want %#v", c.src, got, c.want)
		}
	}
}

// TestErrors: a type that does not fit is an error before the run. A
// value that breaks a rule is an error at the run. Both name the
// position (L-6).
func TestErrors(t *testing.T) {
	for _, c := range []struct {
		src       string
		params    []any
		line, col int
		msg       string
	}{
		// Before the run.
		{"'1' = 1", nil, 1, 14, "they do not compare"},
		{"1 + 'a'", nil, 1, 12, "+ needs INTEGER or REAL, and 'a' is TEXT"},
		{"1 || 'a'", nil, 1, 8, "|| needs TEXT, and 1 is INTEGER"},
		{"1.5 % 2", nil, 1, 8, "% needs INTEGER"},
		{"NOT 1", nil, 1, 12, "NOT needs BOOLEAN"},
		{"1 AND TRUE", nil, 1, 8, "AND needs BOOLEAN"},
		{"1 LIKE 'a'", nil, 1, 8, "LIKE needs TEXT"},
		{"-'a'", nil, 1, 9, "unary - needs INTEGER or REAL"},
		{"CASE WHEN TRUE THEN 1 ELSE 'a' END", nil, 1, 35, "CASE: 1 is INTEGER and 'a' is TEXT; they must have one type"},
		{"CASE WHEN TRUE THEN 1 ELSE 2.5 END", nil, 1, 35, "they must have one type"},
		{"CASE WHEN 1 THEN 1 END", nil, 1, 18, "WHEN needs BOOLEAN"},
		{"coalesce(1, 'a')", nil, 1, 20, "they must have one type"},
		{"1 IN (1, 'a')", nil, 1, 17, "they do not compare"},
		{"length(1)", nil, 1, 15, "needs TEXT or BLOB"},
		{"substr('a', 1.5)", nil, 1, 20, "substr needs INTEGER"},
		{"instr('a', X'00')", nil, 1, 19, "they must have one type"},
		{"CAST(X'00' AS INTEGER)", nil, 1, 8, "no CAST from BLOB to INTEGER"},
		{"CAST(1.5 AS BOOLEAN)", nil, 1, 8, "no CAST from REAL to BOOLEAN"},
		{"nosuch(1)", nil, 1, 8, "no such function: nosuch"},
		{"abs(1, 2)", nil, 1, 8, "abs takes 1 arguments, not 2"},
		{"coalesce(1)", nil, 1, 8, "takes at least 2 arguments"},
		{"substr('a')", nil, 1, 8, "takes 2 or 3 arguments"},
		{"count(*)", nil, 1, 8, "aggregate function count is not allowed here"},
		{"max(1)", nil, 1, 8, "aggregate function max"},
		{"a + 1", nil, 1, 8, "no such column: a"},
		// At the run.
		{"1 / 0", nil, 1, 10, "division by zero"},
		{"1 % 0", nil, 1, 10, "division by zero"},
		{"1.5 / 0", nil, 1, 12, "division by zero"},
		{"9223372036854775807 + 1", nil, 1, 28, "integer overflow"},
		{"-9223372036854775808 - 1", nil, 1, 29, "integer overflow"},
		{"4611686018427387904 * 2", nil, 1, 28, "integer overflow"},
		{"-9223372036854775808 * -1", nil, 1, 29, "integer overflow"},
		{"-9223372036854775808 / -1", nil, 1, 29, "integer overflow"},
		{"-(-9223372036854775808)", nil, 1, 8, "integer overflow"},
		{"abs(-9223372036854775808)", nil, 1, 8, "integer overflow"},
		{"1e308 * 10", nil, 1, 14, "REAL overflow"},
		{"CAST(1e30 AS INTEGER)", nil, 1, 8, "does not convert"},
		{"CAST('12abc' AS INTEGER)", nil, 1, 8, "CAST of '12abc' to INTEGER"},
		{"CAST('' AS INTEGER)", nil, 1, 8, "does not convert"},
		{"CAST('inf' AS REAL)", nil, 1, 8, "does not convert"},
		{"CAST('0x10' AS REAL)", nil, 1, 8, "does not convert"},
		{"CAST(2 AS BOOLEAN)", nil, 1, 8, "does not convert"},
		{"CAST(X'ff' AS TEXT)", nil, 1, 8, "does not convert"},
		{"CAST('yesterday' AS TIMESTAMP)", nil, 1, 8, "does not convert"},
		{"?1 + 1", []any{"a"}, 1, 8, "+ needs INTEGER or REAL, and ?1 is TEXT"},
		{"?1 = 1", []any{"a"}, 1, 13, "they do not compare"},
		{"?1 AND TRUE", []any{int64(1)}, 1, 8, "AND needs BOOLEAN, and ?1 is INTEGER"},
		{"?2", []any{int64(1)}, 1, 8, "parameter ?2 has no value; 1 given"},
		{"?1", []any{1}, 1, 8, "parameter ?1 is a Go int"},
		{"?1", []any{"\xff"}, 1, 8, "not UTF-8"},
		{"CASE WHEN TRUE THEN ?1 ELSE 1 END", []any{"a"}, 1, 28, "CASE: ?1 is TEXT, want INTEGER"},
		{"coalesce(?1, 1)", []any{"a"}, 1, 17, "coalesce: ?1 is TEXT, want INTEGER"},
		{"length(?1)", []any{int64(1)}, 1, 15, "length needs TEXT or BLOB, and ?1 is INTEGER"},
		{"CAST(?1 AS INTEGER)", []any{[]byte{1}}, 1, 8, "no CAST from BLOB to INTEGER"},
	} {
		_, err := eval(t, c.src, c.params...)
		var e *sqlparse.Error
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an error at a position", c.src, err)
			continue
		}
		if e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Msg, c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// columns is a Resolver over named columns of given types.
type columns []table.Column

func (cs columns) Column(tbl, name string) (int, table.Type, error) {
	for i, c := range cs {
		if c.Name == name && tbl == "" {
			return i, c.Type, nil
		}
	}
	return 0, 0, errors.New("no such column: " + name)
}

func TestColumns(t *testing.T) {
	cs := columns{{Name: "n", Type: table.Int64, Null: true}, {Name: "s", Type: table.String}}
	x, err := compileExpr(t, "n * 2 || s", cs)
	if err == nil {
		t.Fatalf("INTEGER || TEXT compiled: %v", x)
	}
	x, err = compileExpr(t, "CAST(n * 2 AS TEXT) || s", cs)
	if err != nil {
		t.Fatal(err)
	}
	if typ, ok := x.Type(); !ok || typ != table.String {
		t.Errorf("type %v %v", typ, ok)
	}
	for _, c := range []struct {
		row  []any
		want any
	}{
		{[]any{int64(21), "!"}, "42!"},
		{[]any{nil, "!"}, nil},
	} {
		if got, err := x.Eval(c.row, nil); err != nil || !sameValue(got, c.want) {
			t.Errorf("%v: %#v %v", c.row, got, err)
		}
	}
}

// FuzzEval compiles and runs any expression. Nothing panics. If the type
// of an expression is known before the run, each value has that type.
// So the checks before the run hold at the run (L-3).
func FuzzEval(f *testing.F) {
	for _, s := range []string{
		"1 + 2 * 3", "CASE WHEN ?1 THEN ?2 ELSE 1 END", "coalesce(?1, ?2, 'x')",
		"substr(?1, ?2, ?3)", "round(?1, ?2)", "min(?1, 2, ?2)", "CAST(?1 AS TEXT)",
		"?1 BETWEEN ?2 AND 3", "?1 IN (1, ?2)", "nullif(?1, 2)", "abs(?1) % 3",
		"?1 LIKE ?2", "instr(?1, ?2)", "-?1 / ?2",
		// A parameter of the wrong type in a branch whose type is known.
		"CASE WHEN ?4 THEN ?3 ELSE 1 END", "coalesce(?3, 1)",
	} {
		f.Add(s, int64(3), 2.5, "a%", true)
	}
	f.Fuzz(func(t *testing.T, src string, i int64, fl float64, s string, b bool) {
		st, err := sqlparse.Parse("SELECT " + src)
		if err != nil {
			return
		}
		sel, ok := st.(*sqlparse.Select)
		if !ok || len(sel.Items) == 0 || sel.Items[0].Expr == nil {
			return
		}
		x, err := Compile(sel.Items[0].Expr, nil)
		if err != nil {
			return
		}
		want, isKnown := x.Type()
		for _, params := range [][]any{
			{i, fl, s, b}, {s, i, fl, b}, {fl, s, b, i}, {b, i, s, fl}, {nil, nil, nil, nil},
			{i, i, i, i}, {s, s, s, s}, {[]byte(s), []byte(s), i, i},
		} {
			v, err := x.Eval(nil, params)
			if err != nil {
				var e *sqlparse.Error
				if !errors.As(err, &e) {
					t.Fatalf("%s %v: %v is not an error at a position", src, params, err)
				}
				continue
			}
			if v == nil || !isKnown {
				continue
			}
			if got, _ := typeOf(v); got != want {
				t.Fatalf("%s %v: value %#v of type %v, the type before the run was %v", src, params, v, got, want)
			}
		}
	})
}
