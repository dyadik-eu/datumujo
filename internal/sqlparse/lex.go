package sqlparse

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Error is an error in the SQL text or in its run. It names the
// position. Err is the cause, if another error caused it; errors.Is
// finds it.
type Error struct {
	At  At
	Msg string
	Err error
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.At.Line, e.At.Col, e.Msg)
}

func errAt(at At, format string, a ...any) *Error {
	return &Error{At: at, Msg: fmt.Sprintf(format, a...)}
}

type kind int

const (
	tEOF     kind = iota
	tIdent        // A name without quotes, in lower case.
	tQuoted       // A name in double quotes, as written.
	tKeyword      // A keyword, in upper case.
	tInt
	tFloat // A number with a point or an exponent.
	tString
	tBlob  // X'...', the value in text.
	tParam // ? or ?NNN; the text is the number or empty.
	tOp
)

type token struct {
	kind kind
	text string
	at   At
}

// keywords are reserved: a column or table with such a name needs double
// quotes. Type names and function names are not keywords. Neither are
// the words the parser knows by their place: ADD, BEGIN, CHECK, COLUMN,
// COMMIT, CURRENT_TIMESTAMP, DEFAULT, EXISTS, IF, INDEX, KEY, ROLLBACK and
// TRANSACTION. So a column can be called key, index, default or check.
var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`ALTER AND AS ASC BETWEEN BY CASE CAST CREATE
		DELETE DESC DISTINCT DROP ELSE END FALSE FROM GROUP HAVING IN INNER INSERT
		INTO IS JOIN LEFT LIKE LIMIT NOT NULL OFFSET ON OR ORDER OUTER PRIMARY
		SELECT SET TABLE THEN TRUE UNIQUE UPDATE VALUES WHEN WHERE`) {
		keywords[k] = true
	}
}

// lexer cuts the text into tokens. It keeps the position as line and
// column of characters.
type lexer struct {
	src  string
	off  int
	line int
	col  int
}

func (l *lexer) at() At { return At{l.line, l.col} }

// peekByte returns the byte at the offset plus n, or 0 past the end.
func (l *lexer) peekByte(n int) byte {
	if l.off+n < len(l.src) {
		return l.src[l.off+n]
	}
	return 0
}

// advance moves past one character.
func (l *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.off:])
	l.off += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// space skips white space and comments.
func (l *lexer) space() error {
	for l.off < len(l.src) {
		c := l.src[l.off]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.advance()
		case c == '-' && l.peekByte(1) == '-':
			for l.off < len(l.src) && l.src[l.off] != '\n' {
				l.advance()
			}
		case c == '/' && l.peekByte(1) == '*':
			start := l.at()
			l.advance()
			l.advance()
			for {
				if l.off >= len(l.src) {
					return errAt(start, "comment without */")
				}
				if l.src[l.off] == '*' && l.peekByte(1) == '/' {
					l.advance()
					l.advance()
					break
				}
				l.advance()
			}
		default:
			return nil
		}
	}
	return nil
}

func isLetter(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

// next returns the next token.
func (l *lexer) next() (token, error) {
	if err := l.space(); err != nil {
		return token{}, err
	}
	at := l.at()
	if l.off >= len(l.src) {
		return token{kind: tEOF, at: at}, nil
	}
	c := l.src[l.off]
	switch {
	case (c == 'x' || c == 'X') && l.peekByte(1) == '\'':
		l.advance()
		s, err := l.quoted('\'', at)
		if err != nil {
			return token{}, err
		}
		if _, err := hex.DecodeString(s); err != nil {
			return token{}, errAt(at, "blob literal: %q is not an even number of hex digits", s)
		}
		return token{tBlob, s, at}, nil
	case isLetter(c):
		start := l.off
		for l.off < len(l.src) && (isLetter(l.src[l.off]) || isDigit(l.src[l.off])) {
			l.advance()
		}
		word := l.src[start:l.off]
		if up := strings.ToUpper(word); keywords[up] {
			return token{tKeyword, up, at}, nil
		}
		return token{tIdent, strings.ToLower(word), at}, nil
	case c == '"':
		s, err := l.quoted('"', at)
		if err != nil {
			return token{}, err
		}
		if s == "" {
			return token{}, errAt(at, "empty name in double quotes")
		}
		if strings.ContainsRune(s, 0) {
			return token{}, errAt(at, "a name cannot hold the character NUL")
		}
		return token{tQuoted, s, at}, nil
	case c == '\'':
		s, err := l.quoted('\'', at)
		if err != nil {
			return token{}, err
		}
		return token{tString, s, at}, nil
	case isDigit(c) || c == '.' && isDigit(l.peekByte(1)):
		return l.number(at)
	case c == '?':
		l.advance()
		start := l.off
		for l.off < len(l.src) && isDigit(l.src[l.off]) {
			l.advance()
		}
		return token{tParam, l.src[start:l.off], at}, nil
	}
	for _, op := range []string{"==", "!=", "<>", "<=", ">=", "||"} {
		if strings.HasPrefix(l.src[l.off:], op) {
			l.advance()
			l.advance()
			return token{tOp, op, at}, nil
		}
	}
	if strings.IndexByte("(),;.*+-/%=<>", c) >= 0 {
		l.advance()
		return token{tOp, string(c), at}, nil
	}
	r := l.advance()
	if r == utf8.RuneError {
		return token{}, errAt(at, "the text is not UTF-8")
	}
	return token{}, errAt(at, "unexpected character %q", r)
}

// quoted reads text between two quote characters. A doubled quote stands
// for one.
func (l *lexer) quoted(q byte, at At) (string, error) {
	l.advance()
	var b strings.Builder
	for {
		if l.off >= len(l.src) {
			return "", errAt(at, "%c without its closing %c", q, q)
		}
		c := l.src[l.off]
		if c == q {
			l.advance()
			if l.off < len(l.src) && l.src[l.off] == q {
				l.advance()
				b.WriteByte(q)
				continue
			}
			return b.String(), nil
		}
		start := l.off
		if r := l.advance(); r == utf8.RuneError {
			return "", errAt(at, "the text is not UTF-8")
		}
		b.WriteString(l.src[start:l.off])
	}
}

// number reads 12, 1.5, .5, 1e10 or 1.5E-3. A letter right after a
// number is an error: 12abc is no name and no number.
func (l *lexer) number(at At) (token, error) {
	start := l.off
	float := false
	for l.off < len(l.src) && isDigit(l.src[l.off]) {
		l.advance()
	}
	if l.off < len(l.src) && l.src[l.off] == '.' {
		float = true
		l.advance()
		for l.off < len(l.src) && isDigit(l.src[l.off]) {
			l.advance()
		}
	}
	if c := l.peekByte(0); c == 'e' || c == 'E' {
		float = true
		l.advance()
		if c := l.peekByte(0); c == '+' || c == '-' {
			l.advance()
		}
		if !isDigit(l.peekByte(0)) {
			return token{}, errAt(at, "number %q: no digits in the exponent", l.src[start:l.off])
		}
		for l.off < len(l.src) && isDigit(l.src[l.off]) {
			l.advance()
		}
	}
	if c := l.peekByte(0); isLetter(c) || c == '.' {
		return token{}, errAt(at, "number %q runs into %q", l.src[start:l.off], c)
	}
	text := l.src[start:l.off]
	if float {
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return token{}, errAt(at, "number %s is out of range", text)
		}
		return token{tFloat, text, at}, nil
	}
	return token{tInt, text, at}, nil
}
