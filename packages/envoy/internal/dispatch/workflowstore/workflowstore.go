// Package workflowstore is the one place in the tree that writes `workflow_nodes` and
// `workflow_tuples`: the record of each run of each step of an issue's workflow, and the plan each
// run of the issue ran from. A run is one node of kind KindStep and a plan one node of kind
// KindPlan, each part of its issue by one `part_of` relation; each of its fields is an attribute,
// stated by one value tuple and held on the node row, which is the projection of its tuples
// (store/migrations/20261116235202_workflow_nodes.up.sql). Nothing here updates a tuple and
// nothing deletes one.
//
// The Legion daemon writes a run twice, at its start and at its end, under one id, and the two
// writes can arrive in either order, or more than once; the rating of a retro is a third write of
// the retro's run, in any order with the other two. So a write never refuses for its order: it
// sets each field the node does not hold yet, leaves each field it holds, and makes the node when
// none exists (PutStep, PutPlan). The read of an issue (IssueSteps) answers each run with its
// attempt, which no write stores, and the read of a project (ProjectPlans) answers each of its
// finished issues with its plan, its ratings, its time and its count of failed steps.
//
// The vocabulary is the node and tuple store's, which legion/INFRA-1149 is to add and this tree
// does not hold yet, as internal/dispatch/prioritystore's is: the rows copy into that store's
// tables column for column the day it lands.
package workflowstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The kinds and the supertags of the two nodes this store writes, a step's run and a plan, and the
// kinds of the nodes their relations reach: the issue each is part of and the handoff a run
// reported.
const (
	KindStep    = "step"
	KindPlan    = "plan"
	KindIssue   = "issue"
	KindHandoff = "handoff"
)

// KindValue is the ToKind of an attribute tuple: its ToID holds the value itself rather than an
// address.
const KindValue = "value"

// The two relations a node states. RelationPartOf is written once, by the write that makes the
// node, to the issue; RelationReports is written once, by the write that sets a run's report, to
// the handoff the ending role's completion named.
const (
	RelationPartOf  = "part_of"
	RelationReports = "reports"
)

// Visibility and state, the two base attributes every node carries. A run and a plan are visible
// to every reader of their project, and no write ends one: a run that ended keeps its node.
const (
	VisibilityProject = "project"
	StateLive         = "live"
)

// NoEvent is the SourceEvent of every row this store writes: a step write appends no event, since
// an event would feed the daemon's intake with facts it caused. `events.id` is a bigserial, so no
// event ever carries 0.
const NoEvent int64 = 0

// The three kinds of a run. StepKindStep is a step a plan names, at its place from 1; the retro and
// the merge are acts of Legion that no plan names, so their place is 0 and their kind tells them
// apart.
const (
	StepKindStep  = "step"
	StepKindRetro = "retro"
	StepKindMerge = "merge"
)

// The three results of a run that ended. A run that still runs has none.
const (
	ResultPassed  = "passed"
	ResultFailed  = "failed"
	ResultStopped = "stopped"
)

// The attributes of a step, each the name of its value tuples and of its key in the node row's
// `attributes`. A retro's run alone also holds AttributeRating and AttributeRatingReason: the
// rating, 1 to 5, the agent that did the retro gave the plan the issue ran, and its reason.
const (
	AttributeIssue        = "issue"
	AttributeProject      = "project"
	AttributeGeneration   = "generation"
	AttributeTask         = "task"
	AttributeStep         = "step"
	AttributeKind         = "kind"
	AttributeRole         = "role"
	AttributeType         = "type"
	AttributeAgent        = "agent"
	AttributeSession      = "session"
	AttributeModel        = "model"
	AttributeStartedAt    = "started_at"
	AttributeEndedAt      = "ended_at"
	AttributeResult       = "result"
	AttributeReturnedTo   = "returned_to"
	AttributeReport       = "report"
	AttributeRating       = "rating"
	AttributeRatingReason = "rating_reason"
)

// The attributes of a plan beside its issue, its project and its generation: its source, the
// owner's document and version it was read from (neither for the source default), and its tasks,
// whose value tuple holds them as JSON text.
const (
	AttributeSource     = "source"
	AttributeArtifactID = "artifact_id"
	AttributeVersion    = "version"
	AttributeTasks      = "tasks"
)

// The two sources of a plan: the owner's document, or the default steps of an issue whose owner
// gave none.
const (
	PlanSourceDefault = "default"
	PlanSourceOwner   = "owner"
)

// RatingReasonMost is the most a rating's reason holds, counted in Unicode code points as the
// daemon counts it when the retro's completion carries the reason, so every reason the daemon
// takes is one Dispatch takes.
const RatingReasonMost = 2000

// The refusals a caller maps to an answer of its own. They are the only ones: a write is never
// refused for its order or for a field the node already holds.
var (
	// ErrStepInput is a value outside its form, or a write that would make a node and does not
	// name the step's identity: its task, kind, step, role and generation. The error's text names
	// the field and the rule.
	ErrStepInput = errors.New("step input")
	// ErrStepOtherIssue is a write of an id whose node is part of another issue.
	ErrStepOtherIssue = errors.New("the step is a step of another issue")
	// ErrPlanInput is a plan's value outside its form, or a plan write that does not name its
	// generation, its source and its tasks. The error's text names the field and the rule.
	ErrPlanInput = errors.New("plan input")
	// ErrPlanOtherIssue is a plan write of an id whose node is part of another issue.
	ErrPlanOtherIssue = errors.New("the plan is a plan of another issue")
)

// Step is one run of one step, as its node holds it. Place is the step's place in its task, the
// attribute `step`: from 1 for kind step, 0 for a retro and a merge; it is not named Step, which
// StepRead's embedded Step would shadow. A text field the node does not hold reads "", Place,
// ReturnedTo and Rating 0, and a time nil.
type Step struct {
	ID, Issue, Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report, RatingReason string
	Generation                                                                                      int64
	Place, ReturnedTo, Rating                                                                       int
	StartedAt, EndedAt                                                                              *time.Time
}

// StepWrite is one write of a run: the id and the issue it writes, and each field it states, nil
// where it states nothing. An empty text and a ReturnedTo of 0 state nothing either, so a write
// that knows no model leaves the model to a later write that knows it. Rating and RatingReason are
// stated together or not at all, and only of a retro.
type StepWrite struct {
	ID, Issue                                                                            string
	Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report, RatingReason *string
	Generation                                                                           *int64
	Place, ReturnedTo, Rating                                                            *int
	StartedAt, EndedAt                                                                   *time.Time
}

// StepRead is a run as the read of its issue answers it: its node, and its attempt, the place of
// the run among the issue's runs of the same task, step and kind, from 1.
type StepRead struct {
	Step
	Attempt int
}

// PlanTask is one task of a plan: its id, a slug unique in the plan, and its steps in order, each
// a role or `role:key` (`acceptance:owner`).
type PlanTask struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
}

// Plan is the plan one run of an issue ran from, as its node holds it: its source, the owner's
// document and version ("" and 0 for the source default) and its tasks.
type Plan struct {
	ID, Issue, Project, Source, ArtifactID string
	Generation                             int64
	Version                                int
	Tasks                                  []PlanTask
}

// PlanWrite is one write of a plan: the id and the issue it writes, and each field it states. A
// plan write names its generation, its source and its tasks, and an owner's plan its document and
// version; the project is set once, never required.
type PlanWrite struct {
	ID, Issue                   string
	Project, Source, ArtifactID *string
	Generation                  *int64
	Version                     *int
	Tasks                       []PlanTask
}

// IssuePlan is one finished issue of a project as the read of the project answers it: its plan
// (the node of its newest run, nil where it has none), the rating of each retro of its runs, the
// earliest start and the latest end its runs hold (each nil where none holds one), and its count
// of runs of kind step that failed.
type IssuePlan struct {
	Issue, Title       string
	ClosedAt           time.Time
	Plan               *Plan
	Ratings            []Rating
	StartedAt, EndedAt *time.Time
	FailedSteps        int
}

// Rating is the rating of one retro: the rating and its reason, the id of the retro's run and the
// moment it ended, nil where its end never arrived.
type Rating struct {
	Rating  int
	Reason  string
	StepID  string
	EndedAt *time.Time
}

// Queryer is the query surface shared by a pool and a transaction. A write takes a pgx.Tx instead,
// because it moves the node row and appends its tuples together.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
