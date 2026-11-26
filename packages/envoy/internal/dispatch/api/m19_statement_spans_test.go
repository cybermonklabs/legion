package api

// The scanner of the statement-binding rule (m19_statement_binding_test.go): where each SQL
// fragment of one statement begins and ends, read as Postgres's scanner reads it, so a marker
// inside a string, a quoted name or a dollar-quoted body is no marker.

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// m19Span is one SQL fragment of a statement: where its comment begins, where the expression it
// opens ends, the name it carries, and whether a row whose own column it reads is bound by it.
type m19Span struct {
	start, end int
	name       string
	binds      bool
}

// m19Spans reads every fragment of one statement, as Postgres's scanner reads it: a marker inside
// a string, a quoted name or a dollar-quoted body is no marker. A fragment runs from its comment
// to the end of what follows it: a parenthesised group, a call, or the rest of the group it
// stands in, which is the whole statement for the read that resolves the viewer.
func m19Spans(sql string) ([]m19Span, error) {
	var spans []m19Span
	for index := 0; index < len(sql); {
		after := m19Skip(sql, index)
		if !strings.HasPrefix(sql[index:], "/*") {
			index = after
			continue
		}
		comment := sql[index:after]
		span := m19Span{start: index}
		switch {
		case strings.HasPrefix(comment, access.Marker):
			span.name = strings.TrimSuffix(strings.TrimPrefix(comment, access.Marker), " */")
			binds, named := m19BindingFragments[span.name]
			if !named {
				return nil, fmt.Errorf("the statement binds %q, a fragment the census does not know: name it in m19BindingFragments, with whether a row whose column it reads is bound by it", span.name)
			}
			span.binds = binds
		case strings.HasPrefix(comment, refs.VisibilityMarker):
			// Every predicate the registry renders answers who may see the node it is over.
			span.name, span.binds = strings.TrimSuffix(strings.TrimPrefix(comment, refs.VisibilityMarker), " */"), true
		default:
			index = after
			continue
		}
		span.end = m19FragmentEnd(sql, after)
		spans = append(spans, span)
		index = after
	}
	return spans, nil
}

// m19FragmentEnd is where the expression a fragment's comment opens, which starts at index, ends.
func m19FragmentEnd(sql string, index int) int {
	open := m19SkipSpace(sql, index)
	if open < len(sql) && sql[open] == '(' {
		return m19CloseParen(sql, open)
	}
	word := open
	for word < len(sql) && m19IsNameByte(sql[word]) {
		word++
	}
	if call := m19SkipSpace(sql, word); word > open && call < len(sql) && sql[call] == '(' {
		if name := strings.ToLower(sql[open:word]); name != "with" && name != "select" && name != "case" {
			return m19CloseParen(sql, call)
		}
	}
	// A fragment that is a statement of its own: to the end of the group it stands in.
	return m19EnclosingEnd(sql, open)
}

// m19CloseParen is the index after the parenthesis that closes the one at open.
func m19CloseParen(sql string, open int) int {
	for index, depth := open, 0; index < len(sql); {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return index + 1
			}
		}
		index = m19Skip(sql, index)
	}
	return len(sql)
}

// m19EnclosingEnd is where the group index stands in ends: its first unmatched parenthesis, or
// the end of the statement.
func m19EnclosingEnd(sql string, index int) int {
	for depth := 0; index < len(sql); {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return index
			}
		}
		index = m19Skip(sql, index)
	}
	return len(sql)
}

// m19Skip is the index after the lexeme of sql at index, as Postgres's scanner reads it: a
// string, a quoted name, a dollar-quoted body and a comment are each one lexeme, whatever they
// hold.
func m19Skip(sql string, index int) int {
	switch {
	case strings.HasPrefix(sql[index:], "--"):
		if end := strings.IndexByte(sql[index:], '\n'); end >= 0 {
			return index + end + 1
		}
		return len(sql)
	case strings.HasPrefix(sql[index:], "/*"):
		for at, depth := index, 0; at < len(sql); {
			switch {
			case strings.HasPrefix(sql[at:], "/*"):
				depth, at = depth+1, at+2
			case strings.HasPrefix(sql[at:], "*/"):
				if depth, at = depth-1, at+2; depth == 0 {
					return at
				}
			default:
				at++
			}
		}
		return len(sql)
	case sql[index] == '\'' || sql[index] == '"':
		quote := sql[index]
		for at := index + 1; at < len(sql); at++ {
			if sql[at] != quote {
				continue
			}
			if at+1 < len(sql) && sql[at+1] == quote {
				at++
				continue
			}
			return at + 1
		}
		return len(sql)
	case sql[index] == '$':
		tag := m19DollarTag(sql, index)
		if tag == "" {
			return index + 1
		}
		if end := strings.Index(sql[index+len(tag):], tag); end >= 0 {
			return index + len(tag) + end + len(tag)
		}
		return len(sql)
	}
	return index + 1
}

// m19DollarTag is the tag of the dollar quote opening at index ("$$", "$body$"), empty where the
// dollar opens no quote: a parameter ($1) or a dollar of its own.
func m19DollarTag(sql string, index int) string {
	for at := index + 1; at < len(sql); at++ {
		if sql[at] == '$' {
			return sql[index : at+1]
		}
		if !m19IsNameByte(sql[at]) || (sql[at] >= '0' && sql[at] <= '9' && at == index+1) {
			return ""
		}
	}
	return ""
}

func m19IsNameByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b|0x20 >= 'a' && b|0x20 <= 'z')
}

func m19SkipSpace(sql string, index int) int {
	for index < len(sql) && (sql[index] == ' ' || sql[index] == '\t' || sql[index] == '\n' || sql[index] == '\r') {
		index++
	}
	return index
}
