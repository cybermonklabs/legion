package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// SignalProvider is a project's model provider as the provider's headroom signal is configured:
// the uncached input tokens of a minute over the project's own limit, and which models count
// toward it. Each field is optional, and its zero value is the key unset.
type SignalProvider struct {
	// TokenLimit is `signal_token_limit_per_minute`, the uncached input tokens the project may
	// spend in a minute, and 0 where the file names none: the headroom then reads as not
	// recorded, never as 0.
	TokenLimit int64
	// TokenModels is `signal_token_model_pattern`, the models whose tokens count against the
	// limit; "" where the file names none, under which every model's tokens count. A request of
	// another model is kept all the same, and its tokens count nowhere.
	TokenModels string
}

// readSignalProvider reads a project's two provider keys out of fields, its members, under key,
// the project's own path. A key the project leaves out keeps its zero value.
func readSignalProvider(fields map[string]*yaml.Node, key string) (SignalProvider, error) {
	var provider SignalProvider
	if limit := fields["signal_token_limit_per_minute"]; limit != nil {
		read, err := readPositiveInteger(limit, key+".signal_token_limit_per_minute", 0)
		if err != nil {
			return SignalProvider{}, err
		}
		provider.TokenLimit = int64(*read)
	}
	if value := fields["signal_token_model_pattern"]; value != nil {
		read, err := readSharedPattern(value, key+".signal_token_model_pattern")
		if err != nil {
			return SignalProvider{}, err
		}
		provider.TokenModels = read
	}
	return provider, nil
}

// readSharedPattern is a pattern key's value, refused naming the key and the reason where it is
// outside the subset SharedPattern holds.
func readSharedPattern(value *yaml.Node, key string) (string, error) {
	pattern, err := stringOf(value, key)
	if err != nil {
		return "", err
	}
	if err := checkSharedPattern(pattern); err != nil {
		return "", fmt.Errorf("%s %s is outside the subset of patterns Go and JavaScript both read alike: %w", key, strconv.Quote(pattern), err)
	}
	return pattern, nil
}

// SharedPattern compiles pattern, one the loader took (checkSharedPattern), as every Go reader of
// it does: with `.` matching every character, a newline included. A JavaScript reader compiles the
// same pattern as `new RegExp(pattern, "su")`, under which the two read every pattern of the
// subset alike.
func SharedPattern(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("(?s)" + pattern)
}

// maxRepeat is the largest count a repetition may name: Go's regexp refuses one above it.
const maxRepeat = 1000

// checkSharedPattern refuses pattern unless it is in the subset of regular expressions that Go's
// regexp (as SharedPattern compiles it) and JavaScript's RegExp (with the flags `su`) both accept
// and read alike. The subset is written as what it holds, so anything else is refused:
//
//   - a character that is not one of `\ . ^ $ | ? * + ( ) [ ] { }`, which is itself;
//   - `\` before one of those, or before `/`, which is that character;
//   - `.`, any character; `^` and `$`, the text's start and end; `\b` and `\B`, a word boundary
//     and its absence; `\d \D \w \W`, the ASCII digits and word characters and their complements;
//     `\t \n \r`, the tab, the newline and the carriage return;
//   - a class `[…]` or `[^…]` of at least one member: a character, a range `a-z` between two
//     characters, `\` before one of `\ ] [ - ^`, `\d \D \w \W` and `\t \n \r`;
//   - a group `(…)` or `(?:…)`, and the alternation `|`;
//   - after an atom that is no assertion, one repetition `*`, `+`, `?`, `{n}`, `{n,}` or `{n,m}`
//     with n ≤ m ≤ 1000, optionally followed by `?`, but none of a group that holds a repetition.
//
// Each construct left out is one of the two engines refusing it — a lookaround, a back
// reference, a named group, an inline flag, `\p`, and a `-` after `\d \D \w \W` in a class
// before anything but the class's end, which Go reads as the character and JavaScript's `u` mode
// refuses as a range — or one they read differently: `\s` is ASCII's spaces in Go and Unicode's in
// JavaScript, `[[:digit:]]` a POSIX class in Go and members in JavaScript, `\A` and `\Q` Go's
// alone, `[]` a class's first member in Go and an empty class in JavaScript, and a lone `{` or `]`
// a literal in one reading and a refusal in the other. A repetition of a group that holds one,
// such as `(a+)+` or `(.*)*`, matches in a time linear in the text in Go and backtracks in
// JavaScript for a time that grows exponentially with it. An empty pattern matches every text,
// which would count every model's tokens as the pattern's; a key with no pattern is the key left
// out.
func checkSharedPattern(pattern string) error {
	if pattern == "" {
		return errors.New("it is empty, and an empty pattern matches every text")
	}
	p := patternReader{src: pattern}
	if _, err := p.alternation(); err != nil {
		return err
	}
	if _, err := SharedPattern(pattern); err != nil {
		return fmt.Errorf("Go's regexp refuses it: %w", err)
	}
	return nil
}

// patternReader reads one pattern of the subset checkSharedPattern holds, from pos on.
type patternReader struct {
	src   string
	pos   int
	depth int
}

// escapes are the escapes outside a class the subset holds: the metacharacters and `/`, which
// are themselves, and the classes and controls.
const (
	literalEscapes = `\.^$|?*+()[]{}/`
	classEscapes   = `dDwWtnr`
	inClassEscapes = `\][-^`
)

// alternation reads sequences separated by `|`, up to the end of the pattern or the `)` that
// closes the group it is read inside, and answers whether any of them holds a repetition.
func (p *patternReader) alternation() (bool, error) {
	var repeats bool
	for {
		holds, err := p.sequence()
		if err != nil {
			return false, err
		}
		repeats = repeats || holds
		if p.pos >= len(p.src) || p.src[p.pos] != '|' {
			return repeats, nil
		}
		p.pos++
	}
}

// sequence reads atoms, each with its repetition, up to `|`, `)` or the end, and answers whether
// it holds a repetition, one inside a group of it included.
func (p *patternReader) sequence() (bool, error) {
	var repeats bool
	for p.pos < len(p.src) {
		at := p.pos
		repeatable, inside := true, false
		switch c := p.src[p.pos]; c {
		case '|':
			return repeats, nil
		case ')':
			if p.depth == 0 {
				return false, fmt.Errorf("the ) at %d closes no group", at)
			}
			return repeats, nil
		case '(':
			var err error
			if inside, err = p.group(); err != nil {
				return false, err
			}
		case '[':
			if err := p.class(); err != nil {
				return false, err
			}
		case '^', '$':
			p.pos++
			repeatable = false
		case '.':
			p.pos++
		case '\\':
			assertion, err := p.escape()
			if err != nil {
				return false, err
			}
			repeatable = !assertion
		case '*', '+', '?':
			return false, fmt.Errorf("the %c at %d repeats nothing", c, at)
		case '{':
			if _, ok := p.bound(); ok {
				return false, fmt.Errorf("the repetition at %d repeats nothing", at)
			}
			return false, fmt.Errorf("the { at %d begins no repetition; write \\{ for the character", at)
		case '}':
			return false, fmt.Errorf("the } at %d closes no repetition; write \\} for the character", at)
		case ']':
			return false, fmt.Errorf("the ] at %d closes no class; write \\] for the character", at)
		default:
			p.pos++
		}
		op := p.pos
		repeated, err := p.repetition(repeatable, at)
		if err != nil {
			return false, err
		}
		if repeated && inside {
			return false, fmt.Errorf("the repetition at %d repeats a group that holds a repetition, which JavaScript backtracks through "+
				"for a time that grows exponentially with the text; repeat the group or what is inside it, not both", op)
		}
		repeats = repeats || repeated || inside
	}
	return repeats, nil
}

// group reads `(…)` or `(?:…)`, and answers whether it holds a repetition.
func (p *patternReader) group() (bool, error) {
	open := p.pos
	p.pos++
	if strings.HasPrefix(p.src[p.pos:], "?") {
		if !strings.HasPrefix(p.src[p.pos:], "?:") {
			return false, fmt.Errorf("(? at %d opens a lookaround, a named group or a flag; only (?: is in the subset", open)
		}
		p.pos += 2
	}
	p.depth++
	repeats, err := p.alternation()
	if err != nil {
		return false, err
	}
	p.depth--
	if p.pos >= len(p.src) {
		return false, fmt.Errorf("a group opened at %d is never closed", open)
	}
	p.pos++
	return repeats, nil
}

// escape reads `\` and the character after it outside a class, and reports whether it is an
// assertion, which nothing may repeat.
func (p *patternReader) escape() (bool, error) {
	at := p.pos
	if at+1 >= len(p.src) {
		return false, fmt.Errorf(`the \ at %d escapes nothing`, at)
	}
	c := p.src[at+1]
	p.pos += 2
	switch {
	case c == 'b' || c == 'B':
		return true, nil
	case strings.IndexByte(literalEscapes, c) >= 0, strings.IndexByte(classEscapes, c) >= 0:
		return false, nil
	case c == 's' || c == 'S':
		return false, fmt.Errorf(`\%c at %d matches the spaces of Unicode in JavaScript and only ASCII's in Go; write the spaces as a class, such as [ \t]`, c, at)
	}
	return false, fmt.Errorf(`\%c at %d is not in the subset`, c, at)
}

// class reads `[…]` or `[^…]`.
func (p *patternReader) class() error {
	open := p.pos
	p.pos++
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		return fmt.Errorf("the class at %d opens with ], which Go reads as a member and JavaScript as the class's end", open)
	}
	for p.pos < len(p.src) {
		at := p.pos
		switch c := p.src[p.pos]; c {
		case ']':
			p.pos++
			return nil
		case '[':
			return fmt.Errorf("the [ at %d inside a class: Go reads [:name:] as a POSIX class, and JavaScript as members; write \\[ for the character", at)
		}
		low, single, err := p.member()
		if err != nil {
			return err
		}
		dash := p.pos+1 < len(p.src) && p.src[p.pos] == '-' && p.src[p.pos+1] != ']'
		if !single && dash {
			return fmt.Errorf(`the - at %d follows a class escape, which begins no range: Go reads it as the character and JavaScript refuses it; write \- for the character`, p.pos)
		}
		if !dash {
			continue
		}
		p.pos++
		high, single, err := p.member()
		if err != nil {
			return err
		}
		if !single {
			return fmt.Errorf("the range at %d ends in a class rather than a character", at)
		}
		if high < low {
			return fmt.Errorf("the range at %d runs backwards", at)
		}
	}
	return fmt.Errorf("a class opened at %d is never closed", open)
}

// member reads one member of a class: a character, or an escape, and answers the character and
// whether it is one, rather than a class such as \d.
func (p *patternReader) member() (rune, bool, error) {
	at := p.pos
	if p.src[at] != '\\' {
		r, size := utf8.DecodeRuneInString(p.src[at:])
		p.pos += size
		return r, true, nil
	}
	if at+1 >= len(p.src) {
		return 0, false, fmt.Errorf(`the \ at %d escapes nothing`, at)
	}
	c := p.src[at+1]
	p.pos += 2
	switch {
	case strings.IndexByte(inClassEscapes, c) >= 0:
		return rune(c), true, nil
	case c == 't':
		return '\t', true, nil
	case c == 'n':
		return '\n', true, nil
	case c == 'r':
		return '\r', true, nil
	case strings.IndexByte("dDwW", c) >= 0:
		return 0, false, nil
	case c == 's' || c == 'S':
		return 0, false, fmt.Errorf(`\%c at %d matches the spaces of Unicode in JavaScript and only ASCII's in Go; write the spaces as a class, such as [ \t]`, c, at)
	}
	return 0, false, fmt.Errorf(`\%c at %d is not in the subset`, c, at)
}

// repetition reads the repetition after the atom that began at atom, if any, which only a
// repeatable atom may carry, and its `?`, and answers whether it read one.
func (p *patternReader) repetition(repeatable bool, atom int) (bool, error) {
	if p.pos >= len(p.src) {
		return false, nil
	}
	at := p.pos
	switch p.src[at] {
	case '*', '+', '?':
		p.pos++
	case '{':
		counts, ok := p.bound()
		if !ok {
			return false, fmt.Errorf("the { at %d begins no repetition; write \\{ for the character", at)
		}
		if counts[0] > maxRepeat || counts[1] > maxRepeat || counts[1] >= 0 && counts[1] < counts[0] {
			return false, fmt.Errorf("the repetition at %d is outside 0 to %d, the bound Go's regexp takes", at, maxRepeat)
		}
	default:
		return false, nil
	}
	if !repeatable {
		return false, fmt.Errorf("the %c at %d repeats an assertion, which matches no character", p.src[at], at)
	}
	if p.pos < len(p.src) && p.src[p.pos] == '?' {
		p.pos++
	}
	if p.pos < len(p.src) && strings.IndexByte("*+?{", p.src[p.pos]) >= 0 {
		return false, fmt.Errorf("the %c at %d repeats nothing", p.src[p.pos], p.pos)
	}
	return true, nil
}

// bound reads `{n}`, `{n,}` or `{n,m}` at pos and answers n and m (-1 for none, unbounded), moving
// past it; it answers false, moving nothing, where pos holds none.
func (p *patternReader) bound() ([2]int, bool) {
	rest := p.src[p.pos:]
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return [2]int{}, false
	}
	low, high, comma := strings.Cut(rest[1:end], ",")
	n, err := strconv.Atoi(low)
	if err != nil || n < 0 || low != strconv.Itoa(n) {
		return [2]int{}, false
	}
	m := n
	if comma {
		m = -1
		if high != "" {
			if m, err = strconv.Atoi(high); err != nil || m < 0 || high != strconv.Itoa(m) {
				return [2]int{}, false
			}
		}
	}
	p.pos += end + 1
	return [2]int{n, m}, true
}
