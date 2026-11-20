package tests3

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The rules that TestEveryWaitAndCheckOfTheScriptHasACase (waits_test.go) holds both of the
// shells it reads to: one-collection.sh, through scriptFindings (waits_test.go), and the shell
// of the store steps and of the script the wait step runs, through stepFindings
// (storesteps_test.go). Each of those reads its file with the reader of shellread_test.go and
// applies these beside rules of its own: a script's head (scriptHeadFindings); its own functions,
// assignments, arithmetic and waits (ownFindings, with ruledCommand and deadlineStatement); and
// how a function takes its arguments (takesArguments, changesArguments, setsArguments).

var (
	wholeSeconds = regexp.MustCompile(`^[1-9][0-9]*$`)
	digits       = regexp.MustCompile(`^[0-9]+$`)
)

// ruledCommand answers whether name is a command these rules read by its name: one of
// scriptCommands, stepCommands or storeRequests, or kill, test or wait. An own function of the
// script or of the steps may not take such a name, since the shell then runs it in that
// command's place: a date of the file's own that answers 0 keeps a wait's deadline from passing.
func ruledCommand(name string) bool {
	return scriptCommands[name] || stepCommands[name] || storeRequests[name] || name == "kill" || name == "test" || name == "wait"
}

// ownFindings answers, by line, what read's own functions, assignments, arithmetic and waits
// break: a function that takes the name of a command these rules read (ruledCommand), or that is
// defined more than once, so the body the rules read is not the one every call runs; an
// assignment to deadline other than a wait's deadline statement,
// deadline=$(( $(date +%s) + <seconds> )) alone right before its until, whose body reads it, so
// the wait's end moves (seconds answers whether a word may give those seconds in this file);
// arithmetic anywhere but in such a statement, since busybox sh reads a variable's value in
// arithmetic as an expression and every shell reads the text of $x there, so either can assign
// to deadline through a value; a wait inside another wait, which would set the deadline that
// wait reads again on every pass; and an assignment to one of names, the words function takes
// off its arguments (within's in the script, s3's in the steps), anywhere but in function's own
// first statement: a later one, even of the same text, takes a word that is not the call's, so no
// call's seconds say its bound, nor its message the message a case wants. That the first
// statement sets names[i] to $<i+1> is takesArguments' rule, which every caller runs beside this.
func ownFindings(read *shellRead, seconds func(*syntax.Word) bool, function string, names ...string) []shellRefusal {
	ordinals := []string{"first", "second", "third"}
	// first holds the assignments of function's first statement, the one place names are set.
	first := map[*syntax.Assign]bool{}
	if decl := read.functions[function]; decl != nil {
		if block, ok := decl.Body.Cmd.(*syntax.Block); ok && len(block.Stmts) > 0 {
			if call, ok := block.Stmts[0].Cmd.(*syntax.CallExpr); ok && len(call.Args) == 0 {
				for _, a := range call.Assigns {
					first[a] = true
				}
			}
		}
	}
	var found []shellRefusal
	defined := map[string]int{}
	for _, d := range read.definitions {
		if ruledCommand(d.name) {
			found = append(found, shellRefusal{d.line, "a function named " + d.name + ", which takes the name of a command these rules read, so the shell runs it in that command's place"})
		}
		if defined[d.name]++; defined[d.name] == 2 {
			found = append(found, shellRefusal{d.line, "a function named " + d.name + ", defined more than once, so the body the rules read is not the one every call runs"})
		}
	}
	statements := map[*syntax.Assign]bool{}
	arithmetic := map[*syntax.ArithmExp]bool{}
	for _, loop := range read.loops {
		if loop.nested {
			found = append(found, shellRefusal{loop.line, "a wait inside another wait (an until, while or for in its condition or its body), which can set the deadline that wait reads again on every pass, so it may never end"})
		}
		if loop.kind != "until" {
			continue
		}
		if a, x := deadlineStatement(loop.before, seconds); a != nil {
			statements[a] = true
			arithmetic[x] = true
		}
	}
	for _, a := range read.assigns {
		i := slices.Index(names, a.name)
		switch {
		case a.name == "deadline" && !statements[a.node]:
			found = append(found, shellRefusal{a.line, "sets deadline other than right before its until, by deadline=$(( $(date +%s) + <seconds> )) alone, so the wait's end moves where its body reads it"})
		case i >= 0 && (a.function != function || !first[a.node]):
			found = append(found, shellRefusal{a.line, fmt.Sprintf("%s takes its %s from other than its %s argument: only its first statement sets it, to $%d, so a call's words may not be what it reads", function, a.name, ordinals[i], i+1)})
		}
	}
	for _, x := range read.arithmetic {
		if !arithmetic[x.node] {
			found = append(found, shellRefusal{x.line, "arithmetic other than a wait's deadline statement, which can assign through a variable's value (busybox sh reads that value as an expression, and every shell reads the text of $x), so deadline may move"})
		}
	}
	return found
}

// deadlineStatement answers the assignment and the arithmetic of stmt when it is a wait's
// deadline statement, deadline=$(( $(date +%s) + <seconds> )) alone, where seconds answers
// whether the word after + may give the seconds in the file it stands in; nil and nil when it is
// not.
func deadlineStatement(stmt *syntax.Stmt, seconds func(*syntax.Word) bool) (*syntax.Assign, *syntax.ArithmExp) {
	if stmt == nil || stmt.Negated || stmt.Background || len(stmt.Redirs) > 0 {
		return nil, nil
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) > 0 || len(call.Assigns) != 1 {
		return nil, nil
	}
	a := call.Assigns[0]
	if a.Name == nil || a.Name.Value != "deadline" || a.Value == nil || len(a.Value.Parts) != 1 {
		return nil, nil
	}
	arithmetic, ok := a.Value.Parts[0].(*syntax.ArithmExp)
	if !ok {
		return nil, nil
	}
	sum, ok := arithmetic.X.(*syntax.BinaryArithm)
	if !ok || sum.Op != syntax.Add {
		return nil, nil
	}
	now, ok := sum.X.(*syntax.Word)
	if !ok || len(now.Parts) != 1 {
		return nil, nil
	}
	date, ok := now.Parts[0].(*syntax.CmdSubst)
	if !ok || len(date.Stmts) != 1 || !callIs(date.Stmts[0], "date", "+%s") {
		return nil, nil
	}
	if by, ok := sum.Y.(*syntax.Word); !ok || !seconds(by) {
		return nil, nil
	}
	return a, arithmetic
}

// takesArguments answers whether function, an own function of read, takes its arguments as its
// first two statements: names[i]=$<i+1> for each of names and no more, then
// `shift <len(names)>`, with no other shift and no set that sets the arguments anywhere in it. A
// call's first word is then its seconds, and its words after those the function takes off are
// what its last command is given, as the shell builds them at that call. A shift that a condition
// can skip, that runs after another statement or in a substitution, or a second one, is not this
// form, whatever its text.
func takesArguments(read *shellRead, function string, names ...string) bool {
	block, ok := read.functions[function].Body.Cmd.(*syntax.Block)
	if !ok || len(block.Stmts) < 2 || !callIs(block.Stmts[1], "shift", strconv.Itoa(len(names))) {
		return false
	}
	first := block.Stmts[0]
	call, ok := first.Cmd.(*syntax.CallExpr)
	if !ok || first.Negated || first.Background || len(first.Redirs) > 0 || len(call.Args) > 0 || len(call.Assigns) != len(names) {
		return false
	}
	for i, a := range call.Assigns {
		if a.Name == nil || a.Name.Value != names[i] || a.Value == nil || readWord(a.Value).source != "$"+strconv.Itoa(i+1) {
			return false
		}
	}
	changes := 0
	for _, c := range read.commands {
		if c.function == function && changesArguments(c) {
			changes++
		}
	}
	return changes == 1
}

// changesArguments answers whether c sets the arguments of the function it runs in: a shift, or a
// set given more than options.
func changesArguments(c shellCommand) bool {
	return c.name.is("shift") || c.name.is("set") && setsArguments(c.args)
}

// setsArguments answers whether the words of a set set the arguments: any word but an option
// (-eu, +x) does, -- and - among them, and so may one whose value this program cannot know. An
// option o (-o, +o, or an o in a cluster such as -euo) takes the word after it as an option's
// name (set -o noglob), which sets no argument; that name must be literal text, since one the
// shell builds may give words after it (`set -o $x` with x='noglob 1' sets $1).
func setsArguments(args []shellWord) bool {
	for i := 0; i < len(args); i++ {
		w := args[i]
		if !w.literal || w.text == "--" || w.text == "-" || !strings.HasPrefix(w.text, "-") && !strings.HasPrefix(w.text, "+") {
			return true
		}
		for names := strings.Count(w.text[1:], "o"); names > 0 && i+1 < len(args); names-- {
			i++
			if !args[i].literal {
				return true
			}
		}
	}
	return false
}

// scriptHeadFindings answers, by line, what the head of a script that runs as a program of its
// own breaks (one-collection.sh, and a script a store step's run: names). The kernel runs such a
// file in the shell its first line names, so that line is #!/bin/sh, the POSIX sh this program
// reads: bash, for one, reads {--max-time,0,http://...}, one word to a POSIX parse, as three, and
// gives curl --max-time 0. No runner's -e reaches such a program, so its first statement is
// `set -eu`: with it missing, after a command or in a function, a failed command (one-collection.sh
// prepare among them) does not end the script, which goes on to write.
func scriptHeadFindings(text string) []shellRefusal {
	var found []shellRefusal
	if first, _, _ := strings.Cut(text, "\n"); first != "#!/bin/sh" {
		found = append(found, shellRefusal{1, "a first line other than #!/bin/sh: the file runs as a program in the shell that line names, which may read a word otherwise than this program, which reads POSIX sh, so it may hold a wait past its bound"})
	}
	file, err := parseShell(text)
	if err != nil {
		// The reader refuses a file that does not parse, by its own finding.
		return found
	}
	if len(file.Stmts) == 0 || !callIs(file.Stmts[0], "set", "-eu") {
		line := 1
		if len(file.Stmts) > 0 {
			line = int(file.Stmts[0].Pos().Line())
		}
		found = append(found, shellRefusal{line, "a first statement other than `set -eu`: the file runs as a program of its own, which no runner's -e reaches, so a failed command does not end it"})
	}
	return found
}
