// Package workflowstore is the one place in the tree that writes `workflow_nodes` and
// `workflow_tuples`: the record of each run of each step of an issue's workflow. A run is one node
// of kind KindStep, part of its issue by one `part_of` relation; each of its fields is an
// attribute, stated by one value tuple and held on the node row, which is the projection of its
// tuples (store/migrations/20261116235202_workflow_nodes.up.sql). Nothing here updates a tuple and
// nothing deletes one.
//
// The Legion daemon writes a run twice, at its start and at its end, under one id, and the two
// writes can arrive in either order, or more than once. So a write never refuses for its order: it
// sets each field the node does not hold yet, leaves each field it holds, but returned_to, which
// only the write that sets the result sets, and makes the node when none exists (PutStep). The
// read of an issue (IssueSteps) answers each run with its attempt, which no write stores.
//
// The vocabulary is the graph's node and tuple store's, which this tree does not hold yet, as
// internal/dispatch/prioritystore's is: the rows copy into that store's tables column for column
// the day it lands.
package workflowstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The kind and the supertag of a step's node, and the kinds of the nodes its relations reach: the
// issue it is part of and the handoff its run reported.
const (
	KindStep    = "step"
	KindIssue   = "issue"
	KindHandoff = "handoff"
)

// KindValue is the to_kind of an attribute tuple (tuple.toKind): its to_id (tuple.toID) holds the
// value itself rather than an address.
const KindValue = "value"

// The two relations a step states. RelationPartOf is written once, by the write that makes the
// node, to the issue; RelationReports is written once, by the write that sets the report, to the
// handoff the ending role's completion named.
const (
	RelationPartOf  = "part_of"
	RelationReports = "reports"
)

// Visibility and state, the two base attributes every node carries. A step is visible to every
// reader of its project, and no write ends one: a run that ended keeps its node.
const (
	VisibilityProject = "project"
	StateLive         = "live"
)

// NoEvent is the source_event of every row this store writes: a step write appends no event, since
// an event would feed the daemon's intake with facts it caused. `events.id` is a bigserial, so no
// event ever carries 0.
const NoEvent int64 = 0

// The three kinds of a run. StepKindStep is a step a plan names, at its place from 1; the retro and
// the merge are acts of Legion that no plan names, so their place is 0 and their kind tells them
// apart. Each is the one word contracts gives it, which the Legion daemon reads too.
const (
	StepKindStep  = contracts.StepKindStep
	StepKindRetro = contracts.StepKindRetro
	StepKindMerge = contracts.StepKindMerge
)

// The three results of a run that ended, each the word contracts gives it. A run that still runs
// has none.
const (
	ResultPassed  = contracts.StepResultPassed
	ResultFailed  = contracts.StepResultFailed
	ResultStopped = contracts.StepResultStopped
)

// The attributes of a step, each the name of its value tuples and of its key in the node row's
// `attributes`.
const (
	AttributeIssue      = "issue"
	AttributeProject    = "project"
	AttributeGeneration = "generation"
	AttributeTask       = "task"
	AttributeStep       = "step"
	AttributeKind       = "kind"
	AttributeRole       = "role"
	AttributeType       = "type"
	AttributeAgent      = "agent"
	AttributeSession    = "session"
	AttributeModel      = "model"
	AttributeStartedAt  = "started_at"
	AttributeEndedAt    = "ended_at"
	AttributeResult     = "result"
	AttributeReturnedTo = "returned_to"
	AttributeReport     = "report"
)

// The refusals a caller maps to an answer of its own. They are the only two: a write is never
// refused for its order or for a field the node already holds.
var (
	// ErrStepInput is a value outside its form, or a write that would make a node and does not
	// name the step's identity: its task, kind, step, role and generation. The error's text names
	// the field and the rule.
	ErrStepInput = errors.New("step input")
	// ErrStepOtherIssue is a write of an id whose node is part of another issue.
	ErrStepOtherIssue = errors.New("the step is a step of another issue")
)

// Step is one run of one step, as its node holds it. Place is the step's place in its task, the
// attribute `step`: from 1 for kind step, 0 for a retro and a merge; it is not named Step, which
// StepRead's embedded Step would shadow. A text field the node does not hold reads "", Place 0,
// and a time or ReturnedTo nil; a ReturnedTo of 0 is place 0 (StepWrite).
type Step struct {
	ID, Issue, Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report string
	Generation                                                                        int64
	Place                                                                             int
	ReturnedTo                                                                        *int
	StartedAt, EndedAt                                                                *time.Time
}

// StepWrite is one write of a run: the id and the issue it writes, and each field it states, nil
// where it states nothing. An empty text states nothing either, so a write that knows no model
// leaves the model to a later write that knows it. ReturnedTo is the place a run returned its task
// to, and 0 is a place: the retro's and the merge's, which a place alone does not tell apart.
// Nothing moves back to the merge, the last phase, so a ReturnedTo of 0 is a return to the retro.
type StepWrite struct {
	ID, Issue                                                              string
	Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report *string
	Generation                                                             *int64
	Place, ReturnedTo                                                      *int
	StartedAt, EndedAt                                                     *time.Time
}

// StepRead is a run as the read of its issue answers it: its node, and its attempt, the place of
// the run among the issue's runs of the same task, step and kind, from 1.
type StepRead struct {
	Step
	Attempt int
}

// Queryer is the query surface shared by a pool and a transaction. A write takes a pgx.Tx instead,
// because it moves the node row and appends its tuples together.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
