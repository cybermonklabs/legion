package charts

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// decode reads JSON as Python's json.loads does, where the answers of the checks depend on it, and
// as encoding/json does not:
//
//   - a string may hold a lone surrogate escape, such as "\ud800", which Python keeps as a
//     character that is not printable and encoding/json turns into U+FFFD, which is;
//   - an integer literal of more than 4,300 digits is refused, as Python refuses to turn one into
//     an int;
//   - a number with a fraction or an exponent is a float, any other an int of any size.
//
// It refuses what Python accepts and no request body holds: NaN, Infinity and -Infinity.

// maxDigits is the most digits of an integer literal that Python's int() reads by default.
const maxDigits = 4300

// maxDepth is how deep arrays and objects may nest. Python's parser reaches about 10,000 before
// its recursion limit stops it, as encoding/json's does.
const maxDepth = 10000

type parser struct {
	text  string
	at    int
	depth int
}

func decode(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("the charts are not UTF-8 text")
	}
	p := &parser{text: string(raw)}
	p.skipSpace()
	value, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.at < len(p.text) {
		return nil, p.fail("extra data")
	}
	return value, nil
}

func (p *parser) fail(reason string) error {
	return fmt.Errorf("%s at byte %d", reason, p.at)
}

func (p *parser) skipSpace() {
	for p.at < len(p.text) && strings.IndexByte(" \t\n\r", p.text[p.at]) >= 0 {
		p.at++
	}
}

func (p *parser) value() (any, error) {
	if p.at >= len(p.text) {
		return nil, p.fail("expected a value")
	}
	switch char := p.text[p.at]; {
	case char == '{':
		return p.object()
	case char == '[':
		return p.list()
	case char == '"':
		return p.string()
	case char == '-' || '0' <= char && char <= '9':
		return p.number()
	}
	for _, word := range []struct {
		text  string
		value any
	}{{"true", true}, {"false", false}, {"null", nil}} {
		if strings.HasPrefix(p.text[p.at:], word.text) {
			p.at += len(word.text)
			return word.value, nil
		}
	}
	return nil, p.fail("expected a value")
}

func (p *parser) list() (any, error) {
	if p.depth++; p.depth > maxDepth {
		return nil, p.fail("nested too deep")
	}
	defer func() { p.depth-- }()
	p.at++
	list := []any{}
	p.skipSpace()
	if p.at < len(p.text) && p.text[p.at] == ']' {
		p.at++
		return list, nil
	}
	for {
		p.skipSpace()
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		list = append(list, item)
		p.skipSpace()
		switch {
		case p.at < len(p.text) && p.text[p.at] == ',':
			p.at++
		case p.at < len(p.text) && p.text[p.at] == ']':
			p.at++
			return list, nil
		default:
			return nil, p.fail("expected , or ]")
		}
	}
}

func (p *parser) object() (any, error) {
	if p.depth++; p.depth > maxDepth {
		return nil, p.fail("nested too deep")
	}
	defer func() { p.depth-- }()
	p.at++
	fields := newObject()
	p.skipSpace()
	if p.at < len(p.text) && p.text[p.at] == '}' {
		p.at++
		return fields, nil
	}
	for {
		p.skipSpace()
		if p.at >= len(p.text) || p.text[p.at] != '"' {
			return nil, p.fail("expected a key")
		}
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.at >= len(p.text) || p.text[p.at] != ':' {
			return nil, p.fail("expected :")
		}
		p.at++
		p.skipSpace()
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		fields.set(key.(string), item)
		p.skipSpace()
		switch {
		case p.at < len(p.text) && p.text[p.at] == ',':
			p.at++
		case p.at < len(p.text) && p.text[p.at] == '}':
			p.at++
			return fields, nil
		default:
			return nil, p.fail("expected , or }")
		}
	}
}

// string reads a string from its opening quote. A lone surrogate escape is kept as its WTF-8
// bytes (surrogate); a high surrogate escape followed by a low one is the one character they spell.
func (p *parser) string() (any, error) {
	p.at++
	var out strings.Builder
	for p.at < len(p.text) {
		char := p.text[p.at]
		switch {
		case char == '"':
			p.at++
			return out.String(), nil
		case char < 0x20:
			return nil, p.fail("a control character in a string")
		case char != '\\':
			out.WriteByte(char)
			p.at++
			continue
		}
		if p.at+1 >= len(p.text) {
			break
		}
		if escaped, ok := simpleEscapes[p.text[p.at+1]]; ok {
			out.WriteByte(escaped)
			p.at += 2
			continue
		}
		if p.text[p.at+1] != 'u' {
			return nil, p.fail("an unknown escape")
		}
		code, ok := p.hex(p.at + 2)
		if !ok {
			return nil, p.fail("an invalid \\u escape")
		}
		p.at += 6
		if 0xD800 <= code && code <= 0xDBFF && strings.HasPrefix(p.text[p.at:], `\u`) {
			if low, ok := p.hex(p.at + 2); !ok {
				return nil, p.fail("an invalid \\u escape")
			} else if 0xDC00 <= low && low <= 0xDFFF {
				code = 0x10000 + (code-0xD800)<<10 + (low - 0xDC00)
				p.at += 6
			}
		}
		if 0xD800 <= code && code <= 0xDFFF {
			out.WriteString(surrogate(code))
		} else {
			out.WriteRune(code)
		}
	}
	return nil, p.fail("a string that does not end")
}

var simpleEscapes = map[byte]byte{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}

// hex reads the four hexadecimal digits at at.
func (p *parser) hex(at int) (rune, bool) {
	if at+4 > len(p.text) {
		return 0, false
	}
	code, err := strconv.ParseUint(p.text[at:at+4], 16, 16)
	return rune(code), err == nil
}

// number reads a number by JSON's grammar: a float when it has a fraction or an exponent, with
// Python's float(), which answers ±Inf past a float's range, else an int of any size of at most
// maxDigits digits. A leading zero ends the whole part, as in Python: 01 is 0 and then extra data.
func (p *parser) number() (any, error) {
	start := p.at
	if p.text[p.at] == '-' {
		p.at++
	}
	digits := p.at
	if p.at < len(p.text) && p.text[p.at] == '0' {
		p.at++
	} else if p.digits() == "" {
		return nil, p.fail("a number with no digits")
	}
	whole := p.at - digits
	isFloat := false
	if p.at+1 < len(p.text) && p.text[p.at] == '.' && isDigit(p.text[p.at+1]) {
		p.at++
		p.digits()
		isFloat = true
	}
	if p.at < len(p.text) && (p.text[p.at] == 'e' || p.text[p.at] == 'E') {
		sign := 0
		if p.at+1 < len(p.text) && (p.text[p.at+1] == '+' || p.text[p.at+1] == '-') {
			sign = 1
		}
		if p.at+1+sign < len(p.text) && isDigit(p.text[p.at+1+sign]) {
			p.at += 1 + sign
			p.digits()
			isFloat = true
		}
	}
	text := p.text[start:p.at]
	if isFloat {
		value, _ := strconv.ParseFloat(text, 64)
		return value, nil
	}
	if whole > maxDigits {
		return nil, fmt.Errorf("an integer of %d digits, and at most %d are read, at byte %d", whole, maxDigits, start)
	}
	value, _ := new(big.Int).SetString(text, 10)
	return value, nil
}

func (p *parser) digits() string {
	start := p.at
	for p.at < len(p.text) && isDigit(p.text[p.at]) {
		p.at++
	}
	return p.text[start:p.at]
}

func isDigit(char byte) bool {
	return '0' <= char && char <= '9'
}
