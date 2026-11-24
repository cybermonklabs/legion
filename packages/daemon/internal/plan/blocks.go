package plan

import (
	"fmt"
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
// its attributes as its opening line writes them, and its text.
type Block struct {
	ID, Type   string
	Attributes map[string]string
	Text       string
}

// FromBlocks reads one version of the plan document from its blocks: each `plan_task` block is a
// task and each `plan_role` block a role the plan defines, in the document's order, and every
// other block is skipped. A document with no task block is read as a plan with no task, which
// Validate refuses. It answers an error only for a block it cannot read at all: a task or a role
// with no id, one id on two such blocks, and a task whose `steps` is outside the grammar
// (ParseSteps); the error names the block.
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
			return Plan{}, fmt.Errorf("the id %s names a %s block and a %s block, so it names no one block", block.ID, earlier, block.Type)
		}
		typeOf[block.ID] = block.Type
		if block.Type == RoleBlockType {
			p.RoleBlocks = append(p.RoleBlocks, block.ID)
			continue
		}
		task, err := taskOf(block)
		if err != nil {
			return Plan{}, err
		}
		p.Tasks = append(p.Tasks, task)
	}
	return p, nil
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

// sameBlock says whether two tasks of one id say the same thing.
func sameBlock(a, b Task) bool {
	return a.Text == b.Text && a.PartOf == b.PartOf && a.Owner == b.Owner && a.Merges == b.Merges &&
		slices.Equal(a.Needs, b.Needs) && slices.Equal(a.Steps, b.Steps)
}
