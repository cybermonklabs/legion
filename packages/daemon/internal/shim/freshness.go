package shim

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The freshness of the working copy the adoption runs on, read with no snapshot, and the update
// of a working copy that is stale (runAdoption). jj refuses an ordinary command in a stale
// workspace ("The working copy is stale"), and no command run with --ignore-working-copy says
// whether it is, because only a snapshot runs jj's check (WorkingCopyFreshness::check_stale). So
// the adoption reads the facts that check reads and compares them as it does, and runs `jj
// workspace update-stale`, which snapshots, only on a working copy it read as stale: on a fresh
// one that command would take the snapshot the adoption's flags leave out.

// updateReserve is the part of the adoption's budget that `jj workspace update-stale` leaves to
// the reads after it: the smaller of 5 s and a quarter of the budget. The update stops at the
// budget less this reserve, so an update cut off at its bound still has the time to read which
// commits it left, and the whole adoption still ends inside the budget the daemon waits for.
func updateReserve(budget time.Duration) time.Duration { return min(5*time.Second, budget/4) }

// read runs one jj read of the workspace with no snapshot (--ignore-working-copy) and no color,
// so what it prints is what the parse below reads, and answers its trimmed standard output.
func (o openedWorkspace) read(args ...string) (string, error) {
	argv := make([]string, 0, len(args)+6)
	argv = append(argv, o.jj, "--ignore-working-copy")
	argv = append(argv, args...)
	argv = append(argv, "--color=never", "-R", o.dir)
	return o.run(nil, argv...)
}

// readFreshness reads whether the working copy is fresh, as jj's own check decides it, with no
// snapshot: every command it runs through read carries --ignore-working-copy. It answers the
// operation the working copy recorded too, "" where the first command failed or printed what
// does not parse, which bringUpToDate reads the change @ had at that operation from.
//
// The commands, in the order they run: `jj debug working-copy` prints the operation and the tree
// the working copy recorded (parseWorkingCopy), and `jj op log -n1 -T id` the repository's head
// operation. The two operations equal is fresh. Otherwise `jj log -r @ -T commit_id` names @'s
// commit, `jj debug object commit` prints that commit's tree and conflict labels (parseCommit),
// and `jj op log -n 1000` the head's last 1,000 operations, jj having no narrower way to ask
// whether one operation is an ancestor of another: the recorded tree and labels equal to the
// commit's, in order, and the recorded operation among those, is fresh, as jj finds a working
// copy fresh whose recorded operation is an ancestor of the head and whose tree is still its
// commit's. Every other case is stale, and so is each case the check cannot read, since a stale
// reading of a fresh working copy costs one `jj workspace update-stale`, which answers that the
// working copy is not stale, and a fresh reading of a stale one leaves the role's next jj
// command to fail: a command that fails writes one warning line with its error, and an output
// that does not parse one with its first line (warn).
func readFreshness(read func(args ...string) (string, error), warn func(line string)) (fresh bool, operation string) {
	failed := func(args []string, err error) {
		warn(fmt.Sprintf("warning: the freshness check's `jj %s` failed, so the working copy is taken as stale and brought up to date: %q",
			strings.Join(args, " "), shimwire.CutAnswerError(err.Error())))
	}
	unread := func(args []string, out string, err error) {
		warn(fmt.Sprintf("warning: the freshness check could not read what `jj %s` printed (%v), so the working copy is taken as stale and brought up to date; its first line: %q",
			strings.Join(args, " "), err, firstLine(out)))
	}
	// run reads one command's output, writing the warning of a command that failed.
	run := func(args ...string) (string, []string, bool) {
		out, err := read(args...)
		if err != nil {
			failed(args, err)
			return "", args, false
		}
		return out, args, true
	}

	out, args, ok := run("debug", "working-copy")
	if !ok {
		return false, ""
	}
	operation, recorded, err := parseWorkingCopy(out)
	if err != nil {
		unread(args, out, err)
		return false, ""
	}
	head, args, ok := run("op", "log", "-n1", "--no-graph", "-T", "id")
	if !ok {
		return false, operation
	}
	if !isHex(head) {
		unread(args, head, errors.New("no operation id"))
		return false, operation
	}
	if head == operation {
		return true, operation
	}
	commit, args, ok := run("log", "-r", "@", "--no-graph", "-T", "commit_id")
	if !ok {
		return false, operation
	}
	if !isHex(commit) {
		unread(args, commit, errors.New("no commit id"))
		return false, operation
	}
	dump, args, ok := run("debug", "object", "commit", commit)
	if !ok {
		return false, operation
	}
	current, err := parseCommit(dump)
	if err != nil {
		unread(args, dump, err)
		return false, operation
	}
	if !recorded.equal(current) {
		return false, operation
	}
	listed, args, ok := run("op", "log", "-n", "1000", "--no-graph", "-T", `id ++ "\n"`)
	if !ok {
		return false, operation
	}
	operations := workspace.OutputLines(listed)
	if slices.ContainsFunc(operations, func(id string) bool { return !isHex(id) }) || len(operations) == 0 {
		unread(args, listed, errors.New("a line that is no operation id"))
		return false, operation
	}
	return slices.Contains(operations, operation), operation
}

// bringUpToDate runs `jj workspace update-stale` on a working copy readFreshness read as stale,
// bounded to bound after the adoption's budget began, and answers each commit in which it left
// the edits that no jj command had recorded. On a stale working copy the update snapshots those
// edits into one commit of @'s change and checks out another, which does not hold them, so the
// change is left divergent with the edits in the commit that is not checked out and no longer on
// disk. So the commits of @'s change are read before the update and after it: each one that is
// new and is not @ holds such edits. Where another workspace abandoned the working-copy commit,
// @ is another change by now, and the update snapshots the edits into a commit of the change @
// had at the operation the working copy recorded (operation), so that change is read too; with
// no operation (the freshness check's first command failed or printed what does not parse) @'s
// change alone is.
//
// A read before the update that fails refuses the adoption with its error, as does the read
// after it. An update that fails refuses with its own error, which names the command and its
// exit or its timeout (workspace.RunCommand), and still answers what the read after it found: an
// update cut off at its bound may have written the commit already. That read runs in the reserve
// the update's bound leaves (updateReserve), so it has the time to.
func (s *shim) bringUpToDate(opened openedWorkspace, operation string, bound time.Duration) ([]string, error) {
	changes, err := opened.changes(operation)
	if err != nil {
		return nil, err
	}
	revset := "change_id(" + strings.Join(changes, ") | change_id(") + ")"
	before, err := opened.read("log", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	s.log.Printf("[worker-shim] the working copy is stale; bringing it up to date with jj workspace update-stale")
	_, updateErr := opened.runWithin(bound, opened.jj, "workspace", "update-stale", "-R", opened.dir)
	after, err := opened.read("log", "-r", "("+revset+") ~ @", "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		if updateErr != nil {
			return nil, updateErr
		}
		return nil, err
	}
	held := workspace.OutputLines(before)
	var edits []string
	for _, commit := range workspace.OutputLines(after) {
		if !slices.Contains(held, commit) {
			edits = append(edits, commit)
		}
	}
	return edits, updateErr
}

// changes is @'s change, and the change @ had at operation where that is another one: the two
// differ where another workspace abandoned the working-copy commit since the working copy
// recorded operation. operation "" reads @'s change alone.
func (o openedWorkspace) changes(operation string) ([]string, error) {
	current, err := o.read("log", "-r", "@", "--no-graph", "-T", "change_id")
	if err != nil {
		return nil, err
	}
	if !isChangeID(current) {
		return nil, fmt.Errorf("jj log -r @ -T change_id printed %q, which is no change id", firstLine(current))
	}
	if operation == "" {
		return []string{current}, nil
	}
	recorded, err := o.read("log", "-r", "@", "--no-graph", "-T", "change_id", "--at-op", operation)
	if err != nil {
		return nil, err
	}
	if !isChangeID(recorded) {
		return nil, fmt.Errorf("jj log -r @ -T change_id --at-op %s printed %q, which is no change id", operation, firstLine(recorded))
	}
	if recorded == current {
		return []string{current}, nil
	}
	return []string{current, recorded}, nil
}

// recordedTree is a tree and its conflict labels as jj's freshness check compares them: the tree
// ids in order (one for a resolved tree, a conflicted tree's sides), and the labels as the
// string literals jj's Debug printed them, quotes and escapes kept. resolved tells a resolved
// tree's one label from a conflicted tree's list.
type recordedTree struct {
	ids      []string
	resolved bool
	labels   []string
}

func (t recordedTree) equal(other recordedTree) bool {
	return slices.Equal(t.ids, other.ids) && t.resolved == other.resolved && slices.Equal(t.labels, other.labels)
}

// parseWorkingCopy reads what `jj debug working-copy` prints: the operation the working copy
// recorded, from `Current operation: OperationId("<hex>")`, and its tree, from `Current tree:
// MergedTree { tree_ids: …, labels: …, .. }`. tree_ids is `Resolved(TreeId("<hex>"))` or
// `Conflicted([TreeId("<hex>"), …])`; labels is `Unlabeled`, which is the one empty label a
// commit's dump prints as `Resolved("")`, or `Labeled([…])`, the list of string literals a
// commit's dump prints as `Conflicted([…])` (parseCommit). Every other line is passed over.
func parseWorkingCopy(out string) (operation string, tree recordedTree, err error) {
	const operationLine, treeLine = "Current operation: ", "Current tree: "
	var sawTree bool
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, operationLine):
			if operation != "" {
				return "", recordedTree{}, errors.New("two operation lines")
			}
			value, err := parseDebug(strings.TrimPrefix(line, operationLine))
			if err != nil {
				return "", recordedTree{}, fmt.Errorf("the operation: %w", err)
			}
			id, isOperation := value.tuple("OperationId")
			hex, isHexID := hexLiteral(id)
			if !isOperation || !isHexID {
				return "", recordedTree{}, errors.New("the operation is no OperationId of a hex id")
			}
			operation = hex
		case strings.HasPrefix(line, treeLine):
			if sawTree {
				return "", recordedTree{}, errors.New("two tree lines")
			}
			sawTree = true
			value, err := parseDebug(strings.TrimPrefix(line, treeLine))
			if err != nil {
				return "", recordedTree{}, fmt.Errorf("the tree: %w", err)
			}
			if value.word != "MergedTree" || value.fields == nil {
				return "", recordedTree{}, errors.New("the tree is no MergedTree")
			}
			ids, ok := treeIDs(value.fields["tree_ids"])
			if !ok {
				return "", recordedTree{}, errors.New("the tree's tree_ids are neither Resolved(TreeId) nor Conflicted([TreeId, …])")
			}
			tree.ids = ids
			labels := value.fields["labels"]
			switch listed, conflicted := labels.tuple("Labeled"); {
			case labels.bare("Unlabeled"):
				tree.resolved, tree.labels = true, []string{`""`}
			case conflicted:
				if tree.labels, ok = stringLiterals(listed); !ok {
					return "", recordedTree{}, errors.New("the tree's Labeled labels are no list of strings")
				}
			default:
				return "", recordedTree{}, errors.New("the tree's labels are neither Unlabeled nor Labeled([…])")
			}
		}
	}
	if operation == "" || !sawTree {
		return "", recordedTree{}, errors.New("no Current operation line and Current tree line")
	}
	return operation, tree, nil
}

// parseCommit reads the tree and the conflict labels of what `jj debug object commit` prints: a
// Rust Debug dump of the commit over many lines, `Commit { …, root_tree: …, conflict_labels: …,
// … }`. root_tree is `Resolved(TreeId("<hex>"))` or `Conflicted([TreeId("<hex>"), …])`, and
// conflict_labels `Resolved("<label>")` or `Conflicted(["<label>", …])`. Each field is read
// whole as a value, however jj breaks it over lines, and the commit's other fields are read only
// as values to pass over: a description that holds `root_tree:` is one string literal of the
// description field.
func parseCommit(out string) (recordedTree, error) {
	value, err := parseDebug(out)
	if err != nil {
		return recordedTree{}, err
	}
	if value.word != "Commit" || value.fields == nil {
		return recordedTree{}, errors.New("no Commit")
	}
	ids, ok := treeIDs(value.fields["root_tree"])
	if !ok {
		return recordedTree{}, errors.New("its root_tree is neither Resolved(TreeId) nor Conflicted([TreeId, …])")
	}
	tree := recordedTree{ids: ids}
	labels := value.fields["conflict_labels"]
	if label, ok := labels.tuple("Resolved"); ok {
		if !label.isString() {
			return recordedTree{}, errors.New("its Resolved conflict label is no string")
		}
		tree.resolved, tree.labels = true, []string{label.literal}
		return tree, nil
	}
	listed, ok := labels.tuple("Conflicted")
	if !ok {
		return recordedTree{}, errors.New("its conflict_labels are neither Resolved(…) nor Conflicted([…])")
	}
	if tree.labels, ok = stringLiterals(listed); !ok {
		return recordedTree{}, errors.New("its Conflicted conflict labels are no list of strings")
	}
	return tree, nil
}

// treeIDs is the tree ids of a Resolved(TreeId("<hex>")) or a Conflicted([TreeId("<hex>"), …]).
func treeIDs(value debugValue) ([]string, bool) {
	if id, ok := value.tuple("Resolved"); ok {
		hex, ok := treeID(id)
		return []string{hex}, ok
	}
	sides, ok := value.tuple("Conflicted")
	if !ok || !sides.list || len(sides.items) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(sides.items))
	for _, side := range sides.items {
		hex, ok := treeID(side)
		if !ok {
			return nil, false
		}
		ids = append(ids, hex)
	}
	return ids, true
}

// treeID is the hex id of a TreeId("<hex>").
func treeID(value debugValue) (string, bool) {
	id, ok := value.tuple("TreeId")
	if !ok {
		return "", false
	}
	return hexLiteral(id)
}

// hexLiteral is the hex id a string literal holds.
func hexLiteral(value debugValue) (string, bool) {
	if !value.isString() {
		return "", false
	}
	hex := value.literal[1 : len(value.literal)-1]
	return hex, isHex(hex)
}

// stringLiterals is each string literal of a list of them, as printed.
func stringLiterals(value debugValue) ([]string, bool) {
	if !value.list {
		return nil, false
	}
	literals := make([]string, 0, len(value.items))
	for _, item := range value.items {
		if !item.isString() {
			return nil, false
		}
		literals = append(literals, item.literal)
	}
	return literals, true
}

// isHex is whether s is a non-empty run of lowercase hex digits: a commit, tree or operation id
// as jj prints one.
func isHex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}

// isChangeID is whether s is a change id as jj prints one, a non-empty run of the letters k to z
// (jj's reverse hex). It is put in a revset (change_id(…)), so nothing else may pass.
func isChangeID(s string) bool {
	return s != "" && strings.Trim(s, "klmnopqrstuvwxyz") == ""
}

// firstLine is the first line of out.
func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return line
}

// debugValue is one value of the text Rust's Debug prints, which is how jj's debug commands
// print what they read: a bare word (a type or a variant with nothing after it, a number), a
// string or char literal kept as printed, a tuple (word(…)), a list ([…]) or a struct
// (word { field: value, … }).
type debugValue struct {
	// word is the bare word, or the type or variant name before a tuple's ( or a struct's {.
	word string
	// literal is a string or char literal as printed, its quotes and escapes kept.
	literal string
	// items is what a tuple's ( ) or a list's [ ] holds; list says it is a list.
	items []debugValue
	list  bool
	// fields is what a struct's { } holds, by name; nil for every other value.
	fields map[string]debugValue
}

// bare is whether the value is the bare word word: a unit variant, with no tuple or struct after it.
func (v debugValue) bare(word string) bool {
	return v.word == word && v.items == nil && v.fields == nil
}

// tuple is the one item of a tuple word(item).
func (v debugValue) tuple(word string) (debugValue, bool) {
	if v.word != word || v.fields != nil || v.list || len(v.items) != 1 {
		return debugValue{}, false
	}
	return v.items[0], true
}

// isString is whether the value is a string literal.
func (v debugValue) isString() bool { return len(v.literal) >= 2 && v.literal[0] == '"' }

// parseDebug reads text as one Rust Debug value, with nothing but white space after it. Line
// breaks are white space, so a value printed over many lines ({:#?}) reads as one printed on one.
func parseDebug(text string) (debugValue, error) {
	p := &debugParser{text: text}
	value, err := p.value()
	if err != nil {
		return debugValue{}, err
	}
	if p.peek() != 0 {
		return debugValue{}, fmt.Errorf("%q after the value, at byte %d", p.text[p.at], p.at)
	}
	return value, nil
}

// debugParser reads a Rust Debug value from text, from byte at on.
type debugParser struct {
	text string
	at   int
}

// peek is the next byte that is not white space, 0 at the end of the text.
func (p *debugParser) peek() byte {
	for p.at < len(p.text) && strings.IndexByte(" \t\r\n", p.text[p.at]) >= 0 {
		p.at++
	}
	if p.at == len(p.text) {
		return 0
	}
	return p.text[p.at]
}

func (p *debugParser) value() (debugValue, error) {
	switch c := p.peek(); {
	case c == '"' || c == '\'':
		literal, err := p.literal()
		return debugValue{literal: literal}, err
	case c == '[':
		p.at++
		items, err := p.items(']')
		return debugValue{items: items, list: true}, err
	case c == '(':
		p.at++
		items, err := p.items(')')
		return debugValue{items: items}, err
	case isWordByte(c):
		value := debugValue{word: p.word()}
		var err error
		switch p.peek() {
		case '(':
			p.at++
			value.items, err = p.items(')')
		case '{':
			p.at++
			value.fields, err = p.fields()
		}
		return value, err
	case c == 0:
		return debugValue{}, errors.New("the text ends where a value was due")
	default:
		return debugValue{}, fmt.Errorf("%q where a value was due, at byte %d", c, p.at)
	}
}

// literal reads a string or char literal whole: to the quote that closes it, past each quote a
// backslash escapes, as Rust's Debug escapes every quote and backslash inside one.
func (p *debugParser) literal() (string, error) {
	quote, start := p.text[p.at], p.at
	for p.at++; p.at < len(p.text); p.at++ {
		switch p.text[p.at] {
		case '\\':
			p.at++
		case quote:
			p.at++
			return p.text[start:p.at], nil
		}
	}
	return "", fmt.Errorf("a literal that opens at byte %d and never closes", start)
}

func (p *debugParser) word() string {
	start := p.at
	for p.at < len(p.text) && isWordByte(p.text[p.at]) {
		p.at++
	}
	return p.text[start:p.at]
}

// items reads the values of a tuple or a list up to closing, which a last comma may come before.
func (p *debugParser) items(closing byte) ([]debugValue, error) {
	items := []debugValue{}
	for {
		if p.peek() == closing {
			p.at++
			return items, nil
		}
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		switch c := p.peek(); c {
		case ',':
			p.at++
		case closing:
			p.at++
			return items, nil
		default:
			return nil, fmt.Errorf("%q where a comma or %q was due, at byte %d", c, closing, p.at)
		}
	}
}

// fields reads a struct's fields up to its }, each `name: value`, and the `..` of a struct that
// prints only some of them.
func (p *debugParser) fields() (map[string]debugValue, error) {
	fields := map[string]debugValue{}
	for {
		switch c := p.peek(); {
		case c == '}':
			p.at++
			return fields, nil
		case strings.HasPrefix(p.text[p.at:], ".."):
			p.at += 2
		case isWordByte(c):
			name := p.word()
			if p.peek() != ':' {
				return nil, fmt.Errorf("the field %s has no value, at byte %d", name, p.at)
			}
			p.at++
			value, err := p.value()
			if err != nil {
				return nil, fmt.Errorf("the field %s: %w", name, err)
			}
			if _, twice := fields[name]; twice {
				return nil, fmt.Errorf("the field %s twice", name)
			}
			fields[name] = value
		default:
			return nil, fmt.Errorf("%q where a field was due, at byte %d", c, p.at)
		}
		switch c := p.peek(); c {
		case ',':
			p.at++
		case '}':
			p.at++
			return fields, nil
		default:
			return nil, fmt.Errorf("%q where a comma or '}' was due, at byte %d", c, p.at)
		}
	}
}

// isWordByte is whether c may be part of a bare word: an identifier or a number, a negative one
// among them.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}
