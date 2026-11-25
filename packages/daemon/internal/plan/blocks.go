package plan

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
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
	// Parent is the id of the block of the same version that this block stands directly
	// under, which Places reads. FromBlocks reads it from a task block's `parent` (ParentOf)
	// before the tree, in place of what the caller set. It is "" for a block with no parent and
	// for a parent that is no block of this plan's document.
	Parent string
}

// FromBlocks reads one version of the plan document from its blocks: each `plan_task` block is a
// task and each `plan_role` block a role the plan defines, in the document's order, and every
// other block is skipped. document is the plan's document as a reference to one of its blocks
// names it, as ParentOf takes it: each task block's Parent is read from its `parent` against it,
// so a `parent` puts a block under a block of this document alone. Places says where each task
// block stands: a task at the top, a part of a task, which is a task of the plan with that task in
// its PartOf, or an element of a task, which is no task of the plan and is kept in Elements for
// Validate. Each task keeps who its parent is, its `parent` whole, in ParentRef. A task block whose
// Parent names an id that no block of blocks holds stands at the top with that id in
// Task.MissingParent, which Validate refuses. A
// `needs` word that names an element of document moves to Task.ElementNeeds (elementNeeds). A
// document with no task block is read as a plan with no task, which Validate refuses. It answers an
// error only for a block it cannot read at all: a task or a role with no id, one id on two such
// blocks, a task block whose `parent` is not one reference or names a block of document by an id
// that does not decode (ParentOf), or whose `steps` is outside the grammar (ParseSteps), and task
// blocks whose parents make a cycle (Places); the error names the block. blocks is not changed.
func FromBlocks(artifactID, document string, version int, blocks []Block) (Plan, error) {
	p := Plan{ArtifactID: artifactID, Version: version}
	typeOf := map[string]string{}
	var taskBlocks []Block
	var read []Task
	blocks = slices.Clone(blocks)
	for i, block := range blocks {
		if block.Type != TaskBlockType && block.Type != RoleBlockType {
			continue
		}
		if block.ID == "" {
			return Plan{}, fmt.Errorf("a %s block has no id, so no step of the plan can be named", block.Type)
		}
		if earlier, ok := typeOf[block.ID]; ok {
			return Plan{}, fmt.Errorf("the id %s names a %s block and a %s block, so it names no one block", block.ID, earlier, block.Type)
		}
		typeOf[block.ID] = block.Type
		if block.Type == RoleBlockType {
			p.RoleBlocks = append(p.RoleBlocks, block.ID)
			continue
		}
		parent, err := ParentOf(block.Attributes["parent"], document)
		if err != nil {
			return Plan{}, fmt.Errorf("task block %s: %w", block.ID, err)
		}
		block.Parent, blocks[i].Parent = parent, parent
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
	return elementNeeds(p, document), nil
}

// taskOf is one `plan_task` block as a task, before Places says where it stands. `needs` is its
// words, each kept whole; `parent` is kept whole as ParentRef; `merges` is read by its presence,
// and only the word false says the task does not merge.
func taskOf(block Block) (Task, error) {
	attributes := block.Attributes
	task := Task{ID: block.ID, Owner: attributes["owner"], ParentRef: strings.TrimSpace(attributes["parent"]), Text: block.Text}
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

// elementNeeds is p with each task's `needs` words that name an element of p (Plan.Elements)
// moved to Task.ElementNeeds in the block's order, each once. A word names an element where it
// names a block of the plan's document whose id is an element's (idInDocument). A reference to a
// block of another document or another issue names none, whatever its id. Every other word stays
// whole in Task.Needs, in its order. Validate refuses each word it moves, since Legion does not
// carry out an element, and Supported refuses each word it keeps.
func elementNeeds(p Plan, document string) Plan {
	if len(p.Elements) == 0 {
		return p
	}
	elements := make(map[string]bool, len(p.Elements))
	for _, element := range p.Elements {
		elements[element.ID] = true
	}
	tasks := make([]Task, len(p.Tasks))
	for i, task := range p.Tasks {
		words := task.Needs
		task.Needs = nil
		for _, word := range words {
			switch {
			case !elements[idInDocument(word, document)]:
				task.Needs = append(task.Needs, word)
			case !slices.Contains(task.ElementNeeds, word):
				task.ElementNeeds = append(task.ElementNeeds, word)
			}
		}
		tasks[i] = task
	}
	p.Tasks = tasks
	return p
}

// idInDocument is the id of the block of document that word names, read as neededID reads it: a
// reference to a block of document (`<document>#b-<id>`, as ParentOf reads a `parent`), or a word
// that is no `dispatch://` reference, the id of a block of the plan's own document. It is "" for a
// reference to anything else: a block of another document or another issue, an ask, a comment or
// a message.
func idInDocument(word, document string) string {
	if strings.HasPrefix(word, "dispatch://") && !strings.HasPrefix(word, document+"#b-") {
		return ""
	}
	return neededID(word)
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
// task whose block says anything else (its steps, its text, `needs`, `owner`, `merges`, the block
// its `parent` names, or the task it is a part of, which its `parent` gives), each task held has and
// next does not, and each task next adds. A change of the prose outside the task blocks changes no
// task, and neither does a change of `serves`, which says what a task is for and nothing Legion
// carries out, nor a change of an element, which is no task (Places), nor a pin added to or taken
// from the same parent (sameParent). The answer is never nil.
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

// sameBlock says whether two tasks of one id say the same thing.
func sameBlock(a, b Task) bool {
	return a.Text == b.Text && sameBesideText(a, b)
}

// sameBesideText says whether two tasks of one id say the same thing but for their text: the same
// steps, `needs`, `owner` and `merges`, the same task they are a part of, and the same parent
// (sameParent). It is the one rule of a change that is only text: the change a running task takes
// (RunningChange).
func sameBesideText(a, b Task) bool {
	return a.PartOf == b.PartOf && sameParent(a.ParentRef, b.ParentRef) && a.Owner == b.Owner && a.Merges == b.Merges &&
		slices.Equal(a.Needs, b.Needs) && slices.Equal(a.Steps, b.Steps)
}

// sameParent says whether two parents, each a block's `parent` whole (Task.ParentRef), name one
// block: each is compared without a trailing pin (`@v<N>`, pin), which names a version of the same
// block and no other block. A block that names no parent has "", which equals "" alone.
func sameParent(a, b string) bool {
	return unpinned(a) == unpinned(b)
}

// unpinned is reference without its trailing `@v<N>` (pin), and reference itself where it ends in
// none.
func unpinned(reference string) string {
	if at := strings.LastIndex(reference, "@"); at >= 0 && pin.MatchString(reference[at+1:]) {
		return reference[:at]
	}
	return reference
}

// RunningChange is the refusal of next's change of task, a task of held that runs: nil where next
// keeps the task's block but for its text (sameBesideText), which a running task takes, and
// RunningTaskChanged where next removes it or changes any other part of its block. A change of
// `serves` is no change of the task at all (ChangedTasks). The task's agent works on the steps,
// and the text says what each step must do: a later text reaches the later starts of the task, and
// leaves the work of the running step as it is.
func RunningChange(held, next Plan, task string) *Refusal {
	at := slices.IndexFunc(held.Tasks, func(t Task) bool { return t.ID == task })
	to := slices.IndexFunc(next.Tasks, func(t Task) bool { return t.ID == task })
	if at >= 0 && to >= 0 && sameBesideText(held.Tasks[at], next.Tasks[to]) {
		return nil
	}
	refusal := RunningTaskChanged(task)
	return &refusal
}

// RunningTaskChanged is the reason a later version of the plan is refused when it removes task,
// which is running, or changes more of it than its text (RunningChange): it names the task, says
// that it runs, and says that the owner stops it first.
func RunningTaskChanged(task string) Refusal {
	return Refusal{Task: task, Reason: fmt.Sprintf("task %s is running, so a later version can change only its text. "+
		"Stop it with stop_task before you change any other part of it or remove it.", task)}
}
