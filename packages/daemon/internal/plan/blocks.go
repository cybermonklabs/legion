package plan

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/steprecord"
)

// The two types of block a plan holds. Every other block of the document is its prose.
// RoleBlockType is no type of Dispatch's block schema yet: the release that lets an owner define
// a role adds it there under this name, with the role's name as the block's id.
const (
	TaskBlockType = "plan_task"
	RoleBlockType = "plan_role"
)

// Block is one block of a version of the plan document, as Dispatch gives it: its id, its type,
// its attributes as its opening line writes them, and its text.
type Block struct {
	ID, Type   string
	Attributes map[string]string
	Text       string
}

// FromBlocks reads one version of the plan document from its blocks: each `plan_task` block is a
// task and each `plan_role` block a role the plan defines, in the document's order, and every
// other block is skipped. Each task's needs are the ids of the tasks they name (resolveNeeds), so
// no plan holds a need as a word. A document with no task block is read as a plan with no task,
// which Validate refuses. It answers an error only for a block it cannot read at all: a task or a
// role with no id, one id on two such blocks, a task whose id is outside Dispatch's task form
// (steprecord.TaskIDHolds), whose steps Dispatch could never record, and a task whose `steps` is
// outside the grammar (ParseSteps); the error names the block, its id or word quoted (quoted).
func FromBlocks(artifactID string, version int, blocks []Block) (Plan, error) {
	p := Plan{ArtifactID: artifactID, Version: version}
	typeOf := map[string]string{}
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
		p.Tasks = append(p.Tasks, task)
	}
	return resolveNeeds(p), nil
}

// taskOf is one `plan_task` block as a task. `needs` is its words, each kept whole; `merges` is
// read by its presence, and only the word false says the task does not merge.
func taskOf(block Block) (Task, error) {
	attributes := block.Attributes
	task := Task{ID: block.ID, PartOf: attributes["part_of"], Owner: attributes["owner"], Text: block.Text}
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

// resolveNeeds is p with each task's Needs as task ids: each word's id after its last "#b-",
// decoded as Dispatch encodes a block id (neededID), or the word itself where it holds no "#b-".
// The ids keep the order the block writes the words in, and a task a block names twice is needed
// once. A word whose id is no task of p stays in Task.Unresolved, once however often the block
// writes it, and Validate refuses it.
func resolveNeeds(p Plan) Plan {
	ids := make(map[string]bool, len(p.Tasks))
	for _, task := range p.Tasks {
		ids[task.ID] = true
	}
	tasks := make([]Task, len(p.Tasks))
	for i, task := range p.Tasks {
		task.Needs, task.Unresolved = resolved(task.Needs, ids)
		tasks[i] = task
	}
	p.Tasks = tasks
	return p
}

// resolved is one task's `needs` words as the ids of the tasks of ids they name, and the words
// that name none, each kept once in the block's order. Each word is looked up in a set of what
// is kept already, so the read takes time linear in the words, where a scan of the kept list for
// each word would be quadratic in the words of one block, and one `needs` attribute within
// Dispatch's document limit holds hundreds of thousands of words.
// TestEachNeedOfAPlanIsReadInLinearTime holds the read to its thread's CPU time, since no count
// kept here would see a scan.
func resolved(words []string, ids map[string]bool) (needs, unresolved []string) {
	if len(words) == 0 {
		return nil, nil
	}
	needed, left := map[string]bool{}, map[string]bool{}
	for _, word := range words {
		id := neededID(word)
		switch {
		case !ids[id]:
			if !left[word] {
				left[word] = true
				unresolved = append(unresolved, word)
			}
		case !needed[id]:
			needed[id] = true
			needs = append(needs, id)
		}
	}
	return needs, unresolved
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
// task whose block says anything else (its steps, its text, `needs`, `owner`, `part_of` or
// `merges`), each task held has and next does not, and each task next adds. A change of the prose
// outside the task blocks changes no task, and neither does a change of `serves`, which says what
// a task is for and nothing Legion carries out. The answer is never nil.
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

// sameBlock says whether two tasks of one id say the same thing: their needs are compared as the
// tasks they name. Neither side holds a word that names no task: the held plan is read from stored
// rows, and a later version passed Validate, which refuses each such word.
func sameBlock(a, b Task) bool {
	return a.Text == b.Text && a.PartOf == b.PartOf && a.Owner == b.Owner && a.Merges == b.Merges &&
		slices.Equal(a.Needs, b.Needs) && slices.Equal(a.Steps, b.Steps)
}
