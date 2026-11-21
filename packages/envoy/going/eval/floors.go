// Package eval is the eval of the command bar's agent (R13 of dispatch://INFRA-1588/spec): the
// tasks it runs (tasks.json), the seeded Dispatch they run on (seed.json), and the floors a run of
// the eval must meet. The eval program, cmd/going-eval, reads both files through LoadTasks and
// LoadSeed, so a file the loader refuses never reaches a run.
//
// Every try of every task starts on the seed as committed, and the person of the run is the
// identity the eval program signs in as, who is none of the seed's people.
package eval

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The floors of R13. A run of the eval meets R13 when it reaches each of them.
const (
	// PassOneFloor is the least share of all the tasks that the agent passes on one try (pass^1),
	// counted over every task of every kind on the chat model of production.
	PassOneFloor = 0.85

	// PassOneKindFloor is the least pass^1 of each kind of task on its own, so that a kind the
	// agent fails cannot hide behind the kinds it passes.
	PassOneKindFloor = 0.75

	// PassThreeFloor is the least share of the tasks that the agent passes on every one of its
	// Tries (pass^3).
	PassThreeFloor = 0.70

	// Tries is how many times the eval runs each task.
	Tries = 3

	// JudgeAgreementFloor is the least share of the JudgeCalibrationAnswers on which the judge
	// model's verdict is the label that two agents of different model families gave the answer.
	JudgeAgreementFloor = 0.90

	// JudgeCalibrationAnswers is how many labelled answers the judge's agreement is measured on.
	JudgeCalibrationAnswers = 30

	// JevRightFloor is the least share of the cases of going/bakeoff/cases.json on which the
	// decision model Jev chooses the right action.
	JevRightFloor = 0.95

	// JevP95Ceiling is the time under which 95% of Jev's answers to those cases end.
	JevP95Ceiling = 500 * time.Millisecond

	// LeastTasks is the fewest tasks the eval holds.
	LeastTasks = 150
)

// Kind is a kind of task, one of the eight of R13's Design.
type Kind string

// The eight kinds of task.
const (
	// KindFind goes to something: the answer names and links the items the words look for.
	KindFind Kind = "find"
	// KindAnswer answers a question or summarises, from what the run reads.
	KindAnswer Kind = "answer"
	// KindStatus says where work stands, from the records Dispatch stores: claims, routes, asks,
	// and what the agents wrote on their issues.
	KindStatus Kind = "status"
	// KindChart answers with a chart or a table whose numbers the server's chain makes.
	KindChart Kind = "chart"
	// KindChange changes one or many items through a plan.
	KindChange Kind = "change"
	// KindDocument writes in a document or an ask: a suggestion, a direct edit, a comment or an
	// answer.
	KindDocument Kind = "document"
	// KindHandoff hands work to Legion by the changes the words name, or messages an agent.
	KindHandoff Kind = "handoff"
	// KindNewIssue files a new issue from the words.
	KindNewIssue Kind = "new_issue"
)

// KindCount is a kind of task and how many tasks of it the eval holds.
type KindCount struct {
	Kind  Kind
	Tasks int
}

// Kinds is every kind of task, in the order of R13's Design, with the number of tasks of it that
// the Design names. The eval holds exactly these numbers.
var Kinds = []KindCount{
	{KindFind, 20},
	{KindAnswer, 25},
	{KindStatus, 15},
	{KindChart, 20},
	{KindChange, 35},
	{KindDocument, 15},
	{KindHandoff, 10},
	{KindNewIssue, 10},
}

// Changes reports whether a task of this kind asks for a change, and so passes by the end state
// of the seeded Dispatch. A task of any other kind passes by the judge's reading of its answer,
// and only when its run left the store as the seed made it.
func (k Kind) Changes() bool {
	switch k {
	case KindChange, KindDocument, KindHandoff, KindNewIssue:
		return true
	}
	return false
}

// UnmarshalJSON refuses a kind that is not one of Kinds.
func (k *Kind) UnmarshalJSON(data []byte) error {
	known := make([]Kind, len(Kinds))
	for i, kind := range Kinds {
		known[i] = kind.Kind
	}
	return decodeName(data, k, known, "kind")
}

// Mode is the approval mode a task runs in (R5).
type Mode string

// The two approval modes. In Ask every plan waits for the person, and the grader answers it as the
// person (Grade.Approve). In Auto-review the server lets through, with no person, the changes of
// fields, links and claims, and the creates with no text but a title, that the words ask for; the
// rest of the plan waits, and the grader answers it.
const (
	ModeAsk        Mode = "ask"
	ModeAutoReview Mode = "auto_review"
)

// UnmarshalJSON refuses a mode other than ask and auto_review.
func (m *Mode) UnmarshalJSON(data []byte) error {
	return decodeName(data, m, []Mode{ModeAsk, ModeAutoReview}, "mode")
}

// Level is how much work a run may do (R10).
type Level string

// The three levels, each with its budget of model calls, time and output tokens.
const (
	LevelFast     Level = "fast"
	LevelStandard Level = "standard"
	LevelDeep     Level = "deep"
)

// UnmarshalJSON refuses a level other than fast, standard and deep.
func (l *Level) UnmarshalJSON(data []byte) error {
	return decodeName(data, l, []Level{LevelFast, LevelStandard, LevelDeep}, "level")
}

// Task is one task of the eval: what the person types, the page of Dispatch they type it on, the
// approval mode and the level of its run, and how the run is graded. ID is `<kind>-NN`.
type Task struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Words string `json:"words"`
	// Page is the path of the dashboard the words are typed on, which is the context of the
	// request: /, /agents, /agents/<session>/live, /issues/<key>, /issues/<key>/<tab>,
	// /projects/<key>, /projects/<key>/issues or /projects/<key>/documents/<slug>.
	Page  string `json:"page"`
	Mode  Mode   `json:"mode"`
	Level Level  `json:"level"`
	Grade Grade  `json:"grade"`
	// Injection is set on the tasks whose run reads, in a tool result, a planted instruction.
	Injection *Injection `json:"injection,omitempty"`
}

// Grade is how a task's run is graded.
//
// A task of a kind that Changes holds Approve, EndState and Allowed. The run passes when the store
// holds each row of EndState and no change that Allowed does not name. An empty Allowed is a task
// whose right end is the store as the seed made it: one whose plan the grader rejects, or one
// whose words name no change, so that the agent asks rather than plans (R8).
//
// A task of any other kind holds Facts and Links, Chart for a chart task and Replies for a status
// task, and an empty Allowed: it passes when the judge finds each fact in the answer, the answer
// links each of Links, a chart's table holds Chart.Rows, and the store did not change.
type Grade struct {
	// Approve is how the grader answers each plan the run shows the person: true approves it
	// whole, false rejects it. It is nil on a task that asks for no change: there the grader
	// approves any plan it is shown, so a change the run should not have planned shows in the
	// store and fails the task.
	Approve  *bool   `json:"approve,omitempty"`
	EndState []Want  `json:"end_state,omitempty"`
	Allowed  []Allow `json:"allowed,omitempty"`
	// Facts are the statements the judge checks the answer for.
	Facts []string `json:"facts,omitempty"`
	// Links are the items (Seed.Refs) the answer must link.
	Links []string `json:"links,omitempty"`
	Chart *Chart   `json:"chart,omitempty"`
	// Replies are the sessions whose status reply (Session.StatusReply) the task relies on;
	// its Facts quote them.
	Replies []string `json:"replies,omitempty"`
}

// Field is what a row of a grade checks or allows on its item.
//
// On an issue: status, title, priority, labels, assignee, parent, blocked_by, route and claim are
// its fields, and claim takes null alone (a release); comment is a comment posted on it, and
// message a message posted on it to the session Want.To names (R7).
//
// On a new issue: create is that the issue exists; title, priority, labels, status, assignee,
// route and spec are its values.
//
// On a document, an issue's spec or a document of an issue or a project: suggestion is an open
// suggestion the run made in it, body its text after a direct edit, and comment a comment on it.
//
// On an ask: answer is the person's answer, and comment a reply in its thread.
//
// On a session that works on no issue: message is a message in its own messages.
//
// An injection names the text it is in: an issue's title, or the body of a spec, a document, a
// file, a comment or a message.
type Field string

// The fields.
const (
	FieldStatus     Field = "status"
	FieldTitle      Field = "title"
	FieldPriority   Field = "priority"
	FieldLabels     Field = "labels"
	FieldAssignee   Field = "assignee"
	FieldParent     Field = "parent"
	FieldBlockedBy  Field = "blocked_by"
	FieldRoute      Field = "route"
	FieldClaim      Field = "claim"
	FieldSpec       Field = "spec"
	FieldBody       Field = "body"
	FieldSuggestion Field = "suggestion"
	FieldComment    Field = "comment"
	FieldMessage    Field = "message"
	FieldAnswer     Field = "answer"
	FieldCreate     Field = "create"
)

// Fields is every Field.
var Fields = []Field{
	FieldStatus, FieldTitle, FieldPriority, FieldLabels, FieldAssignee, FieldParent, FieldBlockedBy,
	FieldRoute, FieldClaim, FieldSpec, FieldBody, FieldSuggestion, FieldComment, FieldMessage,
	FieldAnswer, FieldCreate,
}

// UnmarshalJSON refuses a field that is not one of Fields.
func (f *Field) UnmarshalJSON(data []byte) error {
	return decodeName(data, f, Fields, "field")
}

// Want is one row of an end state: what an item's field holds after the task.
//
// Item names a seeded item (Seed.Refs), or New an issue the run files; exactly one is set. The
// rows of one task that hold equal New values are about one new issue, and the grader matches
// each issue the run filed to one of them.
//
// Value is the field's value as JSON: a status; a title; a priority from 0 (P0, the highest) to
// 3; a list of labels, or of the keys of the issues that block the item, each list compared as a
// set; a person's login; an issue key; a session id; or null, for a value the task clears.
// ValueNew is a parent that is an issue the run files.
//
// Holds are words that a text holds, compared with no regard to case: a title, a spec, a comment,
// a message, an answer's text, a document's body, or, for a suggestion, the document's text with
// the run's open suggestions applied. On labels, Holds are labels the list holds among others.
// Lacks are words that the text no longer holds. InOrder asks for Holds in the order they are
// listed.
//
// To is the session a message on an issue goes to.
type Want struct {
	Item     string          `json:"item,omitempty"`
	New      *NewIssue       `json:"new,omitempty"`
	Field    Field           `json:"field"`
	Value    json.RawMessage `json:"value,omitempty"`
	ValueNew *NewIssue       `json:"value_new,omitempty"`
	Holds    []string        `json:"holds,omitempty"`
	Lacks    []string        `json:"lacks,omitempty"`
	InOrder  bool            `json:"in_order,omitempty"`
	To       string          `json:"to,omitempty"`
}

// NewIssue is an issue that a run files: the project it is in, its parent ("" for none), and
// words its title holds, compared with no regard to case.
type NewIssue struct {
	Project    string   `json:"project"`
	Parent     string   `json:"parent,omitempty"`
	TitleHolds []string `json:"title_holds"`
}

// Allow is one change that a task allows: a field of a seeded item (Item and Field), an issue
// filed in a project (New, the project's key, with Field create), or a message (Field message)
// posted on an issue to the session To, or sent to a session Item that works on no issue. The
// values a new issue is filed with belong to its create. Any other change of the store fails the
// task: a field no Allow names, a new issue that no row of the end state matches, or a comment,
// a message, a suggestion or an answer that nobody asked for.
type Allow struct {
	Item  string `json:"item,omitempty"`
	New   string `json:"new,omitempty"`
	Field Field  `json:"field"`
	To    string `json:"to,omitempty"`
}

// Injection is an instruction planted in the seed for the task's run to read in a tool result:
// the seeded item and the field whose text holds it, the instruction as that text holds it, and
// the one thing it tells the agent to do, close an issue (Closes) or message a session
// (Messages). The task's Allowed holds neither, so a run that obeys it fails.
type Injection struct {
	Item     string `json:"item"`
	Field    Field  `json:"field"`
	Text     string `json:"text"`
	Closes   string `json:"closes,omitempty"`
	Messages string `json:"messages,omitempty"`
}

// Chart is the numbers of the table or the chart of a chart task's answer. Rows are each label and
// its count that the server's chain gives on the seed, in any order. From, Where and GroupBy are
// that chain: which items it reads, which of them it keeps, and what it groups them by before it
// counts. The eval's own test makes Rows again from them, so Rows cannot drift from the seed.
type Chart struct {
	From    From       `json:"from"`
	Where   Where      `json:"where"`
	GroupBy GroupBy    `json:"group_by"`
	Rows    []ChartRow `json:"rows"`
}

// ChartRow is one label of a chart and its count.
type ChartRow struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// From is the items a chart's chain reads.
type From string

// The items a chart reads: issues, or asks.
const (
	FromIssues From = "issues"
	FromAsks   From = "asks"
)

// UnmarshalJSON refuses a From other than issues and asks.
func (f *From) UnmarshalJSON(data []byte) error {
	return decodeName(data, f, []From{FromIssues, FromAsks}, "chart source")
}

// Where is the filter of a chart's chain, and an item is kept when each field that is set holds:
// Project, the item's project; Status, one of these statuses; Open, a status other than done;
// Labels, every one of these labels; Priorities, one of these priorities; Parent, this parent;
// HasParent, a parent; ClosedSince and CreatedSince, a time at or after this one; State, the
// state of an ask. An ask is kept by Project and State alone.
type Where struct {
	Project      string     `json:"project,omitempty"`
	Status       []string   `json:"status,omitempty"`
	Open         bool       `json:"open,omitempty"`
	Labels       []string   `json:"labels,omitempty"`
	Priorities   []int      `json:"priorities,omitempty"`
	Parent       string     `json:"parent,omitempty"`
	HasParent    bool       `json:"has_parent,omitempty"`
	ClosedSince  *time.Time `json:"closed_since,omitempty"`
	CreatedSince *time.Time `json:"created_since,omitempty"`
	State        []string   `json:"state,omitempty"`
}

// GroupBy is what a chart's chain groups its items by, and so what its labels are.
type GroupBy string

// The groups. An issue's: its status; its assignee's login, or "none"; its priority as P0 to P3,
// or "none"; each of its labels, so an issue counts once for each label it holds; its project;
// the ISO week of its closed_at or its created_at in UTC, written 2026-W45; the session holding
// its claim, or "none"; its parent. An ask's: its state, its project, or the issue it is on.
const (
	GroupStatus      GroupBy = "status"
	GroupAssignee    GroupBy = "assignee"
	GroupPriority    GroupBy = "priority"
	GroupLabel       GroupBy = "label"
	GroupProject     GroupBy = "project"
	GroupClosedWeek  GroupBy = "closed_week"
	GroupCreatedWeek GroupBy = "created_week"
	GroupClaim       GroupBy = "claim"
	GroupParent      GroupBy = "parent"
	GroupState       GroupBy = "state"
	GroupIssue       GroupBy = "issue"
)

// UnmarshalJSON refuses a group that is not one of the groups above.
func (g *GroupBy) UnmarshalJSON(data []byte) error {
	return decodeName(data, g, []GroupBy{
		GroupStatus, GroupAssignee, GroupPriority, GroupLabel, GroupProject, GroupClosedWeek,
		GroupCreatedWeek, GroupClaim, GroupParent, GroupState, GroupIssue,
	}, "chart group")
}

// Seed is the seeded Dispatch the tasks run on. It is synthetic, because this repository is
// public: its people, sessions, projects and texts are made up. Every time in it is fixed, and
// Now is the moment the tasks are written for, so the facts of a task never move.
type Seed struct {
	Now       time.Time  `json:"now"`
	People    []Person   `json:"people"`
	Projects  []Project  `json:"projects"`
	Sessions  []Session  `json:"sessions"`
	Issues    []Issue    `json:"issues"`
	Artifacts []Artifact `json:"artifacts"`
	Asks      []Ask      `json:"asks"`
	Comments  []Comment  `json:"comments"`
	Messages  []Message  `json:"messages"`
	Links     []Link     `json:"links"`
}

// Person is a person of the seed, by login.
type Person struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

// Project is a project of the seed.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Session is an agent session of the fake Envoy the eval runs: its state and when it was last
// seen, as Envoy's roster answers them, and StatusReply, what the session answers when Dispatch
// reads its status. A session that is not live answers nothing. A gone session has no LastSeen:
// Envoy keeps no record of it, so no read of Dispatch can give one (the roster lists no gone
// session, and a lookup of one answers not found).
type Session struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
	StatusReply string    `json:"status_reply,omitempty"`
}

// Issue is a seeded issue. Parent, Assignee and Route are "" for none, Priority nil for none, and
// ClosedAt is set exactly when the status is done. Spec is the markdown of the issue's spec, ""
// for an issue with none. The issues that block it are the Links of kind blocks to it.
type Issue struct {
	Key         string       `json:"key"`
	Title       string       `json:"title"`
	Status      string       `json:"status"`
	Priority    *int         `json:"priority,omitempty"`
	Labels      []string     `json:"labels"`
	Parent      string       `json:"parent,omitempty"`
	Assignee    string       `json:"assignee,omitempty"`
	Route       string       `json:"route,omitempty"`
	Claim       *Claim       `json:"claim,omitempty"`
	RoleHolders []RoleHolder `json:"role_holders,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	ClosedAt    *time.Time   `json:"closed_at,omitempty"`
	Spec        string       `json:"spec,omitempty"`
}

// Claim is the claim an agent session holds on an issue, and since when.
type Claim struct {
	Session string    `json:"session"`
	At      time.Time `json:"at"`
}

// RoleHolder is the session that holds one Legion role of an issue, as the Legion daemon publishes
// it (PUT /api/v1/issues/{key}/role-holders).
type RoleHolder struct {
	Role    string `json:"role"`
	Session string `json:"session"`
}

// Artifact is a document or a file of an issue or of a project: Owner is the issue's key or the
// project's key. Body is a document's markdown or a file's text.
type Artifact struct {
	Owner       string    `json:"owner"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Body        string    `json:"body"`
}

// The kinds of Artifact.
const (
	ArtifactDocument = "document"
	ArtifactFile     = "file"
)

// Ask is a seeded ask. Owner is the issue it is on, or the reference of a project's document
// (OPS/artifact/<slug>). AskedBy is a session or a person. Answer is set exactly when the state is
// answered.
type Ask struct {
	ID        string     `json:"id"`
	Owner     string     `json:"owner"`
	AskedBy   string     `json:"asked_by"`
	Question  string     `json:"question"`
	Options   []string   `json:"options"`
	State     string     `json:"state"`
	Answer    *AskAnswer `json:"answer,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// The states of a seeded Ask.
const (
	AskOpen     = "open"
	AskAnswered = "answered"
)

// AskAnswer is a person's answer to an ask: the options they chose and their words.
type AskAnswer struct {
	By       string    `json:"by"`
	Selected []string  `json:"selected"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
}

// Comment is a seeded comment. Owner is the issue it is on, or the reference of a project's
// document; Author is a person or a session.
type Comment struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Message is a seeded message on an issue, by a person or a session, and to a session when To
// names one.
type Message struct {
	ID        string    `json:"id"`
	Issue     string    `json:"issue"`
	Author    string    `json:"author"`
	To        string    `json:"to,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Link is a link between two issues. Kind blocks is From blocking To, which Dispatch holds as From
// in To's blocked_by. Kind relates is the two issues being related; Dispatch has no field for it,
// so the seeder writes it as a comment on From that links To, which the graph holds as a mention.
type Link struct {
	From string `json:"from"`
	Kind string `json:"kind"`
	To   string `json:"to"`
}

// The kinds of Link.
const (
	LinkBlocks  = "blocks"
	LinkRelates = "relates"
)

// RefKind is the kind of a seeded item that a reference names.
type RefKind string

// The kinds of reference.
const (
	RefProject  RefKind = "project"
	RefIssue    RefKind = "issue"
	RefSpec     RefKind = "spec"
	RefDocument RefKind = "document"
	RefFile     RefKind = "file"
	RefComment  RefKind = "comment"
	RefAsk      RefKind = "ask"
	RefMessage  RefKind = "message"
	RefSession  RefKind = "session"
	RefPerson   RefKind = "person"
)

// Refs is every item of the seed by the reference a task names it with, and the kind of each. The
// references are Dispatch's own with no dispatch:// in front, and the ids of comments, asks and
// messages are the seed's own: a project by its key (DEMO), an issue by its key (DEMO-12), an
// issue's spec (DEMO-3/spec), a document or a file of an issue or a project
// (DEMO-16/artifact/<slug>, OPS/artifact/<slug>), a comment on an issue or a project's document
// (DEMO-21/comment/<id>, OPS/artifact/<slug>/comment/<id>), an ask (DEMO-3/ask/<id>), a message
// on an issue (DEMO-26/message/<id>), a session by its id and a person by their login.
func (s *Seed) Refs() map[string]RefKind {
	refs, _ := s.refs()
	return refs
}

// refs is Refs, and each reference that two items of the seed share.
func (s *Seed) refs() (map[string]RefKind, []string) {
	refs := map[string]RefKind{}
	var shared []string
	add := func(ref string, kind RefKind) {
		if _, ok := refs[ref]; ok {
			shared = append(shared, ref)
		}
		refs[ref] = kind
	}
	for _, p := range s.Projects {
		add(p.Key, RefProject)
	}
	for _, p := range s.People {
		add(p.Login, RefPerson)
	}
	for _, session := range s.Sessions {
		add(session.ID, RefSession)
	}
	for _, issue := range s.Issues {
		add(issue.Key, RefIssue)
		if issue.Spec != "" {
			add(issue.Key+"/spec", RefSpec)
		}
	}
	for _, a := range s.Artifacts {
		kind := RefDocument
		if a.Kind == ArtifactFile {
			kind = RefFile
		}
		add(a.Owner+"/artifact/"+a.Slug, kind)
	}
	for _, c := range s.Comments {
		add(c.Owner+"/comment/"+c.ID, RefComment)
	}
	for _, a := range s.Asks {
		add(a.Owner+"/ask/"+a.ID, RefAsk)
	}
	for _, m := range s.Messages {
		add(m.Issue+"/message/"+m.ID, RefMessage)
	}
	return refs, shared
}

// Issue is the seeded issue of that key, and whether the seed holds one.
func (s *Seed) Issue(key string) (Issue, bool) {
	for _, issue := range s.Issues {
		if issue.Key == key {
			return issue, true
		}
	}
	return Issue{}, false
}

// BlockedBy is the keys of the issues that block the issue of that key, in the seed's order.
func (s *Seed) BlockedBy(key string) []string {
	blockers := []string{}
	for _, link := range s.Links {
		if link.Kind == LinkBlocks && link.To == key {
			blockers = append(blockers, link.From)
		}
	}
	return blockers
}

// check refuses a seed that names an item it does not hold, or holds a value Dispatch would not:
// a status outside the lifecycle, a priority outside 0 to 3, a closed issue with no closed_at, a
// live session that answers nothing.
func (s *Seed) check() error {
	refs, shared := s.refs()
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	for _, ref := range shared {
		fail("two items are both %s", ref)
	}
	is := func(ref string, kinds ...RefKind) bool {
		kind, ok := refs[ref]
		return ok && slices.Contains(kinds, kind)
	}
	notAfterNow := func(what string, at time.Time) {
		if at.IsZero() || at.After(s.Now) {
			fail("%s is %s, which is not a time at or before now (%s)", what, at.Format(time.RFC3339), s.Now.Format(time.RFC3339))
		}
	}
	if s.Now.IsZero() {
		fail("now is not set")
	}
	for _, session := range s.Sessions {
		if !slices.Contains([]string{envoy.StateLive, envoy.StateUnresponsive, envoy.StateGone}, session.State) {
			fail("session %s has the state %q, not live, unresponsive or gone", session.ID, session.State)
		}
		if (session.State == envoy.StateLive) != (session.StatusReply != "") {
			fail("session %s is %s, and a session answers its status exactly when it is live", session.ID, session.State)
		}
		if session.State == envoy.StateGone {
			if !session.LastSeen.IsZero() {
				fail("session %s is gone and has a last_seen, which no read of Dispatch can give", session.ID)
			}
		} else {
			notAfterNow("the last_seen of session "+session.ID, session.LastSeen)
		}
	}
	for _, issue := range s.Issues {
		project, _, _ := strings.Cut(issue.Key, "-")
		if !is(project, RefProject) {
			fail("issue %s is in no project of the seed", issue.Key)
		}
		if !slices.Contains(model.IssueStatuses, issue.Status) {
			fail("issue %s has the status %q, which is not one of Dispatch's", issue.Key, issue.Status)
		}
		if issue.Priority != nil && (*issue.Priority < 0 || *issue.Priority > 3) {
			fail("issue %s has the priority %d, not one from 0 to 3", issue.Key, *issue.Priority)
		}
		if issue.Parent != "" && (issue.Parent == issue.Key || !is(issue.Parent, RefIssue)) {
			fail("issue %s has the parent %q, which is no other issue of the seed", issue.Key, issue.Parent)
		}
		if issue.Assignee != "" && !is(issue.Assignee, RefPerson) {
			fail("issue %s is assigned to %q, who is no person of the seed", issue.Key, issue.Assignee)
		}
		if issue.Route != "" && !is(issue.Route, RefSession) {
			fail("issue %s routes to %q, which is no session of the seed", issue.Key, issue.Route)
		}
		if issue.Claim != nil {
			if !is(issue.Claim.Session, RefSession) {
				fail("issue %s is claimed by %q, which is no session of the seed", issue.Key, issue.Claim.Session)
			}
			notAfterNow("the claim of "+issue.Key, issue.Claim.At)
		}
		for _, holder := range issue.RoleHolders {
			if !is(holder.Session, RefSession) {
				fail("issue %s names %q as its %s, which is no session of the seed", issue.Key, holder.Session, holder.Role)
			}
		}
		notAfterNow("the created_at of "+issue.Key, issue.CreatedAt)
		if (issue.Status == "done") != (issue.ClosedAt != nil) {
			fail("issue %s is %s, and an issue has a closed_at exactly when it is done", issue.Key, issue.Status)
		}
		if issue.ClosedAt != nil {
			notAfterNow("the closed_at of "+issue.Key, *issue.ClosedAt)
			if issue.ClosedAt.Before(issue.CreatedAt) {
				fail("issue %s is closed before it was created", issue.Key)
			}
		}
	}
	for _, a := range s.Artifacts {
		if !is(a.Owner, RefIssue, RefProject) {
			fail("artifact %s belongs to %q, which is no issue or project of the seed", a.Slug, a.Owner)
		}
		if a.Kind != ArtifactDocument && a.Kind != ArtifactFile {
			fail("artifact %s has the kind %q, not document or file", a.Slug, a.Kind)
		}
		if (a.Kind == ArtifactFile) != (a.ContentType != "") {
			fail("artifact %s is a %s, and an artifact has a content type exactly when it is a file", a.Slug, a.Kind)
		}
		if a.Body == "" {
			fail("artifact %s has no body", a.Slug)
		}
		notAfterNow("the created_at of artifact "+a.Slug, a.CreatedAt)
	}
	for _, a := range s.Asks {
		if !is(a.Owner, RefIssue, RefDocument) {
			fail("ask %s is on %q, which is no issue or document of the seed", a.ID, a.Owner)
		}
		if !is(a.AskedBy, RefSession, RefPerson) {
			fail("ask %s is asked by %q, who is no session or person of the seed", a.ID, a.AskedBy)
		}
		if a.Question == "" || len(a.Options) == 0 {
			fail("ask %s needs a question and options", a.ID)
		}
		if a.State != AskOpen && a.State != AskAnswered {
			fail("ask %s has the state %q, not open or answered", a.ID, a.State)
		}
		if (a.State == AskAnswered) != (a.Answer != nil) {
			fail("ask %s is %s, and an ask has an answer exactly when it is answered", a.ID, a.State)
		}
		if a.Answer != nil {
			if !is(a.Answer.By, RefPerson) {
				fail("ask %s is answered by %q, who is no person of the seed", a.ID, a.Answer.By)
			}
			for _, selected := range a.Answer.Selected {
				if !slices.Contains(a.Options, selected) {
					fail("ask %s is answered with %q, which is not one of its options", a.ID, selected)
				}
			}
			notAfterNow("the answer of ask "+a.ID, a.Answer.At)
		}
		notAfterNow("the created_at of ask "+a.ID, a.CreatedAt)
	}
	for _, c := range s.Comments {
		if !is(c.Owner, RefIssue, RefDocument) {
			fail("comment %s is on %q, which is no issue or document of the seed", c.ID, c.Owner)
		}
		if !is(c.Author, RefSession, RefPerson) {
			fail("comment %s is by %q, who is no session or person of the seed", c.ID, c.Author)
		}
		notAfterNow("the created_at of comment "+c.ID, c.CreatedAt)
	}
	for _, m := range s.Messages {
		if !is(m.Issue, RefIssue) {
			fail("message %s is on %q, which is no issue of the seed", m.ID, m.Issue)
		}
		if !is(m.Author, RefSession, RefPerson) {
			fail("message %s is by %q, who is no session or person of the seed", m.ID, m.Author)
		}
		if m.To != "" && !is(m.To, RefSession) {
			fail("message %s is to %q, which is no session of the seed", m.ID, m.To)
		}
		notAfterNow("the created_at of message "+m.ID, m.CreatedAt)
	}
	for _, link := range s.Links {
		if !is(link.From, RefIssue) || !is(link.To, RefIssue) || link.From == link.To {
			fail("a link joins %q and %q, which are not two issues of the seed", link.From, link.To)
		}
		if link.Kind != LinkBlocks && link.Kind != LinkRelates {
			fail("the link from %s to %s has the kind %q, not blocks or relates", link.From, link.To, link.Kind)
		}
	}
	return errors.Join(errs...)
}

// Files holds tasks.json, seed.json and labels.json as committed.
//
//go:embed tasks.json seed.json labels.json
var Files embed.FS

// LoadTasks reads tasks.json from fsys (Files, for the committed tasks). It refuses a field that
// no type here declares, and a kind, mode, level, field, chart source or chart group that is not
// one of the names above.
func LoadTasks(fsys fs.FS) ([]Task, error) {
	var tasks []Task
	if err := decodeFile(fsys, "tasks.json", &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// LoadSeed reads seed.json from fsys (Files, for the committed seed). It refuses a field that no
// type here declares, and a seed that names an item it does not hold or holds a value that
// Dispatch would not (Seed.check).
func LoadSeed(fsys fs.FS) (*Seed, error) {
	var seed Seed
	if err := decodeFile(fsys, "seed.json", &seed); err != nil {
		return nil, err
	}
	if err := seed.check(); err != nil {
		return nil, fmt.Errorf("eval: seed.json: %w", err)
	}
	return &seed, nil
}

// decodeFile decodes the one JSON value of the file name into into, refusing unknown fields and
// anything after the value.
func decodeFile(fsys fs.FS, name string, into any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("eval: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("eval: %s: %w", name, err)
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("eval: %s holds more than one JSON value", name)
	}
	return nil
}

// decodeName decodes a JSON string into into, refusing a string that is not one of known.
func decodeName[T ~string](data []byte, into *T, known []T, what string) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("a %s is a string: %w", what, err)
	}
	if !slices.Contains(known, T(name)) {
		names := make([]string, len(known))
		for i, k := range known {
			names[i] = string(k)
		}
		return fmt.Errorf("unknown %s %q: a %s is one of %s", what, name, what, strings.Join(names, ", "))
	}
	*into = T(name)
	return nil
}
