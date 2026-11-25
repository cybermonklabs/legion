package texts

import "strings"

// This file holds the records of the parts that a start puts before a task (supervise's
// Delivery.message joins them in front of the task with InFront, or with InFrontOfWords in front of
// a task no record drew, an operator's or one an earlier release queued, each a task of its own
// kind).

// RelaunchNoteRecord is what the last let-go of a claim's process ended, in front of a task or
// alone (supervise's RelaunchNote).
type RelaunchNoteRecord struct {
	TaskHead
	RelaunchFacts
}

// CutTurnNoteRecord is the note in front of a task that a let-go took back from a turn its process
// could not finish (supervise's CutTurnNote). dispatch://INFRA-3353's R2 keeps the note word
// for word, so the note is its Kept sentence and its cause is no field of its own: the sentence
// names it.
type CutTurnNoteRecord struct {
	TaskHead
	// Cause is why the process ended, in the claim's record's words (supervise.EndedWork.Cause),
	// empty where the record names no cause.
	Cause string `json:"-"`
}

// Kept is the cut-turn note, word for word as dispatch://INFRA-3353's R2 keeps it.
func (r CutTurnNoteRecord) Kept() []string {
	because := "Your process ended, and the daemon recorded no cause for it."
	if r.Cause != "" {
		because = "Your process ended because " + r.Cause + "."
	}
	return []string{strings.Join([]string{
		"Your previous turn on this task was interrupted.",
		because,
		"Your tool calls in flight were never answered.",
		"The daemon has no report of the background work of that turn.",
		"So the subagents and background jobs of that turn are gone.",
		"Every other subagent and background job that you started in that process is gone too.",
		"No result of that work will come to you.",
		"Your transcript does not say so.",
		"Before you do any of that work again, check what it already did.",
		"Then continue the task.",
	}, " ")}
}

// AbandonedSessionNoteRecord is the note in front of the first prompt of the session that
// replaced a session the claim abandoned (supervise's AbandonedSessionNote): the session left behind,
// why, in the words the claim recorded, and the issue's branch, which outlives the session (no
// branch for the controller, which works no checkout).
type AbandonedSessionNoteRecord struct {
	TaskHead
	Session string `json:"session"`
	Why     string `json:"why"`
	Branch  string `json:"branch,omitempty"`
}

// OutOfMemoryReason is the reason Kubernetes gives for a container it ended because the pod ran
// out of memory.
const OutOfMemoryReason = "OOMKilled"

// ToolsGuide is the part of the worker skill that says what work an issue pod does not run and
// where such work goes: the guide an out-of-memory note points its reader to
// (OutOfMemoryNoteRecord.ReadBeforeHeavyWork).
const ToolsGuide = "skill://legion-worker/references/tools.md"

// OutOfMemoryNoteRecord is the note in front of a task whose turn ended when its pod ran out of
// memory (NewOutOfMemoryNote): the reason Kubernetes ended the container that runs every
// role of the issue, and the guide to read before running heavy work again (ReadBeforeHeavyWork,
// ToolsGuide). dispatch://INFRA-1229's R2 has the role get, with its task, a note that its pod ran
// out of memory and a pointer to that guide, as main's note said ("read
// skill://legion-worker/references/tools.md before you run such work again"), and R8 of
// dispatch://INFRA-3686 keeps the pointer as a field.
type OutOfMemoryNoteRecord struct {
	TaskHead
	Reason              string `json:"reason"`
	ReadBeforeHeavyWork string `json:"read_before_heavy_work"`
}

// NewOutOfMemoryNote is the out-of-memory note of issue, which has no other input.
func NewOutOfMemoryNote(issue string) OutOfMemoryNoteRecord {
	return OutOfMemoryNoteRecord{TaskHead: TaskHead{Kind: OutOfMemoryNote, Issue: issue}, Reason: OutOfMemoryReason, ReadBeforeHeavyWork: ToolsGuide}
}

// ProviderErrorNoteRecord is the note in front of a task that a provider error took back
// (supervise's ProviderErrorNote): the HTTP status the provider answered, absent where it
// answered none.
type ProviderErrorNoteRecord struct {
	TaskHead
	HTTPStatus int `json:"http_status,omitempty"`
}
