package sqlexec

import (
	"bytes"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// aggregates are the functions of GROUP BY. They are not scalar
// functions; step 21 of the roadmap runs them.
var aggregates = map[string]bool{"count": true, "sum": true, "avg": true, "total": true}

// scalar is a scalar function: its argument counts, the check of its
// arguments before the run, and the function. The function gets values
// that passed the checks. NULL handling is its own.
type scalar struct {
	min, max int // argument counts; max -1 is no bound
	// prepare checks the arguments and returns the result node, without
	// eval. It may keep what it needs for the run.
	prepare func(at sqlparse.At, args []node) (node, error)
	run     func(at sqlparse.At, args []node, vals []any) (any, error)
}

var scalars map[string]scalar

func init() {
	text := []table.Type{table.String}
	textOrBlob := []table.Type{table.String, table.Bytes}
	integer := []table.Type{table.Int64}
	// fixed returns a prepare that checks each argument against its list
	// of types and gives a result of type t.
	fixed := func(t table.Type, lists ...[]table.Type) func(sqlparse.At, []node) (node, error) {
		return func(at sqlparse.At, args []node) (node, error) {
			for i, a := range args {
				l := lists[len(lists)-1]
				if i < len(lists) {
					l = lists[i]
				}
				if err := want(a, "argument", l...); err != nil {
					return node{}, err
				}
			}
			return known(t), nil
		}
	}
	anyNull := func(vals []any) bool {
		for _, v := range vals {
			if v == nil {
				return true
			}
		}
		return false
	}
	scalars = map[string]scalar{
		"abs": {1, 1, func(at sqlparse.At, args []node) (node, error) {
			if err := want(args[0], "abs", numbers...); err != nil {
				return node{}, err
			}
			return node{typ: args[0].typ, known: args[0].known}, nil
		}, func(at sqlparse.At, args []node, vals []any) (any, error) {
			switch v := vals[0].(type) {
			case int64:
				if v == math.MinInt64 {
					return nil, errAt(at, "integer overflow: abs(%d)", v)
				}
				if v < 0 {
					return -v, nil
				}
				return v, nil
			case float64:
				if v < 0 {
					return -v, nil
				}
				return v, nil // -0.0 stays -0.0, as in SQLite
			}
			return nil, nil
		}},
		"length": {1, 1, fixed(table.Int64, textOrBlob), func(at sqlparse.At, args []node, vals []any) (any, error) {
			switch v := vals[0].(type) {
			case string:
				return int64(utf8.RuneCountInString(v)), nil
			case []byte:
				return int64(len(v)), nil
			}
			return nil, nil
		}},
		"lower": {1, 1, fixed(table.String, text), func(at sqlparse.At, args []node, vals []any) (any, error) {
			if vals[0] == nil {
				return nil, nil
			}
			return asciiCase(vals[0].(string), 'A', 'Z', 'a'-'A'), nil
		}},
		"upper": {1, 1, fixed(table.String, text), func(at sqlparse.At, args []node, vals []any) (any, error) {
			if vals[0] == nil {
				return nil, nil
			}
			return asciiCase(vals[0].(string), 'a', 'z', 'A'-'a'), nil
		}},
		"trim":  {1, 2, fixed(table.String, text), trimFunc(strings.Trim)},
		"ltrim": {1, 2, fixed(table.String, text), trimFunc(strings.TrimLeft)},
		"rtrim": {1, 2, fixed(table.String, text), trimFunc(strings.TrimRight)},
		"replace": {3, 3, fixed(table.String, text), func(at sqlparse.At, args []node, vals []any) (any, error) {
			if vals[0] == nil || vals[1] == nil {
				return nil, nil
			}
			// An empty pattern replaces nothing, so the replacement does
			// not matter, not even NULL. SQLite does the same.
			s, from := vals[0].(string), vals[1].(string)
			if from == "" {
				return s, nil
			}
			if vals[2] == nil {
				return nil, nil
			}
			return strings.ReplaceAll(s, from, vals[2].(string)), nil
		}},
		"instr": {2, 2, func(at sqlparse.At, args []node) (node, error) {
			for _, a := range args {
				if err := want(a, "instr", textOrBlob...); err != nil {
					return node{}, err
				}
			}
			if _, err := same(args, "instr"); err != nil {
				return node{}, err
			}
			return known(table.Int64), nil
		}, func(at sqlparse.At, args []node, vals []any) (any, error) {
			if anyNull(vals) {
				return nil, nil
			}
			switch a := vals[0].(type) {
			case string:
				b, ok := vals[1].(string)
				if !ok {
					return nil, errAt(args[1].at, "instr: %s is BLOB and %s is TEXT; they must have one type", args[1].desc, args[0].desc)
				}
				i := strings.Index(a, b)
				if i < 0 {
					return int64(0), nil
				}
				return int64(utf8.RuneCountInString(a[:i]) + 1), nil
			case []byte:
				b, ok := vals[1].([]byte)
				if !ok {
					return nil, errAt(args[1].at, "instr: %s is TEXT and %s is BLOB; they must have one type", args[1].desc, args[0].desc)
				}
				return int64(bytes.Index(a, b) + 1), nil
			}
			return nil, nil
		}},
		"substr": {2, 3, func(at sqlparse.At, args []node) (node, error) {
			if err := want(args[0], "substr", textOrBlob...); err != nil {
				return node{}, err
			}
			for _, a := range args[1:] {
				if err := want(a, "substr", integer...); err != nil {
					return node{}, err
				}
			}
			return node{typ: args[0].typ, known: args[0].known}, nil
		}, func(at sqlparse.At, args []node, vals []any) (any, error) {
			if anyNull(vals) {
				return nil, nil
			}
			p2, has2 := int64(0), len(vals) == 3
			if has2 {
				p2 = vals[2].(int64)
			}
			switch s := vals[0].(type) {
			case string:
				r := []rune(s)
				i, n := substrRange(int64(len(r)), vals[1].(int64), p2, has2)
				return string(r[i : i+n]), nil
			case []byte:
				i, n := substrRange(int64(len(s)), vals[1].(int64), p2, has2)
				return bytes.Clone(s[i : i+n]), nil
			}
			return nil, nil
		}},
		"coalesce": {2, -1, sameTypes("coalesce"), func(at sqlparse.At, args []node, vals []any) (any, error) {
			for _, v := range vals {
				if v != nil {
					return v, nil
				}
			}
			return nil, nil
		}},
		"ifnull": {2, 2, sameTypes("ifnull"), func(at sqlparse.At, args []node, vals []any) (any, error) {
			if vals[0] != nil {
				return vals[0], nil
			}
			return vals[1], nil
		}},
		"nullif": {2, 2, func(at sqlparse.At, args []node) (node, error) {
			if err := comparableNodes(args[0], args[1], "nullif"); err != nil {
				return node{}, err
			}
			return node{typ: args[0].typ, known: args[0].known}, nil
		}, func(at sqlparse.At, args []node, vals []any) (any, error) {
			eq, err := equal(args[0], args[1], vals[0], vals[1], "nullif")
			if err != nil {
				return nil, err
			}
			if eq == true {
				return nil, nil
			}
			return vals[0], nil
		}},
		"min": {2, -1, extremePrepare("min"), extreme(-1)},
		"max": {2, -1, extremePrepare("max"), extreme(1)},
		"round": {1, 2, fixed(table.Float64, numbers, integer), func(at sqlparse.At, args []node, vals []any) (any, error) {
			if anyNull(vals) {
				return nil, nil
			}
			digits := int64(0)
			if len(vals) == 2 {
				digits = vals[1].(int64)
			}
			return round(float(vals[0]), digits), nil
		}},
	}
}

func asciiCase(s string, lo, hi rune, shift rune) string {
	return strings.Map(func(r rune) rune {
		if r >= lo && r <= hi {
			return r + shift
		}
		return r
	}, s)
}

func trimFunc(f func(string, string) string) func(sqlparse.At, []node, []any) (any, error) {
	return func(at sqlparse.At, args []node, vals []any) (any, error) {
		for _, v := range vals {
			if v == nil {
				return nil, nil
			}
		}
		chars := " "
		if len(vals) == 2 {
			chars = vals[1].(string)
		}
		return f(vals[0].(string), chars), nil
	}
}

// sameTypes is the prepare of a function whose result is one of its
// arguments: they need one type.
func sameTypes(name string) func(sqlparse.At, []node) (node, error) {
	return func(at sqlparse.At, args []node) (node, error) {
		n, err := same(args, name)
		if err != nil {
			return node{}, err
		}
		for i := range args {
			args[i] = conform(args[i], n, name)
		}
		return n, nil
	}
}

// extremePrepare checks the arguments of the scalar min and max. They
// must compare. The result has a known type only if all have one type.
func extremePrepare(name string) func(sqlparse.At, []node) (node, error) {
	return func(at sqlparse.At, args []node) (node, error) {
		for _, b := range args[1:] {
			if err := comparableNodes(args[0], b, name); err != nil {
				return node{}, err
			}
		}
		n := node{typ: args[0].typ, known: true}
		for _, a := range args {
			if !a.known || a.typ != n.typ {
				n.known = false
			}
		}
		return n, nil
	}
}

// extreme returns the smallest (sign -1) or largest (sign 1) argument,
// or NULL if one is NULL. Of equal values, min returns the last and max
// the first, as in SQLite: min(1, 1.0) is 1.0, max(1.0, 1) is 1.0.
func extreme(sign int) func(sqlparse.At, []node, []any) (any, error) {
	return func(at sqlparse.At, args []node, vals []any) (any, error) {
		best := 0
		for i, v := range vals {
			if v == nil {
				return nil, nil
			}
			if i == 0 {
				continue
			}
			if err := comparableValues(args[best], args[i], vals[best], v, "min and max"); err != nil {
				return nil, err
			}
			d, ok := compare(v, vals[best])
			if !ok {
				return nil, nil
			}
			if d*sign > 0 || d == 0 && sign < 0 {
				best = i
			}
		}
		return vals[best], nil
	}
}

// substrRange returns the start and the length of substr(x, p1, p2) in a
// value of n characters or bytes. It follows SQLite: p1 counts from 1,
// a negative p1 counts from the end, and a negative p2 takes the
// characters before p1.
func substrRange(n, p1, p2 int64, has2 bool) (int64, int64) {
	const bound = 1 << 40 // far above any length; keeps the sums below overflow
	p1 = max(-bound, min(bound, p1))
	neg := false
	if !has2 {
		p2 = bound
	} else {
		p2 = max(-bound, min(bound, p2))
		if p2 < 0 {
			p2, neg = -p2, true
		}
	}
	switch {
	case p1 < 0:
		p1 += n
		if p1 < 0 {
			p2 += p1
			if p2 < 0 {
				p2 = 0
			}
			p1 = 0
		}
	case p1 > 0:
		p1--
	case p2 > 0:
		p2--
	}
	if neg {
		p1 -= p2
		if p1 < 0 {
			p2 += p1
			p1 = 0
		}
	}
	if p1 > n {
		p1 = n
	}
	if p1+p2 > n {
		p2 = n - p1
	}
	return p1, max(p2, 0)
}

// round rounds f to digits decimal places, half away from zero. It
// works on the exact binary value of f: round(2.675, 2) is 2.67,
// because 2.675 is stored as 2.67499... A negative digits is 0.
func round(f float64, digits int64) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return f
	}
	digits = max(0, min(digits, 400))
	neg := f < 0
	// Every float64 has an exact decimal form with at most 1074 digits
	// after the point.
	s := new(big.Float).SetFloat64(math.Abs(f)).Text('f', 1100)
	intPart, frac, _ := strings.Cut(s, ".")
	d := []byte(intPart + frac[:digits])
	if frac[digits] >= '5' {
		i := len(d) - 1
		for i >= 0 && d[i] == '9' {
			d[i] = '0'
			i--
		}
		if i < 0 {
			d = append([]byte{'1'}, d...)
		} else {
			d[i]++
		}
	}
	point := len(d) - int(digits)
	text := string(d[:point]) + "." + string(d[point:]) + "0"
	r, _ := strconv.ParseFloat(text, 64)
	// SQLite rounds to no decimal places through an integer when the
	// value fits 2^52, and an integer has no -0: round(-0.4) is 0.0.
	// Other results keep the sign: round(-0.001, 2) is -0.0.
	if neg && !(r == 0 && digits == 0 && f >= -0x1p52) {
		r = -r
	}
	return r
}

func (c *compiler) call(x *sqlparse.Call) (node, error) {
	if aggregates[x.Name] || x.Star || x.Distinct || (x.Name == "min" || x.Name == "max") && len(x.Args) == 1 {
		return node{}, errAt(x.At, "aggregate function %s is not allowed here", x.Name)
	}
	f, ok := scalars[x.Name]
	if !ok {
		return node{}, errAt(x.At, "no such function: %s", x.Name)
	}
	if len(x.Args) < f.min || f.max >= 0 && len(x.Args) > f.max {
		want := strconv.Itoa(f.min)
		switch {
		case f.max < 0:
			want = "at least " + want
		case f.max != f.min:
			want += " or " + strconv.Itoa(f.max)
		}
		return node{}, errAt(x.At, "%s takes %s arguments, not %d", x.Name, want, len(x.Args))
	}
	args, err := c.exprs(x.Args)
	if err != nil {
		return node{}, err
	}
	n, err := f.prepare(x.At, args)
	if err != nil {
		return node{}, err
	}
	at, name := x.At, x.Name
	n.eval = func(e *env) (any, error) {
		vals := make([]any, len(args))
		for i, a := range args {
			v, err := a.eval(e)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		if err := checkArgs(name, args, vals); err != nil {
			return nil, err
		}
		return f.run(at, args, vals)
	}
	return n, nil
}

// argTypes are the types each function takes, for the check at run
// time of an argument whose type was not known before.
var argTypes = map[string][][]table.Type{
	"abs":     {numbers},
	"length":  {{table.String, table.Bytes}},
	"lower":   {{table.String}},
	"upper":   {{table.String}},
	"trim":    {{table.String}},
	"ltrim":   {{table.String}},
	"rtrim":   {{table.String}},
	"replace": {{table.String}},
	"instr":   {{table.String, table.Bytes}},
	"substr":  {{table.String, table.Bytes}, {table.Int64}},
	"round":   {numbers, {table.Int64}},
}

func checkArgs(name string, args []node, vals []any) error {
	lists, ok := argTypes[name]
	if !ok {
		return nil
	}
	for i, a := range args {
		l := lists[len(lists)-1]
		if i < len(lists) {
			l = lists[i]
		}
		if err := check(a, vals[i], name, l...); err != nil {
			return err
		}
	}
	return nil
}

// casts lists the CASTs that exist, from a type to a type. Any other one
// is an error. The same type is always allowed.
var casts = map[[2]table.Type]bool{
	{table.Int64, table.Float64}: true, {table.Int64, table.String}: true, {table.Int64, table.Bool}: true,
	{table.Float64, table.Int64}: true, {table.Float64, table.String}: true,
	{table.Bool, table.Int64}: true, {table.Bool, table.Float64}: true, {table.Bool, table.String}: true,
	{table.String, table.Int64}: true, {table.String, table.Float64}: true, {table.String, table.Bool}: true,
	{table.String, table.Bytes}: true, {table.String, table.Time}: true,
	{table.Bytes, table.String}: true,
	{table.Time, table.String}:  true,
}

func (c *compiler) cast(x *sqlparse.Cast) (node, error) {
	a, err := c.expr(x.X)
	if err != nil {
		return node{}, err
	}
	to, ok := sqlTypes[x.Type]
	if !ok {
		return node{}, errAt(x.At, "unknown type %s", x.Type)
	}
	if a.known && a.typ != to && !casts[[2]table.Type{a.typ, to}] {
		return node{}, errAt(x.At, "no CAST from %s to %s", TypeName(a.typ), TypeName(to))
	}
	n := known(to)
	n.eval = func(e *env) (any, error) {
		v, err := a.eval(e)
		if err != nil || v == nil {
			return nil, err
		}
		from, _ := typeOf(v)
		if from == to {
			return v, nil
		}
		if !casts[[2]table.Type{from, to}] {
			return nil, errAt(x.At, "no CAST from %s to %s", TypeName(from), TypeName(to))
		}
		return castValue(x.At, v, to)
	}
	return n, nil
}

// castValue converts v to type to. A value that does not convert exactly
// or completely is an error: '12abc' is no INTEGER, and 1e30 does not fit
// one.
func castValue(at sqlparse.At, v any, to table.Type) (any, error) {
	fail := func() (any, error) {
		return nil, errAt(at, "CAST of %s to %s: the value does not convert", sqlLiteral(v), TypeName(to))
	}
	switch x := v.(type) {
	case int64:
		switch to {
		case table.Float64:
			return float64(x), nil
		case table.String:
			return strconv.FormatInt(x, 10), nil
		case table.Bool:
			if x == 0 || x == 1 {
				return x == 1, nil
			}
			return fail()
		}
	case float64:
		switch to {
		case table.Int64:
			if math.IsNaN(x) || x >= 0x1p63 || x < -0x1p63 {
				return fail()
			}
			return int64(x), nil
		case table.String:
			return realText(x), nil
		}
	case bool:
		switch to {
		case table.Int64:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case table.Float64:
			if x {
				return 1.0, nil
			}
			return 0.0, nil
		case table.String:
			return strconv.FormatBool(x), nil
		}
	case string:
		s := strings.Trim(x, " \t\n\r")
		switch to {
		case table.Int64:
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i, nil
			}
		case table.Float64:
			if s != "" && strings.Trim(s, "0123456789.eE+-") == "" {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					return f, nil
				}
			}
		case table.Bool:
			switch strings.ToLower(s) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
		case table.Bytes:
			return []byte(x), nil
		case table.Time:
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t.UTC(), nil
			}
		}
		return fail()
	case []byte:
		if to == table.String && utf8.Valid(x) {
			return string(x), nil
		}
		return fail()
	case time.Time:
		if to == table.String {
			return x.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return fail()
}

// sqlLiteral prints a value as SQL, for messages.
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return sqlparse.Quote(x)
	case time.Time:
		return sqlparse.Quote(x.UTC().Format(time.RFC3339Nano))
	}
	return Text(v)
}
