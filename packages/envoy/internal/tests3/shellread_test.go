package tests3

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The shell reader of the waits program (TestEveryWaitAndCheckOfTheScriptHasACase, waits_test.go).
// It parses one-collection.sh, and the shell each store step runs (its run: text, or the script
// that text names), as POSIX shell with mvdan.cc/sh's parser, and answers what the parse holds in
// the form the program's rules read:
//
//   - every simple command the file runs, wherever the shell runs it: at the top, in a function,
//     in a condition, a loop, a case or a block, inside a command substitution at any depth
//     (`$(...)` or backticks, quoted or not, inside arithmetic, a parameter's default, an
//     assignment, a redirection or a here-document), the command of a trap, and the command that
//     timeout and the script's own within run. Each comes with its name and its words as the
//     shell resolves them, and with where it runs;
//   - every until, while and for, with the statement before it where it stands in a list of
//     statements of its own, and whether it stands inside another until or while;
//   - every assignment: name=value, and those a for's variable and a parameter's ${x=...} or
//     ${x:=...} make (an assignment arithmetic makes is in an arithmetic expansion, which the
//     rules refuse anywhere but in a wait's deadline statement);
//   - every arithmetic expansion, wherever it stands;
//   - the file's own functions, each definition of them, and which of them each runs.
//
// A word holding an expansion of any kind (a variable, a substitution, arithmetic, a leading ~,
// the $'...' that busybox sh expands as bash does, or the $"..." that bash expands) or a file-name
// pattern is told apart from a literal word, so no rule reads it as text it is not: its value is
// one this program cannot know. What the reader cannot read it answers as refused, by line: a
// file that does not parse, a kind of shell other than those these files use, and a trap whose
// command is not plain text.

// A shellWord is one word of a command.
type shellWord struct {
	// text is the word as the shell gives it, with its quotes and escapes taken out. Where the
	// word is not literal, each expansion in it stands as written ($collection, $(...)).
	text string
	// source is the word as written.
	source string
	// literal: the word holds no expansion of any kind and no file-name pattern, so text is all
	// of it.
	literal bool
	// single: the shell gives it as one word, never split or matched to file names: a literal
	// word, or one whose every expansion stands inside double quotes, "$@" excepted.
	single bool
	word   *syntax.Word
}

// is answers whether the word is the literal text.
func (w shellWord) is(text string) bool { return w.literal && w.text == text }

// form answers the word's text where it is literal, and its source where it is not.
func (w shellWord) form() string {
	if w.literal {
		return w.text
	}
	return w.source
}

// A shellCommand is one simple command a file runs.
type shellCommand struct {
	// line is its line in the file, and offset where its name stands; the command of a trap has
	// the trap's line and an offset of -1.
	line, offset int
	// function is the file's own function it runs in: "" at the top and in a trap.
	function string
	// name is its first word; a literal name that holds a / is its base name.
	name shellWord
	args []shellWord
	// acted: the shell acts on its failure, so under set -e that failure does not end the script:
	// it is a condition of an if, until or while, a side of && or ||, the left of a pipe, negated,
	// within's command, or inside a substitution in another command's words.
	acted bool
	// timed: it is the command a timeout runs. waiting: it runs in an until or a while.
	timed, waiting bool
	background     bool
}

// A shellLoop is one until, while or for a file runs.
type shellLoop struct {
	line     int
	function string
	kind     string // "until", "while" or "for"
	// before is the statement before it, where it is a statement of its own in a list of
	// statements and not the first; nil otherwise.
	before *syntax.Stmt
	body   []*syntax.Stmt
	// nested: it stands inside another until or while, in its condition or its body.
	nested bool
}

// A shellAssign is one assignment a file makes.
type shellAssign struct {
	line     int
	function string
	name     string
	// node is the assignment where it is name=value, and nil for any other.
	node *syntax.Assign
}

// A shellRefusal is a part of a file the reader cannot read.
type shellRefusal struct {
	line int
	what string
}

// A shellArithmetic is one arithmetic expansion, $((...)), a file holds.
type shellArithmetic struct {
	line int
	node *syntax.ArithmExp
}

// A shellDefinition is one definition of an own function, with its line.
type shellDefinition struct {
	line int
	name string
}

// A shellRead is what the reader found in one file.
type shellRead struct {
	// functions holds each own function by its name, at its last definition, and definitions
	// every definition the reader reads, in the order it reads them.
	functions   map[string]*syntax.FuncDecl
	definitions []shellDefinition
	// calls holds, for each own function, the own functions it runs.
	calls      map[string][]string
	commands   []shellCommand
	loops      []shellLoop
	assigns    []shellAssign
	arithmetic []shellArithmetic
	refused    []shellRefusal
}

// shellScope is where the reader stands: the function, and what the shell does with a failure.
type shellScope struct {
	function              string
	acted, timed, waiting bool
	// trap is the line of the trap whose command is being read, whose own positions are not the
	// file's; 0 elsewhere.
	trap int
}

func (s shellScope) line(n syntax.Node) int {
	if s.trap != 0 {
		return s.trap
	}
	return int(n.Pos().Line())
}

func (s shellScope) offset(n syntax.Node) int {
	if s.trap != 0 {
		return -1
	}
	return int(n.Pos().Offset())
}

func parseShell(text string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(text), "")
}

// readShell reads text, one-collection.sh or the shell a store step runs.
func readShell(text string) *shellRead {
	read := &shellRead{functions: map[string]*syntax.FuncDecl{}, calls: map[string][]string{}}
	file, err := parseShell(text)
	if err != nil {
		read.refused = append(read.refused, shellRefusal{errorLine(err), "does not parse as POSIX shell, so this program cannot read it (" + err.Error() + ")"})
		return read
	}
	// The own functions first, so a call that comes before its function's text is read as one.
	syntax.Walk(file, func(n syntax.Node) bool {
		if f, ok := n.(*syntax.FuncDecl); ok && f.Name != nil {
			read.functions[f.Name.Value] = f
		}
		return true
	})
	read.list(file.Stmts, shellScope{})
	return read
}

func errorLine(err error) int {
	var parse syntax.ParseError
	var lang syntax.LangError
	switch {
	case errors.As(err, &parse):
		return int(parse.Pos.Line())
	case errors.As(err, &lang):
		return int(lang.Pos.Line())
	}
	return 0
}

func (r *shellRead) refuse(line int, what string) {
	r.refused = append(r.refused, shellRefusal{line, what})
}

func (r *shellRead) list(stmts []*syntax.Stmt, s shellScope) {
	for i, stmt := range stmts {
		var before *syntax.Stmt
		if i > 0 {
			before = stmts[i-1]
		}
		r.stmt(stmt, s, before)
	}
}

// stmt reads one statement; before is the statement before it in its list, or nil where it is
// the first or stands in no list of its own.
func (r *shellRead) stmt(stmt *syntax.Stmt, s shellScope, before *syntax.Stmt) {
	if stmt.Negated {
		s.acted = true
	}
	for _, redirect := range stmt.Redirs {
		r.words(redirect, s.substitution())
	}
	switch c := stmt.Cmd.(type) {
	case nil:
		// redirections alone
	case *syntax.CallExpr:
		r.call(c, s, stmt.Background)
	case *syntax.BinaryCmd:
		x, y := s, s
		switch c.Op {
		case syntax.AndStmt, syntax.OrStmt:
			x.acted, y.acted = true, true
		case syntax.Pipe:
			x.acted = true
		default:
			r.refuse(s.line(c), "runs a "+c.Op.String()+", a kind of shell this program does not read")
			return
		}
		r.stmt(c.X, x, nil)
		r.stmt(c.Y, y, nil)
	case *syntax.IfClause:
		for clause := c; clause != nil; clause = clause.Else {
			cond := s
			cond.acted = true
			r.list(clause.Cond, cond)
			r.list(clause.Then, s)
		}
	case *syntax.WhileClause:
		kind := "while"
		if c.Until {
			kind = "until"
		}
		r.loops = append(r.loops, shellLoop{s.line(c), s.function, kind, before, c.Do, s.waiting})
		inner := s
		inner.waiting = true
		cond := inner
		cond.acted = true
		r.list(c.Cond, cond)
		r.list(c.Do, inner)
	case *syntax.ForClause:
		iter, ok := c.Loop.(*syntax.WordIter)
		if !ok {
			r.refuse(s.line(c), "runs a C-style for, a kind of shell this program does not read")
			return
		}
		r.loops = append(r.loops, shellLoop{s.line(c), s.function, "for", before, c.Do, s.waiting})
		r.assigns = append(r.assigns, shellAssign{s.line(c), s.function, iter.Name.Value, nil})
		for _, item := range iter.Items {
			r.words(item, s.substitution())
		}
		r.list(c.Do, s)
	case *syntax.CaseClause:
		r.words(c.Word, s.substitution())
		for _, item := range c.Items {
			for _, pattern := range item.Patterns {
				r.words(pattern, s.substitution())
			}
			r.list(item.Stmts, s)
		}
	case *syntax.Block:
		r.list(c.Stmts, s)
	case *syntax.FuncDecl:
		r.definitions = append(r.definitions, shellDefinition{s.line(c), c.Name.Value})
		r.stmt(c.Body, shellScope{function: c.Name.Value, trap: s.trap}, nil)
	default:
		r.refuse(s.line(c), "runs a "+strings.TrimPrefix(fmt.Sprintf("%T", c), "*syntax.")+", a kind of shell this program does not read")
	}
}

// substitution answers the scope of a substitution in a command's words, whose failure the
// shell does not act on as the command's own.
func (s shellScope) substitution() shellScope {
	s.acted = true
	return s
}

func (r *shellRead) call(c *syntax.CallExpr, s shellScope, background bool) {
	// The status of an assignment alone is its substitutions' last, which set -e acts on; with a
	// command after it, the command's is.
	assigned := s
	if len(c.Args) > 0 {
		assigned.acted = true
	}
	for _, a := range c.Assigns {
		r.words(a, assigned)
		if a.Name != nil {
			r.assigns = append(r.assigns, shellAssign{s.line(a), s.function, a.Name.Value, a})
		}
	}
	for _, w := range c.Args {
		r.words(w, s.substitution())
	}
	if len(c.Args) > 0 {
		r.command(c.Args, s, background)
	}
}

// command reads the command words give, and the command it runs in turn, if any: timeout's
// command, within's (the script's own function) and a trap's.
func (r *shellRead) command(words []*syntax.Word, s shellScope, background bool) {
	name := readWord(words[0])
	if name.literal && strings.Contains(name.text, "/") {
		name.text = path.Base(name.text)
	}
	args := make([]shellWord, 0, len(words)-1)
	for _, w := range words[1:] {
		args = append(args, readWord(w))
	}
	r.commands = append(r.commands, shellCommand{
		line: s.line(words[0]), offset: s.offset(words[0]), function: s.function, name: name, args: args,
		acted: s.acted, timed: s.timed, waiting: s.waiting, background: background,
	})
	if !name.literal {
		return
	}
	if r.functions[name.text] != nil && s.function != "" {
		r.calls[s.function] = append(r.calls[s.function], name.text)
	}
	switch {
	case name.text == "timeout":
		// timeout [-s <signal>] <seconds> <command>...
		rest := words[1:]
		if len(rest) > 1 && readWord(rest[0]).is("-s") {
			rest = rest[2:]
		}
		if len(rest) > 1 {
			inner := s
			inner.timed = true
			r.command(rest[1:], inner, false)
		}
	case name.text == "within" && r.functions["within"] != nil && len(words) > 4:
		// within <seconds> <message> <file> <command>...: the command runs as within's until's
		// condition, in the function that calls within.
		inner := s
		inner.acted = true
		r.command(words[4:], inner, false)
	case name.text == "trap":
		r.trap(words[1:], s)
	}
}

// trap reads the command a trap sets (trap [--] <command> <condition>...), which the shell runs
// later, at the top of the script, so it is read as code at the top.
func (r *shellRead) trap(words []*syntax.Word, s shellScope) {
	if len(words) > 0 && readWord(words[0]).is("--") {
		words = words[1:]
	}
	if len(words) < 2 {
		return // trap alone lists the traps; trap <n> resets one
	}
	line := s.line(words[0])
	action := readWord(words[0])
	if !action.literal {
		r.refuse(line, "a trap whose command is not plain text, so this program cannot read the command the shell runs")
		return
	}
	if action.text == "-" {
		return // resets the traps it names
	}
	file, err := parseShell(action.text)
	if err != nil {
		r.refuse(line, "a trap whose command does not parse as POSIX shell ("+err.Error()+")")
		return
	}
	r.list(file.Stmts, shellScope{trap: line})
}

// words reads the code that a word, an assignment or a redirection holds: each command
// substitution in it, wherever it stands (inside quotes, arithmetic, a parameter's default or a
// here-document), is read as code, each arithmetic expansion is one of the file's, and each
// assignment a parameter makes in it is an assignment of the file's.
func (r *shellRead) words(node syntax.Node, s shellScope) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			r.list(n.Stmts, s)
			return false
		case *syntax.ArithmExp:
			r.arithmetic = append(r.arithmetic, shellArithmetic{s.line(n), n})
		case *syntax.ParamExp:
			if n.Param != nil && n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) {
				r.assigns = append(r.assigns, shellAssign{s.line(n), s.function, n.Param.Value, nil})
			}
		case *syntax.ProcSubst, *syntax.ExtGlob, *syntax.BraceExp:
			r.refuse(s.line(n), "holds a "+strings.TrimPrefix(fmt.Sprintf("%T", n), "*syntax.")+", a kind of shell this program does not read")
			return false
		}
		return true
	})
}

// readWord answers w as a rule reads it.
func readWord(w *syntax.Word) shellWord {
	out := shellWord{source: printed(w), literal: true, single: true, word: w}
	var text strings.Builder
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			// An unquoted *, ? or [ makes a pattern the shell matches to file names, but [ alone,
			// which is the test command.
			if p.Value != "[" && strings.ContainsAny(p.Value, "*?[") {
				out.literal, out.single = false, false
			}
			// A leading ~ is the value of $HOME or of a user's home; a $ right before a quote
			// makes $'...', which busybox sh expands as bash does ($'\055f' is -f), or $"...",
			// which bash expands, where this POSIX parse reads a $ and a quoted text (it sets no
			// SglQuoted.Dollar).
			if i == 0 && strings.HasPrefix(p.Value, "~") || i+1 < len(w.Parts) && dollarBeforeQuote(p.Value, w.Parts[i+1]) {
				out.literal = false
			}
			text.WriteString(unescape(p.Value, false))
		case *syntax.SglQuoted:
			text.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					text.WriteString(unescape(lit.Value, true))
					continue
				}
				out.literal = false
				if param, ok := inner.(*syntax.ParamExp); ok && param.Param != nil && param.Param.Value == "@" {
					out.single = false
				}
				text.WriteString(printed(inner))
			}
		default:
			out.literal, out.single = false, false
			text.WriteString(printed(p))
		}
	}
	out.text = text.String()
	return out
}

// unescape takes out the backslashes of a literal's escapes: every one outside quotes, and inside
// double quotes those before $, `, ", \ and a newline.
func unescape(value string, quoted bool) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) && (!quoted || strings.IndexByte("$`\"\\\n", value[i+1]) >= 0) {
			i++
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// dollarBeforeQuote answers whether value, an unquoted literal, ends with a $ that no backslash
// escapes, and next, the part after it, is a quoted text: $'...' or $"...".
func dollarBeforeQuote(value string, next syntax.WordPart) bool {
	switch next.(type) {
	case *syntax.SglQuoted, *syntax.DblQuoted:
	default:
		return false
	}
	if !strings.HasSuffix(value, "$") {
		return false
	}
	escapes := len(value) - 1 - len(strings.TrimRight(value[:len(value)-1], `\`))
	return escapes%2 == 0
}

// printed answers a node as the shell's source text.
func printed(n syntax.Node) string {
	var b bytes.Buffer
	if err := syntax.NewPrinter().Print(&b, n); err != nil {
		return fmt.Sprintf("<%T>", n)
	}
	return b.String()
}

// plainVariable answers whether p is $name or ${name} and no more.
func plainVariable(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// selfRunning answers each own function that runs itself again, at once or through others, with
// the way it does (a → b → a).
func (r *shellRead) selfRunning() map[string]string {
	found := map[string]string{}
	for name := range r.functions {
		if way := r.way(name, name); way != "" {
			found[name] = way
		}
	}
	return found
}

// way answers how own function from runs own function to, at once or through others
// (from → b → to), or "" when it does not.
func (r *shellRead) way(from, to string) string {
	way := map[string]string{from: from}
	queue := []string{from}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range r.calls[at] {
			if next == to {
				return way[at] + " → " + to
			}
			if _, seen := way[next]; !seen {
				way[next] = way[at] + " → " + next
				queue = append(queue, next)
			}
		}
	}
	return ""
}

// callIs answers whether stmt is a simple command whose words are want, each word read as its
// literal text or, where it is not literal, as written.
func callIs(stmt *syntax.Stmt, want ...string) bool {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Negated || stmt.Background || len(stmt.Redirs) > 0 || len(call.Assigns) > 0 || len(call.Args) != len(want) {
		return false
	}
	for i, w := range call.Args {
		if readWord(w).form() != want[i] {
			return false
		}
	}
	return true
}
