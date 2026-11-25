package asof

import (
	"fmt"
	"strings"
)

// refusedStatementLength is how many characters of a refused statement RefusedError names.
const refusedStatementLength = 80

// RefusedError is a statement that Bind does not bind: a read at a cut writes nothing, and a
// statement that Bind cannot read as one read is not sent at all. The store returns it before
// it sends anything.
type RefusedError struct {
	// Statement is the first 80 characters of the statement, from its first word. Leading
	// whitespace and comments are not part of it.
	Statement string
	// Reason says which rule refused the statement.
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("asof: a read at a cut does not run %q: %s", e.Statement, e.Reason)
}

// Bind puts the shadows at cut (Shadows) of the tables that sql names in front of sql, so that
// each of those tables reads as it stood at the cut.
//
//   - A table is named by a word of sql in any case, or by a quoted identifier in the table's own
//     case. A column, an alias or a schema-qualified name (public.events) of the same name names
//     the table too: the statement then does not read that shadow, which costs only its parse. A
//     table that sql reads only inside a function or a view is not named, and reads as it is now.
//   - The shadows keep the order of the shadows list, whatever order sql names them in.
//   - A statement that names no shadowed table gets no WITH list: Bind returns it unchanged.
//   - Leading whitespace and comments stay where they are, in front of the WITH list.
//   - A statement with its own leading WITH or WITH RECURSIVE list gets the shadows as the first
//     members of that list, and RECURSIVE stays. Its own members can then read the shadows.
//   - A statement whose first word is set, show, savepoint, release or rollback reads and sets no
//     table. Bind returns it unchanged: a WITH list in front of it is a syntax error. Bind refuses
//     those of them that can take a read out of its read-only transaction (below).
//
// Bind refuses, with a *RefusedError, a statement whose first word after its WITH list is not
// select, values, table or "(", a statement whose own WITH list holds an insert, update, delete
// or merge, a string that holds more than one statement, whether or not it names a shadowed
// table, and a statement that holds a U&"..." identifier, whose escapes can spell the name of any
// table. The store runs each read at a cut in a read-only transaction. A statement that Bind
// passes unchanged can take a read out of that transaction, so Bind refuses these too:
//
//   - A set statement whose next word, after any session or local, is transaction or
//     characteristics. As a transaction's first statement, set transaction read write makes the
//     transaction read-write, and so does set session characteristics as transaction read write
//     for the session's next transactions. Bind refuses each of these forms, whatever modes it
//     names: a list of modes can hold read write after other modes.
//   - A set statement that names the parameter transaction_read_only or
//     default_transaction_read_only. PostgreSQL matches a parameter's name in any case, so Bind
//     matches a word or a quoted identifier in any case.
//   - A rollback that does not roll back to a savepoint. It ends the transaction, and the
//     statements after it run outside it. Only rollback [work | transaction] to [savepoint] name
//     passes.
//
// Other settings pass, such as set local statement_timeout = '15s', and so do savepoint and
// release. Keywords are case-insensitive. Bind reads sql with a tokenizer that reads a string
// literal, a quoted identifier and a dollar-quoted body each as one token and skips comments, so
// a word inside a literal, a body or a comment is not SQL to it and names no table. It reads them
// as PostgreSQL's lexer does where a table's name depends on it: a literal goes on after
// whitespace that holds a newline and a second quote, an E'...' literal with its backslash
// escapes in each part; a -- comment ends at a newline or a carriage return; and /* */ comments
// nest. It reads no setting: a backslash in a plain literal is a character, as under
// standard_conforming_strings on, the default.
func Bind(sql string, cut Cut) (string, error) {
	scan := scanner{sql: sql}
	first := scan.next()
	if first.kind == tokenEnd {
		return sql, nil
	}
	head := scan.head(first)
	if reason := scan.single(); reason != "" {
		return "", refuse(sql, first, reason)
	}
	if scan.unicodeIdentifier {
		return "", refuse(sql, first, `it holds a U&"..." identifier, whose escapes can spell the name of any table`)
	}
	if !head.with && head.main.kind == tokenWord && passes(sql[head.main.start:head.main.end]) {
		rest := scanner{sql: sql, pos: head.main.end}
		if reason := rest.leavesReadOnly(head.main); reason != "" {
			return "", refuse(sql, first, reason)
		}
		return sql, nil
	}
	if head.write != "" {
		return "", refuse(sql, first, "its WITH list holds a data-modifying "+head.write)
	}
	if !head.reads(sql) {
		return "", refuse(sql, first, head.notARead(sql))
	}
	if scan.named == ([len(shadows)]bool{}) {
		return sql, nil
	}
	list := shadowList(cut, &scan.named)
	if head.with {
		return sql[:head.headerEnd] + " " + list + "," + sql[head.headerEnd:], nil
	}
	return sql[:first.start] + "with " + list + "\n" + sql[first.start:], nil
}

// passes reports whether a statement that begins with word reads and sets no table: Bind
// returns it unchanged.
func passes(word string) bool {
	for _, keyword := range [...]string{"set", "show", "savepoint", "release", "rollback"} {
		if strings.EqualFold(word, keyword) {
			return true
		}
	}
	return false
}

// leavesReadOnly says how a statement that passes Bind unchanged (passes) can take a read at a
// cut out of its read-only transaction, or "" when it cannot. first is the statement's first
// word, and the scanner reads the tokens after it.
func (s *scanner) leavesReadOnly(first token) string {
	switch {
	case s.isWord(first, "set"):
		// set [ session | local ] { transaction mode, ... | session characteristics as
		// transaction mode, ... | parameter { to | = } value }
		t := s.next()
		for s.isWord(t, "session") || s.isWord(t, "local") {
			t = s.next()
		}
		if s.isWord(t, "transaction") || s.isWord(t, "characteristics") {
			return "it sets the modes of a transaction, and read write can make a read at a cut read-write"
		}
		for _, parameter := range [...]string{"transaction_read_only", "default_transaction_read_only"} {
			if s.isWord(t, parameter) || s.isQuoted(t, parameter) {
				return "it sets " + parameter + ", and off can make a read at a cut read-write"
			}
		}
	case s.isWord(first, "rollback"):
		// rollback [ work | transaction ] to [ savepoint ] name
		t := s.next()
		if s.isWord(t, "work") || s.isWord(t, "transaction") {
			t = s.next()
		}
		if !s.isWord(t, "to") {
			return "it ends the transaction rather than roll back to a savepoint, and the statements after it run outside the read-only transaction of the cut"
		}
	}
	return ""
}

// refuse is the refusal of sql for reason, naming the statement from its first token.
func refuse(sql string, first token, reason string) *RefusedError {
	statement := sql[first.start:]
	characters := 0
	for index := range statement {
		if characters == refusedStatementLength {
			statement = statement[:index]
			break
		}
		characters++
	}
	return &RefusedError{Statement: statement, Reason: reason}
}

// statementHead is what Bind reads from the front of a statement.
type statementHead struct {
	// with is whether the statement opens with its own WITH list, and headerEnd is where its
	// WITH or WITH RECURSIVE ends.
	with      bool
	headerEnd int
	// main is the first token after the WITH list, or the first token when there is none.
	main token
	// write is the keyword of a data-modifying member of the WITH list (insert, update, delete
	// or merge), or empty when the list holds none.
	write string
}

// reads reports whether the statement's first token after its WITH list begins a read.
func (h statementHead) reads(sql string) bool {
	switch h.main.kind {
	case tokenOpen:
		return true
	case tokenWord:
		word := sql[h.main.start:h.main.end]
		return strings.EqualFold(word, "select") || strings.EqualFold(word, "values") || strings.EqualFold(word, "table")
	}
	return false
}

// notARead says how a statement that does not begin a read begins.
func (h statementHead) notARead(sql string) string {
	word := "nothing"
	if h.main.kind != tokenEnd {
		word = strings.ToLower(sql[h.main.start:h.main.end])
	}
	if h.with {
		return "after its WITH list it goes on with " + word + ", not with select, values, table or ("
	}
	return "it begins with " + word + ", not with select, values, table or ("
}

// head reads a statement's WITH list, when it has one, from its first token, and leaves the
// scanner after the first token that follows the list. A list that does not follow
// PostgreSQL's grammar ends at the token where it stops following it: PostgreSQL refuses that
// statement in any case.
func (s *scanner) head(first token) statementHead {
	if !s.isWord(first, "with") {
		return statementHead{main: first}
	}
	head := statementHead{with: true, headerEnd: first.end}
	t := s.next()
	if s.isWord(t, "recursive") {
		head.headerEnd = t.end
		t = s.next()
	}
	for {
		// name [ ( column, ... ) ] as [ [ not ] materialized ] ( statement )
		if t.kind != tokenWord && t.kind != tokenQuoted {
			head.main = t
			return head
		}
		t = s.next()
		if t.kind == tokenOpen {
			s.skipTo(t.depth)
			t = s.next()
		}
		if !s.isWord(t, "as") {
			head.main = t
			return head
		}
		t = s.next()
		if s.isWord(t, "not") {
			t = s.next()
		}
		if s.isWord(t, "materialized") {
			t = s.next()
		}
		if t.kind != tokenOpen {
			head.main = t
			return head
		}
		member := s.head(s.next())
		if head.write == "" {
			head.write = member.write
		}
		if member.main.kind == tokenWord && head.write == "" {
			word := s.sql[member.main.start:member.main.end]
			for _, keyword := range [...]string{"insert", "update", "delete", "merge"} {
				if strings.EqualFold(word, keyword) {
					head.write = keyword
				}
			}
		}
		if member.main.kind != tokenClose || member.main.depth != t.depth {
			s.skipTo(t.depth)
		}
		t = s.next()
		// [ search { breadth | depth } first by column, ... set column ]
		if s.isWord(t, "search") {
			s.next() // breadth or depth
			s.next() // first
			s.next() // by
			s.columns()
			s.next() // the sequence column
			t = s.next()
		}
		// [ cycle column, ... set column [ to value default value ] using column ]
		if s.isWord(t, "cycle") {
			s.columns()
			s.next() // the mark column
			t = s.next()
			if s.isWord(t, "to") {
				for t.kind != tokenEnd && !s.isWord(t, "using") {
					t = s.next()
				}
			}
			s.next() // the path column
			t = s.next()
		}
		if t.kind != tokenComma {
			head.main = t
			return head
		}
		t = s.next()
	}
}

// columns reads a list of column names, one or more separated by commas, and the set that
// follows the list in a search or cycle clause.
func (s *scanner) columns() {
	s.next()
	for s.next().kind == tokenComma {
		s.next()
	}
}

// skipTo reads tokens until the ")" that closes the "(" opened at depth, or the end.
func (s *scanner) skipTo(depth int) {
	for {
		t := s.next()
		if t.kind == tokenEnd || (t.kind == tokenClose && t.depth == depth) {
			return
		}
	}
}

// single reads the rest of the statement and gives why it is more than one statement, or ""
// when it is one: a ";" followed by any token.
func (s *scanner) single() string {
	for t := s.next(); t.kind != tokenEnd; t = s.next() {
		if t.kind != tokenSemicolon {
			continue
		}
		t = s.next()
		for t.kind == tokenSemicolon {
			t = s.next()
		}
		if t.kind != tokenEnd {
			return "it holds more than one statement"
		}
		return ""
	}
	return ""
}

func (s *scanner) isWord(t token, keyword string) bool {
	return t.kind == tokenWord && strings.EqualFold(s.sql[t.start:t.end], keyword)
}

// isQuoted reports whether t is a quoted identifier that holds name in any case, as PostgreSQL
// matches the name of a parameter.
func (s *scanner) isQuoted(t token, name string) bool {
	return t.kind == tokenQuoted && t.end-t.start == len(name)+2 && s.sql[t.end-1] == '"' &&
		strings.EqualFold(s.sql[t.start+1:t.end-1], name)
}

type tokenKind uint8

const (
	tokenEnd tokenKind = iota
	// tokenWord is a keyword or an identifier that is not quoted.
	tokenWord
	// tokenQuoted is a quoted identifier.
	tokenQuoted
	// tokenString is a string literal or a dollar-quoted body.
	tokenString
	tokenOpen
	tokenClose
	tokenComma
	tokenSemicolon
	// tokenOther is a number, an operator, a parameter or other punctuation.
	tokenOther
)

// token is one token of a statement: sql[start:end]. depth is the depth of parentheses that
// holds the token; a "(" and the ")" that closes it have the same depth.
type token struct {
	kind       tokenKind
	start, end int
	depth      int
}

// scanner splits a statement into tokens. It knows of PostgreSQL's lexical structure only what
// Bind needs: where a token begins and ends, so that no word inside a literal, a quoted
// identifier, a dollar-quoted body or a comment is read as SQL.
type scanner struct {
	sql   string
	pos   int
	depth int
	// named marks each shadowed table that a word or a quoted identifier of the tokens read so
	// far names, by its place in shadows.
	named [len(shadows)]bool
	// unicodeIdentifier is whether the tokens read so far hold a U&"..." identifier.
	unicodeIdentifier bool
}

// mark marks the shadowed table that identifier names (shadowNamed), when it names one.
func (s *scanner) mark(identifier string, quoted bool) {
	if index := shadowNamed(identifier, quoted); index >= 0 {
		s.named[index] = true
	}
}

// next is the next token after whitespace and comments. At the end it gives a tokenEnd at
// len(sql).
func (s *scanner) next() token {
	s.skipSpaceAndComments()
	start := s.pos
	if start >= len(s.sql) {
		return token{kind: tokenEnd, start: start, end: start, depth: s.depth}
	}
	c := s.sql[start]
	kind := tokenOther
	depth := s.depth
	switch {
	case c == '(':
		s.pos++
		s.depth++
		kind = tokenOpen
	case c == ')':
		s.pos++
		if s.depth > 0 {
			s.depth--
		}
		depth = s.depth
		kind = tokenClose
	case c == ',':
		s.pos++
		kind = tokenComma
	case c == ';':
		s.pos++
		kind = tokenSemicolon
	case c == '\'':
		s.skipString(false)
		kind = tokenString
	case c == '"':
		s.skipQuoted()
		kind = tokenQuoted
		// An unterminated identifier runs to the end, and PostgreSQL refuses the statement.
		if s.pos-start >= 2 && s.sql[s.pos-1] == '"' {
			s.mark(s.sql[start+1:s.pos-1], true)
		}
	case c == '$':
		if tag := s.dollarTag(); tag != "" {
			s.skipDollarQuoted(tag)
			kind = tokenString
		} else {
			// A parameter ($1) or a stray dollar sign.
			s.pos++
			for s.pos < len(s.sql) && isDigit(s.sql[s.pos]) {
				s.pos++
			}
		}
	case isIdentifierStart(c):
		s.pos++
		for s.pos < len(s.sql) && isIdentifierPart(s.sql[s.pos]) {
			s.pos++
		}
		kind = tokenWord
		// E'...' is a string with backslash escapes. B'...', X'...', N'...' and U&'...' end
		// as a plain string does, so the next token reads them. U&"..." is an identifier whose
		// escapes Bind does not read (unicodeIdentifier).
		switch {
		case s.pos-start == 1 && (c == 'e' || c == 'E') && s.at(s.pos, '\''):
			s.skipString(true)
			kind = tokenString
		case s.pos-start == 1 && (c == 'u' || c == 'U') && s.at(s.pos, '&') && s.at(s.pos+1, '"'):
			s.unicodeIdentifier = true
		default:
			s.mark(s.sql[start:s.pos], false)
		}
	case isDigit(c):
		for s.pos < len(s.sql) && (isIdentifierPart(s.sql[s.pos]) || s.sql[s.pos] == '.') {
			s.pos++
		}
	default:
		s.pos++
	}
	return token{kind: kind, start: start, end: s.pos, depth: depth}
}

// skipSpaceAndComments moves past whitespace, -- comments and /* */ comments, which nest in
// PostgreSQL.
func (s *scanner) skipSpaceAndComments() {
	for s.pos < len(s.sql) {
		switch c := s.sql[s.pos]; {
		case isSpace(c):
			s.pos++
		case c == '-' && s.at(s.pos+1, '-'):
			s.pos = s.commentEnd(s.pos)
		case c == '/' && s.at(s.pos+1, '*'):
			s.pos += 2
			for nested := 1; nested > 0 && s.pos < len(s.sql); {
				switch {
				case s.sql[s.pos] == '/' && s.at(s.pos+1, '*'):
					nested++
					s.pos += 2
				case s.sql[s.pos] == '*' && s.at(s.pos+1, '/'):
					nested--
					s.pos += 2
				default:
					s.pos++
				}
			}
		default:
			return
		}
	}
}

// commentEnd is where the -- comment that begins at start ends: at the newline or the carriage
// return after it, which is not part of it, or at the end.
func (s *scanner) commentEnd(start int) int {
	end := strings.IndexAny(s.sql[start+2:], "\n\r")
	if end < 0 {
		return len(s.sql)
	}
	return start + 2 + end
}

// skipString moves past a string literal whose opening quote is at the scanner's position. A
// doubled quote is a quote inside the string; with escapes (an E'...' string), a backslash takes
// the next byte too. The literal goes on where its quote is followed by a quote after whitespace
// that holds a newline (continuedAt), with escapes as before. An unterminated string runs to the
// end.
func (s *scanner) skipString(escapes bool) {
	s.pos++
	for s.pos < len(s.sql) {
		switch s.sql[s.pos] {
		case '\\':
			if escapes {
				s.pos++
			}
		case '\'':
			if !s.at(s.pos+1, '\'') {
				next := s.continuedAt(s.pos + 1)
				if next < 0 {
					s.pos++
					return
				}
				s.pos = next
			} else {
				s.pos++
			}
		}
		s.pos++
	}
	s.pos = len(s.sql)
}

// continuedAt is the place of the quote that goes on with a string literal whose part ended
// before from, or -1 where none does: scan.l's {quotecontinue}, whitespace that holds a newline,
// then a quote. Before the newline the whitespace holds no other newline and may hold a --
// comment; after it, any whitespace and -- comments that a newline ends.
func (s *scanner) continuedAt(from int) int {
	at := from
	for at < len(s.sql) {
		if c := s.sql[at]; c == ' ' || c == '\t' || c == '\f' || c == '\v' {
			at++
		} else if c == '-' && s.at(at+1, '-') {
			at = s.commentEnd(at)
		} else {
			break
		}
	}
	if at >= len(s.sql) || !isNewline(s.sql[at]) {
		return -1
	}
	for at++; at < len(s.sql); {
		c := s.sql[at]
		if isSpace(c) {
			at++
			continue
		}
		if c == '-' && s.at(at+1, '-') {
			if end := s.commentEnd(at); end < len(s.sql) {
				at = end + 1
				continue
			}
		}
		break
	}
	if !s.at(at, '\'') {
		return -1
	}
	return at
}

// skipQuoted moves past a quoted identifier; a doubled quote is a quote inside it.
func (s *scanner) skipQuoted() {
	s.pos++
	for s.pos < len(s.sql) {
		if s.sql[s.pos] == '"' {
			if !s.at(s.pos+1, '"') {
				s.pos++
				return
			}
			s.pos++
		}
		s.pos++
	}
}

// dollarTag is the opening tag of a dollar-quoted body at the scanner's position ($$ or
// $name$), or "" when the dollar sign opens none.
func (s *scanner) dollarTag() string {
	end := s.pos + 1
	if end < len(s.sql) && isIdentifierStart(s.sql[end]) {
		for end < len(s.sql) && isIdentifierPart(s.sql[end]) && s.sql[end] != '$' {
			end++
		}
	}
	if !s.at(end, '$') {
		return ""
	}
	return s.sql[s.pos : end+1]
}

// skipDollarQuoted moves past a dollar-quoted body that tag opens, to the end of the same tag. An
// unterminated body runs to the end.
func (s *scanner) skipDollarQuoted(tag string) {
	s.pos += len(tag)
	end := strings.Index(s.sql[s.pos:], tag)
	if end < 0 {
		s.pos = len(s.sql)
		return
	}
	s.pos += end + len(tag)
}

func (s *scanner) at(index int, c byte) bool {
	return index < len(s.sql) && s.sql[index] == c
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isSpace reports whether c is whitespace to PostgreSQL (scan.l's {space}).
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// isNewline reports whether c ends a line to PostgreSQL (scan.l's {newline}).
func isNewline(c byte) bool { return c == '\n' || c == '\r' }

// isIdentifierStart reports whether c begins an identifier or a keyword: a letter, an
// underscore, or a byte of a non-ASCII character.
func isIdentifierStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

// isIdentifierPart reports whether c continues an identifier or a keyword.
func isIdentifierPart(c byte) bool {
	return isIdentifierStart(c) || isDigit(c) || c == '$'
}
