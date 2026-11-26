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

// SignalProvider is a project's model provider as the two signals of admission by signals that
// read it are configured: the headroom, the uncached input tokens of a minute over the project's
// own limit, and the refusals, each classed by who refused it. Each field is optional, and its
// zero value is the key unset.
//
// Every pattern is in the subset SharedPattern holds: the hosts class a refusal by its text
// (the Claude Code bridge with Go's regexp, the Oh My Pi plugin with JavaScript's RegExp), the
// pattern of the project's own limit first and the provider's second, and a refusal that matches
// neither is unknown. A deployment's own limit and its provider can both answer 429 with no header
// that tells them apart, which is why the text decides; each deployment writes its own patterns,
// and Legion names none.
type SignalProvider struct {
	// TokenLimit is `signal_token_limit_per_minute`, the uncached input tokens the project may
	// spend in a minute, and 0 where the file names none: the headroom then reads as not
	// recorded, never as 0.
	TokenLimit int64
	// TokenModels is `signal_token_model_pattern`, the models whose tokens count against the
	// limit; "" where the file names none, under which every model's tokens count. A request of
	// another model is kept all the same, and its refusal counts, but its tokens count nowhere.
	TokenModels string
	// OwnRefusal is `signal_own_refusal_pattern`, the text of a refusal by the project's own
	// limit, and ProviderRefusal `signal_provider_refusal_pattern`, the text of a refusal by the
	// provider; "" where the file names it none, which tells the hosts no pattern. The two reach
	// every host that runs a session of the project in its launch environment, as
	// runtime.OwnRefusalVariable and runtime.ProviderRefusalVariable, and no other way.
	OwnRefusal, ProviderRefusal string
}

// readSignalProvider reads a project's four provider keys out of fields, its members, under key,
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
	for _, pattern := range []struct {
		name string
		into *string
	}{
		{"signal_token_model_pattern", &provider.TokenModels},
		{"signal_own_refusal_pattern", &provider.OwnRefusal},
		{"signal_provider_refusal_pattern", &provider.ProviderRefusal},
	} {
		if value := fields[pattern.name]; value != nil {
			read, err := readSharedPattern(value, key+"."+pattern.name)
			if err != nil {
				return SignalProvider{}, err
			}
			*pattern.into = read
		}
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
//     with n ≤ m ≤ 1000, optionally followed by `?`.
//
// Each construct left out is one of the two engines refusing it — a lookaround, a back
// reference, a named group, an inline flag, `\p` — or one they read differently: `\s` is ASCII's
// spaces in Go and Unicode's in JavaScript, `[[:digit:]]` a POSIX class in Go and members in
// JavaScript, `\A` and `\Q` Go's alone, `[]` a class's first member in Go and an empty class in
// JavaScript, and a lone `{` or `]` a literal in one reading and a refusal in the other. An empty
// pattern matches every text, which would class every refusal as the pattern's; a key with no
// pattern is the key left out.
func checkSharedPattern(pattern string) error {
	if pattern == "" {
		return errors.New("it is empty, and an empty pattern matches every text")
	}
	p := patternReader{src: pattern}
	if err := p.alternation(); err != nil {
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
// closes the group it is read inside.
func (p *patternReader) alternation() error {
	for {
		if err := p.sequence(); err != nil {
			return err
		}
		if p.pos >= len(p.src) || p.src[p.pos] != '|' {
			return nil
		}
		p.pos++
	}
}

// sequence reads atoms, each with its repetition, up to `|`, `)` or the end.
func (p *patternReader) sequence() error {
	for p.pos < len(p.src) {
		at := p.pos
		repeatable := true
		switch c := p.src[p.pos]; c {
		case '|':
			return nil
		case ')':
			if p.depth == 0 {
				return fmt.Errorf("the ) at %d closes no group", at)
			}
			return nil
		case '(':
			if err := p.group(); err != nil {
				return err
			}
		case '[':
			if err := p.class(); err != nil {
				return err
			}
		case '^', '$':
			p.pos++
			repeatable = false
		case '.':
			p.pos++
		case '\\':
			assertion, err := p.escape()
			if err != nil {
				return err
			}
			repeatable = !assertion
		case '*', '+', '?':
			return fmt.Errorf("the %c at %d repeats nothing", c, at)
		case '{':
			if _, ok := p.bound(); ok {
				return fmt.Errorf("the repetition at %d repeats nothing", at)
			}
			return fmt.Errorf("the { at %d begins no repetition; write \\{ for the character", at)
		case '}':
			return fmt.Errorf("the } at %d closes no repetition; write \\} for the character", at)
		case ']':
			return fmt.Errorf("the ] at %d closes no class; write \\] for the character", at)
		default:
			p.pos++
		}
		if err := p.repetition(repeatable, at); err != nil {
			return err
		}
	}
	return nil
}

// group reads `(…)` or `(?:…)`.
func (p *patternReader) group() error {
	open := p.pos
	p.pos++
	if strings.HasPrefix(p.src[p.pos:], "?") {
		if !strings.HasPrefix(p.src[p.pos:], "?:") {
			return fmt.Errorf("(? at %d opens a lookaround, a named group or a flag; only (?: is in the subset", open)
		}
		p.pos += 2
	}
	p.depth++
	if err := p.alternation(); err != nil {
		return err
	}
	p.depth--
	if p.pos >= len(p.src) {
		return fmt.Errorf("a group opened at %d is never closed", open)
	}
	p.pos++
	return nil
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
		if !single || p.pos+1 >= len(p.src) || p.src[p.pos] != '-' || p.src[p.pos+1] == ']' {
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
// repeatable atom may carry, and its `?`.
func (p *patternReader) repetition(repeatable bool, atom int) error {
	if p.pos >= len(p.src) {
		return nil
	}
	at := p.pos
	switch p.src[at] {
	case '*', '+', '?':
		p.pos++
	case '{':
		counts, ok := p.bound()
		if !ok {
			return fmt.Errorf("the { at %d begins no repetition; write \\{ for the character", at)
		}
		if counts[0] > maxRepeat || counts[1] > maxRepeat || counts[1] >= 0 && counts[1] < counts[0] {
			return fmt.Errorf("the repetition at %d is outside 0 to %d, the bound Go's regexp takes", at, maxRepeat)
		}
	default:
		return nil
	}
	if !repeatable {
		return fmt.Errorf("the %c at %d repeats an assertion, which matches no character", p.src[at], at)
	}
	if p.pos < len(p.src) && p.src[p.pos] == '?' {
		p.pos++
	}
	if p.pos < len(p.src) && strings.IndexByte("*+?{", p.src[p.pos]) >= 0 {
		return fmt.Errorf("the %c at %d repeats nothing", p.src[p.pos], p.pos)
	}
	return nil
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
