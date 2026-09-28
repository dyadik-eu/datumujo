// Package sqlexec runs the SQL of stage 2. This file and expr.go hold the
// values and the expressions.
//
// A value is a Go value of the table layer: nil for NULL, int64, float64,
// bool, string, []byte or time.Time. So a row of a table is a row of
// values, and no conversion sits between the two.
package sqlexec

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// sqlTypes maps the SQL type names of the parser to the column types.
var sqlTypes = map[string]table.Type{
	sqlparse.TypeInteger:   table.Int64,
	sqlparse.TypeReal:      table.Float64,
	sqlparse.TypeBoolean:   table.Bool,
	sqlparse.TypeText:      table.String,
	sqlparse.TypeBlob:      table.Bytes,
	sqlparse.TypeTimestamp: table.Time,
}

// TypeName returns the SQL name of a column type, for messages.
func TypeName(t table.Type) string {
	for n, tt := range sqlTypes {
		if tt == t {
			return n
		}
	}
	return t.String()
}

// typeOf returns the column type of a value that is not nil.
func typeOf(v any) (table.Type, bool) {
	switch v.(type) {
	case int64:
		return table.Int64, true
	case float64:
		return table.Float64, true
	case bool:
		return table.Bool, true
	case string:
		return table.String, true
	case []byte:
		return table.Bytes, true
	case time.Time:
		return table.Time, true
	}
	return 0, false
}

func numeric(t table.Type) bool { return t == table.Int64 || t == table.Float64 }

// comparable reports whether values of two types compare: the same type,
// or two numbers.
func comparable(a, b table.Type) bool { return a == b || numeric(a) && numeric(b) }

// compareIntFloat compares an int64 with a float64 exactly. Converting
// the int64 to float64 would round it above 2^53. f is not NaN.
func compareIntFloat(i int64, f float64) int {
	switch {
	case f >= 0x1p63:
		return -1
	case f < -0x1p63:
		return 1
	}
	t := math.Trunc(f)
	if ti := int64(t); i != ti {
		if i < ti {
			return -1
		}
		return 1
	}
	switch {
	case f > t:
		return -1
	case f < t:
		return 1
	}
	return 0
}

// compare orders two values that are not nil and whose types compare.
// It reports false if one of them is NaN: NaN has no place in the order.
func compare(a, b any) (int, bool) {
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return cmp3(x < y, x > y), true
		case float64:
			if math.IsNaN(y) {
				return 0, false
			}
			return compareIntFloat(x, y), true
		}
	case float64:
		if math.IsNaN(x) {
			return 0, false
		}
		switch y := b.(type) {
		case int64:
			return -compareIntFloat(y, x), true
		case float64:
			if math.IsNaN(y) {
				return 0, false
			}
			return cmp3(x < y, x > y), true
		}
	case bool:
		y := b.(bool)
		return cmp3(!x && y, x && !y), true
	case string:
		return strings.Compare(x, b.(string)), true
	case []byte:
		return bytes.Compare(x, b.([]byte)), true
	case time.Time:
		return x.Compare(b.(time.Time)), true
	}
	panic(fmt.Sprintf("sqlexec: compare %T with %T", a, b))
}

func cmp3(less, more bool) int {
	switch {
	case less:
		return -1
	case more:
		return 1
	}
	return 0
}

// realText prints a REAL with the fewest digits that give the value
// back. It uses an exponent below 1e-4 and from 1e15 on, and the
// mantissa always has a decimal point: 1.0, 0.5, 1.0e+20. SQLite prints
// 15 or 17 digits; see the oracle table in the requirements.
func realText(f float64) string {
	switch {
	case f == 0:
		return "0.0" // -0.0 too, as in SQLite
	case math.IsInf(f, 1):
		return "Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	mant, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	switch {
	case e < -4 || e >= 15:
		m := digits[:1] + "." + digits[1:]
		if len(digits) == 1 {
			m += "0"
		}
		es := "+"
		if e < 0 {
			es, e = "-", -e
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, e)
	case e < 0:
		return sign + "0." + strings.Repeat("0", -e-1) + digits
	case len(digits) > e+1:
		return sign + digits[:e+1] + "." + digits[e+1:]
	}
	return sign + digits + strings.Repeat("0", e+1-len(digits)) + ".0"
}

// Text prints a value for a person: the shell and messages use it.
func Text(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return realText(x)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return x
	case []byte:
		return fmt.Sprintf("X'%X'", x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprint(v)
}
