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
// the retro's run, in any order with the other two. It writes a run's plan under one id too, when
// the work starts and again at each later version of the owner's plan that Legion takes. So a
// write never refuses for its order, and makes the node when none exists. A run's write sets each
// field the node does not hold yet, but returned_to, which only the write that sets the result
// sets, and leaves each field it holds (PutStep). A plan's source, document, version and tasks are
// one fact, which a write of a higher version replaces whole and any other write leaves (PutPlan).
// The read of an issue (IssueSteps) answers each run with its attempt, which no write stores, and
// the read of a project (ProjectPlans) answers each of its finished issues with its plan, its
// ratings, its time and its count of failed steps.
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

// The kinds and the supertags of the two nodes this store writes, a step's run and a plan, and the
// kinds of the nodes their relations reach: the issue each is part of and the handoff a run
// reported.
const (
	KindStep    = "step"
	KindPlan    = "plan"
	KindIssue   = "issue"
	KindHandoff = "handoff"
)

// KindValue is the to_kind of an attribute tuple (tuple.toKind): its to_id (tuple.toID) holds the
// value itself rather than an address.
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
// owner's document and version it was read from (both empty for the source default, whose version
// 0 an owner's later version replaces), and its tasks, whose value tuple holds them as JSON text.
const (
	AttributeSource     = "source"
	AttributeArtifactID = "artifact_id"
	AttributeVersion    = "version"
	AttributeTasks      = "tasks"
)

// The two sources of a plan: the owner's document, or the default steps of an issue whose owner
// gave none, or whose experiment's group takes none from its owner.
const (
	PlanSourceDefault = "default"
	PlanSourceOwner   = "owner"
)

// The refusals a caller maps to an answer of its own. They are the only ones: a write is never
// refused for its order or for stating again a field the node already holds.
var (
	// ErrStepInput is a value outside its form, a write that would make a node and does not name
	// the step's identity (its task, kind, step, role and generation), or a type or a rating that
	// the kind the write names or the kind the node holds does not take (typeOfKind,
	// ratingOfKind). The error's text names the field and the rule.
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
// StepRead's embedded Step would shadow. A text field the node does not hold reads "", Place 0,
// and a time, ReturnedTo or Rating nil; a ReturnedTo of 0 is place 0 (StepWrite).
type Step struct {
	ID, Issue, Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report string
	Generation                                                                        int64
	Place                                                                             int
	ReturnedTo                                                                        *int
	Rating                                                                            *StepRating
	StartedAt, EndedAt                                                                *time.Time
}

// StepRating is the rating the agent that did a retro gave the plan its issue ran, 1 to 5, with
// its reason: one value, so a rating is never held or written without its reason, nor a reason
// without its rating. A write's Reason "" states no reason, which the write refuses.
type StepRating struct {
	Rating int
	Reason string
}

// StepWrite is one write of a run: the id and the issue it writes, and each field it states, nil
// where it states nothing. An empty text states nothing either, so a write that knows no model
// leaves the model to a later write that knows it. ReturnedTo is the place a run returned its task
// to, and 0 is a place: the retro's and the merge's, which a place alone does not tell apart.
// Nothing moves back to the merge, the last phase, so a ReturnedTo of 0 is a return to the retro.
// Only a retro takes a Rating.
type StepWrite struct {
	ID, Issue                                                              string
	Project, Task, Kind, Role, Type, Agent, Session, Model, Result, Report *string
	Generation                                                             *int64
	Place, ReturnedTo                                                      *int
	Rating                                                                 *StepRating
	StartedAt, EndedAt                                                     *time.Time
}

// StepRead is a run as the read of its issue answers it: its node, and its attempt, the place of
// the run among the issue's runs of the same task, step and kind, from 1.
type StepRead struct {
	Step
	Attempt int
}

// PlanTask is one task of a plan: its id, a slug unique in the plan, and its steps in order, each
// a role or `role:key` (`acceptance:owner`). It has the fields of model.PlanTask and of the plan
// write's body, which the route converts between, so a field added to one alone fails the build.
type PlanTask struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
}

// Plan is the plan one run of an issue ran from, as its node holds it: its source, the owner's
// document (nil for the source default) and its tasks.
type Plan struct {
	ID, Issue, Project, Source string
	Generation                 int64
	Document                   *PlanDocument
	Tasks                      []PlanTask
}

// PlanDocument is the document an owner gave as an issue's plan: its artifact id and the version
// of it Legion took, one value, so a plan never holds or names a document without its version, nor
// a version without its document. A write's ArtifactID "" or Version 0 states none of that half,
// which an owner's plan refuses.
type PlanDocument struct {
	ArtifactID string
	Version    int
}

// PlanWrite is one write of a plan: the id and the issue it writes, and each field it states. A
// plan write names its generation, its source and its tasks, and an owner's plan its document; a
// Document left zero states none, as a default plan's does. The project is set once, never
// required.
type PlanWrite struct {
	ID, Issue       string
	Project, Source *string
	Generation      *int64
	Document        PlanDocument
	Tasks           []PlanTask
}

// IssuePlan is one finished issue of a project as the read of the project answers it: its plan
// (the node of its newest run, nil where it has none), the rating of each retro of its runs (nil
// where no retro rated it; the route answers that as []), the earliest start and the latest end its
// runs hold (each nil where none holds one), and its count of runs of kind step that failed.
type IssuePlan struct {
	Issue, Title       string
	ClosedAt           time.Time
	Plan               *Plan
	Ratings            []Rating
	StartedAt, EndedAt *time.Time
	FailedSteps        int
}

// Rating is the rating of one retro: the rating and its reason, the id of the retro's run and the
// moment it ended, nil where its end never arrived. It has the fields of model.PlanRating, which the
// route converts it to, so a field added to one alone fails the build rather than being dropped.
type Rating struct {
	Rating  int
	Reason  string
	StepID  string
	EndedAt *time.Time
}

// Window is a span of time, [Since, Until), each nil for no bound: ProjectPlans bounds the close of
// the issues it lists by it, and the API's window parser answers one for a route's two bounds. It
// is one value so a reader cannot take the two bounds in the wrong order. A bound need not be a
// whole microsecond: ProjectPlans rounds each up to the next one (store.MicrosecondCeiling), which
// keeps its meaning against a close PostgreSQL holds to the microsecond.
type Window struct {
	Since, Until *time.Time
}

// Queryer is the query surface shared by a pool and a transaction. A write takes a pgx.Tx instead,
// because it moves the node row and appends its tuples together.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
