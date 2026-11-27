package plan

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The two types of block a plan holds. Every other block of the document is its prose.
// RoleBlockType is no type of Dispatch's block schema yet: the release that lets an owner define
// a role adds it there under this name, with the role's name as the block's id.
const (
	TaskBlockType = "plan_task"
	RoleBlockType = "plan_role"
)

// Block is one block of a version of the plan document, as Dispatch gives it: its id, its type,
// its attributes as its opening line writes them, and its text. Dispatch gives a version's typed
// blocks alone, since only a typed block keeps its id from one read of the version to the next.
type Block struct {
	ID, Type   string
	Attributes map[string]string
	Text       string
	// Parent is the id of the block of the plan's document that this block stands directly
	// under, as its reader resolved it (ParentOf before the tree). It is "" for a block with no
	// parent and for a parent outside the plan's document. An id that no block of the version
	// holds stays, for Validate to refuse (Task.MissingParent).
	Parent string
}

// FromBlocks reads one version of the plan document of issue from its blocks: each `plan_task`
// block is a task and each `plan_role` block a role the plan defines, in the document's order, and
// every other block is skipped. Places says where each task block stands: a task at the top, a
// part of a task, which is a task of the plan with that task in its PartOf, or an element of a
// task, which is no task of the plan and is kept in Elements for Validate. A task block whose
// Parent names an id that no block of blocks holds stands at the top with that id in
// Task.MissingParent, which Validate refuses.
// Each task's needs are the ids of the tasks they name, read against issue (resolveNeeds), so no
// plan holds a need as a word. A document with no task block is read as a plan with no task, which
// Validate refuses. It answers an error only for a block it cannot read at all: a task or a role
// with no id, one id on two such blocks, a task whose id is outside Dispatch's task form
// (steprecord.TaskIDHolds), whose steps Dispatch could never record, a task block whose `steps` is
// outside the grammar (ParseSteps), and task blocks whose parents make a cycle (Places); the error
// names the block, its id or word quoted (quoted).
func FromBlocks(issue, artifactID string, version int, blocks []Block) (Plan, error) {
	p := Plan{ArtifactID: artifactID, Version: version}
	typeOf := map[string]string{}
	var taskBlocks []Block
	var read []Task
	for _, block := range blocks {
		if block.Type != TaskBlockType && block.Type != RoleBlockType {
			continue
		}
		if block.ID == "" {
			return Plan{}, fmt.Errorf("a %s block has no id, so no step of the plan can be named", block.Type)
		}
		if earlier, ok := typeOf[block.ID]; ok {
			return Plan{}, fmt.Errorf("the id %s names a %s block and a %s block, so it names no one block", quoted(block.ID), earlier, block.Type)
		}
		typeOf[block.ID] = block.Type
		if block.Type == RoleBlockType {
			p.RoleBlocks = append(p.RoleBlocks, block.ID)
			continue
		}
		if !steprecord.TaskIDHolds(block.ID) {
			// The error gives no length of its own: quoted's mark gives the length of what the URL
			// rule left of a cut id, and the length of the id as written would give away the length
			// of a credential it holds.
			return Plan{}, fmt.Errorf("the task %s has an id outside the form of a task id, %s, so Dispatch could record none of its steps",
				quoted(block.ID), steprecord.TaskIDRule)
		}
		task, err := taskOf(block)
		if err != nil {
			return Plan{}, err
		}
		taskBlocks, read = append(taskBlocks, block), append(read, task)
	}
	places, err := Places(blocks)
	if err != nil {
		return Plan{}, err
	}
	held := make(map[string]bool, len(blocks))
	for _, block := range blocks {
		held[block.ID] = true
	}
	for i, task := range read {
		block, place := taskBlocks[i], places[task.ID]
		if place.Kind == PlaceElement {
			p.Elements = append(p.Elements, elementOf(block, task, place))
			continue
		}
		if block.Parent != "" && !held[block.Parent] {
			task.MissingParent = block.Parent
		}
		task.PartOf = place.Task
		p.Tasks = append(p.Tasks, task)
	}
	return resolveNeeds(p, issue), nil
}

// taskOf is one `plan_task` block as a task, before Places says where it stands. `needs` is its
// words, each kept whole; `merges` is read by its presence, and only the word false says the task
// does not merge.
func taskOf(block Block) (Task, error) {
	attributes := block.Attributes
	task := Task{ID: block.ID, Owner: attributes["owner"], Text: block.Text}
	if needs := strings.Fields(attributes["needs"]); len(needs) > 0 {
		task.Needs = needs
	}
	if merges, ok := attributes["merges"]; ok && merges != "false" {
		task.Merges = true
	}
	steps, err := ParseSteps(attributes["steps"])
	if err != nil {
		return Task{}, fmt.Errorf("task %s: %w", block.ID, err)
	}
	if len(steps) > 0 {
		task.Steps = steps
	}
	return task, nil
}

// elementOf is task, read from block, as an element of a task (place): the block it stands
// directly under, and each attribute it names that only a task carries out, read as taskOf reads
// each one, `steps` with a step, `owner` with a role and `merges` by its presence.
func elementOf(block Block, task Task, place Place) Element {
	element := Element{ID: task.ID, Of: place.Task, Under: block.Parent}
	if task.NamesSteps() {
		element.Names = append(element.Names, "steps")
	}
	if task.Owner != "" {
		element.Names = append(element.Names, "owner")
	}
	if _, merges := block.Attributes["merges"]; merges {
		element.Names = append(element.Names, "merges")
	}
	return element
}

// resolveNeeds is p, the plan of issue, with each task's Needs as task ids: each word's id after its
// last "#b-", decoded as Dispatch encodes a block id (neededID), or the word itself where it holds
// no "#b-". The ids keep the order the block writes the words in, and a task a block names twice is
// needed once. The plan's document is a document of issue (register_plan refuses any other), so a
// `dispatch://` reference names a task of p only when it is of issue (issueOf): one into another
// issue, and one that starts with no issue key, names no task of p, whatever its block id, and
// stays in Task.OutsideIssue. A word whose id is an element of p (Plan.Elements) stays in
// Task.ElementNeeds, and any other word whose id is no task of p stays in Task.Unresolved.
// Validate refuses each of the three.
func resolveNeeds(p Plan, issue string) Plan {
	ids := make(map[string]bool, len(p.Tasks))
	for _, task := range p.Tasks {
		ids[task.ID] = true
	}
	elements := make(map[string]bool, len(p.Elements))
	for _, element := range p.Elements {
		elements[element.ID] = true
	}
	tasks := make([]Task, len(p.Tasks))
	for i, task := range p.Tasks {
		task.Needs, task.Unresolved, task.OutsideIssue, task.ElementNeeds = resolved(task.Needs, issue, ids, elements)
		tasks[i] = task
	}
	p.Tasks = tasks
	return p
}

// resolved is one task's `needs` words read as resolveNeeds reads them: the ids of the tasks of
// ids they name, the words that name no task, the words into another issue or none, and the words
// that name an element of elements, each kept once in the block's order. Each word is looked up in
// a set of what each list keeps already, so the read takes time linear in the words, where a scan
// of the kept list for each word would be quadratic in the words of one block, and one `needs`
// attribute within Dispatch's document limit holds hundreds of thousands of words.
// TestEachNeedOfAPlanIsReadInLinearTime holds the read to its thread's CPU time, since no count
// kept here would see a scan.
func resolved(words []string, issue string, ids, elements map[string]bool) (needs, unresolved, outside, elementNeeds []string) {
	if len(words) == 0 {
		return nil, nil, nil, nil
	}
	needed, left, away, ofElements := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	keep := func(list *[]string, kept map[string]bool, value string) {
		if !kept[value] {
			kept[value] = true
			*list = append(*list, value)
		}
	}
	for _, word := range words {
		if of, reference := issueOf(word); reference && of != issue {
			keep(&outside, away, word)
			continue
		}
		switch id := neededID(word); {
		case elements[id]:
			keep(&elementNeeds, ofElements, word)
		case !ids[id]:
			keep(&unresolved, left, word)
		default:
			keep(&needs, needed, id)
		}
	}
	return needs, unresolved, outside, elementNeeds
}

// issueOf is the issue a `needs` word names when it is a `dispatch://` reference, as Dispatch reads
// one (parseDispatch in packages/envoy/internal/dispatch/text): the key the reference starts with,
// before its first `/` or `#`, when that key is an issue key, and "" for a reference that starts
// with a project's key, `agent` or anything else, which names no issue. reference says whether the
// word is a `dispatch://` reference at all: a word with none names a task or a block of the plan's
// own document. The document a reference of the plan's own issue names is not read: a need names
// a task by its block id alone, so a block of another document of the issue with a task's id names
// that task.
func issueOf(word string) (issue string, reference bool) {
	rest, ok := strings.CutPrefix(word, "dispatch://")
	if !ok {
		return "", false
	}
	key, _, _ := strings.Cut(rest, "#")
	key, _, _ = strings.Cut(key, "/")
	if !claim.IsIssueKey(key) {
		return "", true
	}
	return key, true
}

// pin is the `@v<N>` that pins a reference to version N, as Dispatch reads one.
var pin = regexp.MustCompile(`^v[1-9][0-9]*$`)

// neededID is the id of the task a `needs` word names: the block id after the word's last `#b-`,
// read as Dispatch reads a block reference's fragment (EncodeBlockID and blockID in
// packages/envoy/internal/dispatch/text): an `@` is only ever a pin, which names a version of the
// document and no other block, and the rest is percent-decoded. A word with no `#b-` is the id
// itself. It is "" for a fragment with a malformed pin or an escape that does not decode, an id no
// task has: FromBlocks refuses a block with none.
func neededID(word string) string {
	at := strings.LastIndex(word, "#b-")
	if at < 0 {
		return word
	}
	encoded := word[at+len("#b-"):]
	if unpinned, version, pinned := strings.Cut(encoded, "@"); pinned {
		if !pin.MatchString(version) {
			return ""
		}
		encoded = unpinned
	}
	// url.PathUnescape answers "" with its error, for an escape that does not decode.
	id, _ := url.PathUnescape(encoded)
	return id
}

// ChangedTasks is the ids of the tasks next changes against held, in the order of their ids: each
// task whose block says anything else (its steps, its text, `needs`, `owner`, `merges`, or the task
// it is a part of, which its `parent` gives), each task held has and next does not, and each task
// next adds. A change of the prose outside the task blocks changes no task, and neither does a
// change of `serves`, which says what a task is for and nothing Legion carries out, nor a change of
// an element, which is no task (Places). The answer is never nil.
//
// It compares the tasks as the blocks write them, so a held plan is read from the blocks of the
// version that was given, not from the steps a task runs (a task with no steps runs the project's
// defaults, and its block still names none).
func ChangedTasks(held, next Plan) []string {
	before := make(map[string]Task, len(held.Tasks))
	for _, task := range held.Tasks {
		before[task.ID] = task
	}
	after := make(map[string]bool, len(next.Tasks))
	changed := []string{}
	for _, task := range next.Tasks {
		after[task.ID] = true
		if was, ok := before[task.ID]; !ok || !sameBlock(was, task) {
			changed = append(changed, task.ID)
		}
	}
	for _, task := range held.Tasks {
		if !after[task.ID] {
			changed = append(changed, task.ID)
		}
	}
	slices.Sort(changed)
	return changed
}

// OwnershipRefusals is why giver may not give next over held: one reason for each task of changed,
// the tasks next changes over held (ChangedTasks, which the caller computes once for its own
// rules too), whose writer (WriterOf) is not giver, in held where held has the task or in next
// where next has it, in the order of the ids, and none when giver writes each. The owner of the
// issue writes the tasks of the top level, and the sub-owner of a task writes its parts. A reason
// names the task's writer: a part, of a part among them, is its own task's sub-owner's, and the
// owner's reason says what the owner can do instead; a task of the top level is the owner's, which
// a sub-owner's reason says by naming the task it writes the parts of. A first version is compared
// with an empty held plan, so the owner's first version that writes a part is refused as a later
// one is. A task that next removes together with the task it is a part of, directly or through its
// tasks, is no change of that task's parts but goes with it, since a part cannot outlive its task,
// so it is refused nothing.
func OwnershipRefusals(held, next Plan, changed []string, giver Giver) []Refusal {
	before := make(map[string]Task, len(held.Tasks))
	for _, task := range held.Tasks {
		before[task.ID] = task
	}
	after := make(map[string]bool, len(next.Tasks))
	for _, task := range next.Tasks {
		after[task.ID] = true
	}
	var refusals []Refusal
	for _, id := range changed {
		task, inHeld := before[id]
		if !after[id] && removedWithItsTask(task, before, after) {
			continue
		}
		var writers []string
		if inHeld {
			writers = append(writers, held.WriterOf(id))
		}
		if after[id] {
			writers = append(writers, next.WriterOf(id))
		}
		for _, writer := range writers {
			if writer == string(giver) {
				continue
			}
			reason := fmt.Sprintf("task %s is a part of task %s, and only the sub-owner of task %s changes the parts of %s.", id, writer, writer, writer)
			switch {
			case writer == "":
				reason = fmt.Sprintf("task %s is not a part of task %s, and the sub-owner of task %s changes only the parts of %s.", id, giver, giver, giver)
			case giver == "":
				reason += fmt.Sprintf(" Leave the parts of %s to its sub-owner, which writes them, or write this work as tasks of your own, with needs, outside task %s.",
					writer, writer)
			}
			refusals = append(refusals, Refusal{Task: id, Reason: reason})
			break
		}
	}
	return refusals
}

// removedWithItsTask says whether a held task that the next version lacks is a part of a task
// the next version lacks too, directly or through its tasks: before is the held plan's tasks by
// id, and after the ids of the next version's.
func removedWithItsTask(task Task, before map[string]Task, after map[string]bool) bool {
	seen := map[string]bool{}
	for above := task.PartOf; above != "" && !seen[above]; above = before[above].PartOf {
		seen[above] = true
		if _, held := before[above]; !held {
			return false
		}
		if !after[above] {
			return true
		}
	}
	return false
}

// sameBlock says whether two tasks of one id say the same thing: their needs are compared as the
// tasks they name. Neither side holds a word that names no task: the held plan is read from stored
// rows, and a later version passed Validate, which refuses each such word.
func sameBlock(a, b Task) bool {
	return a.Text == b.Text && a.PartOf == b.PartOf && a.Owner == b.Owner && a.Merges == b.Merges &&
		slices.Equal(a.Needs, b.Needs) && slices.Equal(a.Steps, b.Steps)
}
