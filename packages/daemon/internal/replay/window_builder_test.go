package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The project every built window is recorded from, its claim tokens' project, and the repository
// each of its issues resolves to.
const (
	builtProject      = "ACME"
	builtTokenProject = "acme"
	builtRepository   = "acme/widgets"
)

// builtLinger is the linger every built window's deployment runs: a root that leaves the workflow
// lingers this long before its tree closes.
const builtLinger = 2 * time.Hour

// builtPerson is the actor of every Dispatch event a person makes in a built window.
var builtPerson = eventActor{Kind: "user", ID: "acme-person"}

// day is the day every built window is recorded on, and clock the moment hh:mm UTC of it.
var day = time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)

func clock(hour, minute int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// windowBuilder writes a recorded window in Go, for a test that needs a small window of its own
// rather than a fixture directory: the window's settings, then each fact of its record in the
// order it happened, through the helpers below, and build to have the Window ReadWindow would
// give for it. Every input is written in the format of window.go: each log line exactly as the
// daemon's JSON handler writes it (slog's JSONHandler: time, level, msg, then the attributes in
// the order the daemon passes them), with the msg and attribute keys of the daemon's own code, and
// each issue's Dispatch events in the shape Dispatch's issue event log serves them, the shape of
// the fixture clock-far-from-now's events, numbered as Dispatch numbers them: a sequence of their
// own for each issue and one id across the project, both in time order.
//
// The recorded daemon ran the rule main, so the helpers that write the daemon's lines write them
// as main writes them — a slot taken back where an issue moves past its review, for one — and a
// window under another rule writes its lines with line.
type windowBuilder struct {
	t      *testing.T
	window Window

	lines  []builtLine
	events []builtEvent
	issues map[string]*builtIssue

	// boots is how many boots the record holds so far, the id of the last one's daemon_boot row.
	boots int
	// deliveries is the last outbox row id a turn's delivery named.
	deliveries int
	// slots is every issue that holds a slot in the record now.
	slots map[string]bool
	// claims is the incarnation of every claim the record has launched, and turns the delivery
	// of each claim's open turn.
	claims map[claim.Token]int
	turns  map[claim.Token]int
}

// builtLine is one line of the record's log at its moment.
type builtLine struct {
	at   time.Time
	line json.RawMessage
}

// builtEvent is one Dispatch event of the record, before build numbers its id.
type builtEvent struct {
	at      time.Time
	key     string
	seq     int64
	kind    string
	actor   eventActor
	payload any
}

// builtIssue is an issue of the record as Dispatch shows it now: what its next issue event's
// payload carries, and the record's own reading of it (its tree and generation).
type builtIssue struct {
	Key       string
	Number    int
	Title     string
	Status    string
	Parent    string
	Rank      string
	Labels    []string
	BlockedBy []string
	Created   time.Time

	tree       string
	generation int
	seq        int64
	last       time.Time
}

// filing is an issue a person files: its fields as the issue.created event carries them, and Tree,
// the tree the recorded daemon records it under, empty for an issue it records as a tree of its
// own or never records.
type filing struct {
	Title     string
	Parent    string
	Tree      string
	Status    string
	Rank      string
	Labels    []string
	BlockedBy []string
}

// eventActor is a Dispatch event's actor.
type eventActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// newWindowBuilder starts the window of project ACME over [from, to): its inputs reach an hour
// before from, its cap is 4 from from on, it ran the rule main all through, it ticks every hour and
// gates no design step, and its other settings are those of the fixture clock-far-from-now.
func newWindowBuilder(t *testing.T, from, to time.Time) *windowBuilder {
	t.Helper()
	return &windowBuilder{
		t: t,
		window: Window{
			Project:           builtProject,
			From:              from,
			To:                to,
			Before:            Duration(time.Hour),
			Caps:              []CapAt{{At: from, Cap: 4}},
			Parts:             []Part{{From: from, To: to, Release: "fixture", Commit: record.MainRule.Commit, Rule: record.MainRule.Name}},
			TickInterval:      Duration(time.Hour),
			GateSteps:         []record.DesignStep{},
			Linger:            Duration(builtLinger),
			ReviewRoundCap:    3,
			StalledTreeNotice: Duration(6 * time.Hour),
			WorkerSilence:     Duration(4 * time.Hour),
			WorkerBackground:  Duration(4 * time.Hour),
			Experiments:       []config.Experiment{},
			Collector:         record.MainRule.Commit,
		},
		issues: make(map[string]*builtIssue),
		slots:  make(map[string]bool),
		claims: make(map[claim.Token]int),
		turns:  make(map[claim.Token]int),
	}
}

// reaches sets how far before the window's start its inputs reach.
func (b *windowBuilder) reaches(before time.Duration) {
	b.window.Before = Duration(before)
}

// capped sets the window's admission cap to limit for all of it.
func (b *windowBuilder) capped(limit int) {
	b.window.Caps = []CapAt{{At: b.window.From, Cap: limit}}
}

// gated sets the design gate steps the window's deployment gates.
func (b *windowBuilder) gated(steps ...record.DesignStep) {
	b.window.GateSteps = steps
}

// capAt is the cap the window's daemon ran with at at: the last change at or before it, or the
// first where at is before every change.
func (b *windowBuilder) capAt(at time.Time) int {
	limit := b.window.Caps[0].Cap
	for _, change := range b.window.Caps {
		if !change.At.After(at) {
			limit = change.Cap
		}
	}
	return limit
}

// build is the window: written into a directory of the test's own with Window.Write and read back
// with ReadWindow, so it is exactly the value a replay of a collected window is given, and every
// Dispatch event decoded with intake.DecodeDispatchEvent first, so an event the daemon would refuse
// fails here rather than in the replay that reads it.
func (b *windowBuilder) build() Window {
	b.t.Helper()
	window := b.window
	slices.SortStableFunc(b.lines, func(x, y builtLine) int { return x.at.Compare(y.at) })
	seen := make(map[string]bool)
	window.Log = make([]json.RawMessage, 0, len(b.lines))
	for _, line := range b.lines {
		window.Log = append(window.Log, line.line)
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(line.line, &head); err != nil {
			b.t.Fatalf("read the msg of the built line %s: %v", line.line, err)
		}
		seen[head.Msg] = true
	}
	window.Lines = make([]string, 0, len(seen))
	for msg := range seen {
		window.Lines = append(window.Lines, msg)
	}
	slices.Sort(window.Lines)

	slices.SortStableFunc(b.events, func(x, y builtEvent) int { return x.at.Compare(y.at) })
	window.Events = make(map[string][]json.RawMessage)
	for i, event := range b.events {
		raw, err := json.Marshal(struct {
			ID         int64      `json:"id"`
			IssueKey   string     `json:"issue_key"`
			ArtifactID *string    `json:"artifact_id"`
			Project    string     `json:"project"`
			Seq        int64      `json:"seq"`
			Type       string     `json:"type"`
			Actor      eventActor `json:"actor"`
			Notify     bool       `json:"notify"`
			CreatedAt  time.Time  `json:"created_at"`
			Payload    any        `json:"payload"`
		}{ID: int64(100 + i + 1), IssueKey: event.key, Project: builtProject, Seq: event.seq, Type: event.kind,
			Actor: event.actor, Notify: true, CreatedAt: event.at, Payload: event.payload})
		if err != nil {
			b.t.Fatalf("write the event of %s at %s: %v", event.key, event.at.Format(time.RFC3339), err)
		}
		if _, _, err := intake.DecodeDispatchEvent(builtProject, event.key, raw); err != nil {
			b.t.Fatalf("the built event %s is no event the daemon decodes: %v", raw, err)
		}
		window.Events[event.key] = append(window.Events[event.key], raw)
	}
	window.Merges = map[string][]Merge{}

	dir := b.t.TempDir()
	if err := window.Write(dir); err != nil {
		b.t.Fatalf("write the built window: %v", err)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		b.t.Fatalf("read the built window back: %v", err)
	}
	return read
}

// line is one line of the record's log at at, as the daemon's JSON handler writes it.
func (b *windowBuilder) line(at time.Time, level slog.Level, msg string, args ...any) {
	b.t.Helper()
	var written bytes.Buffer
	entry := slog.NewRecord(at, level, msg, 0)
	entry.Add(args...)
	if err := slog.NewJSONHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug}).Handle(context.Background(), entry); err != nil {
		b.t.Fatalf("write the line %q: %v", msg, err)
	}
	b.lines = append(b.lines, builtLine{at: at, line: json.RawMessage(bytes.TrimSuffix(written.Bytes(), []byte("\n")))})
}

// info is line at level Info, the level of nearly every line a window holds.
func (b *windowBuilder) info(at time.Time, msg string, args ...any) {
	b.t.Helper()
	b.line(at, slog.LevelInfo, msg, args...)
}

// boot is a daemon process starting at at: the first line of the store's opening, which every boot
// writes (internal/daemon's run).
func (b *windowBuilder) boot(at time.Time) {
	b.t.Helper()
	b.boots++
	b.info(at, "daemon: opening the store; a connection error that the next attempt can clear is tried again",
		"attempts", 10, "attemptLimit", "10s", "firstWait", "1s", "longestWait", "16s", "longestTotal", "3m15s")
}

// stopped is the daemon process of the last boot stopping at at (internal/daemon's run).
func (b *windowBuilder) stopped(at time.Time) {
	b.t.Helper()
	b.info(at, "legion daemon stopped", "project", builtProject, "boot", b.boots)
}

// issue is key's issue, which a test must have filed.
func (b *windowBuilder) issue(key string) *builtIssue {
	b.t.Helper()
	issue, filed := b.issues[key]
	if !filed {
		b.t.Fatalf("%s is not filed in the built window", key)
	}
	return issue
}

// tree is the tree the record holds key under.
func (b *windowBuilder) tree(key string) string {
	b.t.Helper()
	return b.issue(key).tree
}

// generation is the generation the record holds key at.
func (b *windowBuilder) generation(key string) int {
	b.t.Helper()
	return b.issue(key).generation
}

// seq is the sequence of key's last Dispatch event so far.
func (b *windowBuilder) seq(key string) int64 {
	b.t.Helper()
	return b.issue(key).seq
}

// artifact is the id of key's primary artifact, its design document.
func (b *windowBuilder) artifact(key string) string {
	b.t.Helper()
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xa0+b.issue(key).Number)
}

// event is one Dispatch event of key at at, by actor: it takes key's next sequence. Each issue's
// events must be written in time order.
func (b *windowBuilder) event(at time.Time, key, kind string, actor eventActor, payload any) {
	b.t.Helper()
	issue := b.issue(key)
	if at.Before(issue.last) {
		b.t.Fatalf("an event of %s at %s is written after one at %s", key, at.Format(time.RFC3339), issue.last.Format(time.RFC3339))
	}
	issue.seq++
	issue.last = at
	b.events = append(b.events, builtEvent{at: at, key: key, seq: issue.seq, kind: kind, actor: actor, payload: payload})
}

// issueEvent is an issue event of key at at, by actor, carrying the issue as it stands: the
// whole payload Dispatch's issue events carry, as the fixture clock-far-from-now's do.
func (b *windowBuilder) issueEvent(at time.Time, key, kind string, actor eventActor) {
	b.t.Helper()
	issue := b.issue(key)
	var parent *string
	if issue.Parent != "" {
		parent = &issue.Parent
	}
	var closed *time.Time
	if issue.Status == record.DoneStatus {
		closed = &at
	}
	// The issue's next sequence is the one this event takes, which the payload's last_seq names.
	next := issue.seq + 1
	payload := struct {
		Assignee      *string    `json:"assignee"`
		BlockedBy     []string   `json:"blocked_by"`
		Claim         *string    `json:"claim"`
		ClosedAt      *time.Time `json:"closed_at"`
		Components    any        `json:"components"`
		CreatedAt     time.Time  `json:"created_at"`
		CreatedBy     any        `json:"created_by"`
		ExternalLinks []string   `json:"external_links"`
		Key           string     `json:"key"`
		Labels        []string   `json:"labels"`
		LastSeq       int64      `json:"last_seq"`
		Number        int        `json:"number"`
		Parent        *string    `json:"parent"`
		Primary       string     `json:"primary_artifact_id"`
		Priority      *string    `json:"priority"`
		Project       string     `json:"project"`
		Rank          string     `json:"rank"`
		Repo          *string    `json:"repo"`
		ResolvedRepo  string     `json:"resolved_repo"`
		Route         *string    `json:"route"`
		Status        string     `json:"status"`
		Title         string     `json:"title"`
		UpdatedAt     time.Time  `json:"updated_at"`
	}{
		BlockedBy: append([]string{}, issue.BlockedBy...),
		ClosedAt:  closed,
		Components: map[string]any{"ids": []string{}, "inherited_from": nil, "mode": "inherit", "reason": nil,
			"unknown": []string{}},
		CreatedAt:     issue.Created,
		CreatedBy:     map[string]string{"id": builtPerson.ID, "kind": builtPerson.Kind},
		ExternalLinks: []string{},
		Key:           key,
		Labels:        append([]string{}, issue.Labels...),
		LastSeq:       next,
		Number:        issue.Number,
		Parent:        parent,
		Primary:       b.artifact(key),
		Project:       builtProject,
		Rank:          issue.Rank,
		ResolvedRepo:  builtRepository,
		Status:        issue.Status,
		Title:         issue.Title,
		UpdatedAt:     at,
	}
	b.event(at, key, kind, actor, payload)
}

// file is a person filing key at at (issue.created). The record holds it under the tree filing
// names, or as a tree of its own.
func (b *windowBuilder) file(at time.Time, key string, f filing) {
	b.t.Helper()
	if _, filed := b.issues[key]; filed {
		b.t.Fatalf("%s is filed twice", key)
	}
	var number int
	if _, err := fmt.Sscanf(key, builtProject+"-%d", &number); err != nil {
		b.t.Fatalf("%s is no key of project %s: %v", key, builtProject, err)
	}
	tree := f.Tree
	if tree == "" {
		tree = key
	}
	b.issues[key] = &builtIssue{Key: key, Number: number, Title: f.Title, Status: f.Status, Parent: f.Parent, Rank: f.Rank,
		Labels: slices.Clone(f.Labels), BlockedBy: slices.Clone(f.BlockedBy), Created: at, tree: tree, generation: 1}
	b.issueEvent(at, key, "issue.created", builtPerson)
}

// edit is a person changing key at at: the change is made to the issue as it stands, and the event
// is issue.closed where the change sets it done and issue.updated otherwise.
func (b *windowBuilder) edit(at time.Time, key string, change func(*builtIssue)) {
	b.t.Helper()
	b.changed(at, key, builtPerson, change)
}

// daemonWrites is the recorded daemon's own status write of key at at, the event its Dispatch
// session authored, which a replay leaves out and makes again from its own status-write row.
func (b *windowBuilder) daemonWrites(at time.Time, key, status string) {
	b.t.Helper()
	b.changed(at, key, eventActor{Kind: "session", ID: dispatch.DaemonSession(key)}, func(issue *builtIssue) { issue.Status = status })
}

// changed is the issue event of a change of key by actor.
func (b *windowBuilder) changed(at time.Time, key string, actor eventActor, change func(*builtIssue)) {
	b.t.Helper()
	change(b.issue(key))
	kind := "issue.updated"
	if b.issue(key).Status == record.DoneStatus {
		kind = "issue.closed"
	}
	b.issueEvent(at, key, kind, actor)
}

// labelled is a change that puts the legion label on an issue, and unlabelled one that takes it
// off.
func labelled(issue *builtIssue) {
	issue.Labels = append(slices.DeleteFunc(slices.Clone(issue.Labels), func(label string) bool { return label == dispatch.LegionLabel }), dispatch.LegionLabel)
}

func unlabelled(issue *builtIssue) {
	issue.Labels = slices.DeleteFunc(slices.Clone(issue.Labels), func(label string) bool { return label == dispatch.LegionLabel })
}

// setStatus is a change that sets an issue's status.
func setStatus(to string) func(*builtIssue) {
	return func(issue *builtIssue) { issue.Status = to }
}

// handsOver is the recorded daemon's write of a hand-over at at (workflow's handOver): key set todo
// with the legion label by the daemon's own session, which gives the child to Legion as a tree of
// its own.
func (b *windowBuilder) handsOver(at time.Time, key string) {
	b.t.Helper()
	b.changed(at, key, eventActor{Kind: "session", ID: dispatch.DaemonSession(key)}, func(issue *builtIssue) {
		issue.Status = "todo"
		labelled(issue)
	})
}

// ownTree is the record taking key as a tree of its own at its next generation: a child the linger
// of its closed tree handed to Legion, or an orphan a person handed to it.
func (b *windowBuilder) ownTree(key string) {
	b.t.Helper()
	issue := b.issue(key)
	issue.tree = key
	issue.generation++
}

// slotTaken and slotReleased are the admission pass giving key a slot and taking it back at at
// (admit's logSlot), with the slots in use after the change.
func (b *windowBuilder) slotTaken(at time.Time, key string) {
	b.t.Helper()
	if b.slots[key] {
		b.t.Fatalf("%s takes a slot at %s while it holds one", key, at.Format(time.RFC3339))
	}
	b.slots[key] = true
	b.info(at, "admission: slot taken", "issue", key, "slots", len(b.slots), "cap", b.capAt(at))
}

func (b *windowBuilder) slotReleased(at time.Time, key string) {
	b.t.Helper()
	if !b.slots[key] {
		b.t.Fatalf("%s gives a slot back at %s while it holds none", key, at.Format(time.RFC3339))
	}
	delete(b.slots, key)
	b.info(at, "admission: slot released", "issue", key, "slots", len(b.slots), "cap", b.capAt(at))
}

// token is the claim token of key's role.
func (b *windowBuilder) token(key string, role claim.Role) claim.Token {
	b.t.Helper()
	token, err := claim.NewToken(builtTokenProject, key, role)
	if err != nil {
		b.t.Fatal(err)
	}
	return token
}

// launched is the first launch of key's role's claim at at (supervise's machine); a claim already
// launched launches nothing, unless again asks for a new incarnation of it, as a claim whose
// worker failed is launched again by the retry of its phase.
func (b *windowBuilder) launched(at time.Time, key string, role claim.Role, again bool) {
	b.t.Helper()
	token := b.token(key, role)
	if _, up := b.claims[token]; up && !again {
		return
	}
	b.claims[token] = len(b.claims) + 1
	b.info(at, "supervise: launched", "claim", string(token), "generation", b.generation(key),
		"incarnation", fmt.Sprintf("5e0c1a%02x", len(b.claims)), "resumed", false, "resumedBy", "none")
}

// turnStarts is a turn of key's role starting at at, its claim launched first where it has not
// been; turnEnds ends that turn.
func (b *windowBuilder) turnStarts(at time.Time, key string, role claim.Role) {
	b.t.Helper()
	b.launched(at, key, role, false)
	token := b.token(key, role)
	if _, open := b.turns[token]; open {
		b.t.Fatalf("%s starts a turn at %s while one is open", token, at.Format(time.RFC3339))
	}
	b.deliveries++
	b.turns[token] = b.deliveries
	b.info(at, "supervise: a turn started", "claim", string(token), "generation", b.generation(key),
		"delivery", fmt.Sprintf("outbox:%d", b.deliveries))
}

func (b *windowBuilder) turnEnds(at time.Time, key string, role claim.Role) {
	b.t.Helper()
	token := b.token(key, role)
	delivery, open := b.turns[token]
	if !open {
		b.t.Fatalf("%s ends a turn at %s while none is open", token, at.Format(time.RFC3339))
	}
	delete(b.turns, token)
	b.info(at, "supervise: a turn ended", "claim", string(token), "generation", b.generation(key),
		"delivery", fmt.Sprintf("outbox:%d", delivery))
}

// claimFailed is key's role's claim failing at at, its worker having died three times (supervise's
// machine): the open turn ends with it and writes no line of its own.
func (b *windowBuilder) claimFailed(at time.Time, key string, role claim.Role) {
	b.t.Helper()
	token := b.token(key, role)
	delete(b.turns, token)
	b.line(at, slog.LevelError, "supervise: claim failed", "claim", string(token), "cause", "died",
		"why", "the worker's process died three times", "launchFailures", 0, "deaths", 3, "promptFailures", 0, "promptRetires", 0)
}

// moved is the record's move of key from one phase to another by trigger at at
// (workflow.(*Engine).transition), with the status the move gives it, and the daemon's write of
// that status to Dispatch a second later where the status changes.
func (b *windowBuilder) moved(at time.Time, key string, from, to phase.Phase, trigger workflow.TriggerKind) {
	b.t.Helper()
	issue := b.issue(key)
	written := workflow.PhaseStatus(to)
	shown := issue.Status
	if written == "" {
		written = shown
	}
	b.info(at, "workflow: phase changed", "issue", key, "tree", issue.tree, "from", string(from), "to", string(to),
		"trigger", string(trigger), "status", written)
	if written != shown {
		b.daemonWrites(at.Add(time.Second), key, written)
	}
}

// registered is key's architect registering its design at at, version 1 of its spec, and open
// whether that version stands approved (workflow's gateRegistered).
func (b *windowBuilder) registered(at time.Time, key string, open bool) {
	b.t.Helper()
	gated := make([]string, len(b.window.GateSteps))
	for i, step := range b.window.GateSteps {
		gated[i] = string(step)
	}
	b.info(at, "workflow: design gate registered", "issue", key, "artifact", b.artifact(key), "step", string(record.DesignStepSpec),
		"version", 1, "open", open, "changes_requested", false, "gated_steps", gated)
}

// specWritten is key's architect writing version 1 of its spec at at (artifact.version, by the
// architect's session).
func (b *windowBuilder) specWritten(at time.Time, key string) {
	b.t.Helper()
	architect := eventActor{Kind: "session", ID: string(b.token(key, claim.RoleArchitect))}
	b.event(at, key, "artifact.version", architect, map[string]any{
		"artifact_id": b.artifact(key), "diff": "", "name": "spec.md",
		"version": map[string]any{"authors": []map[string]string{{"id": architect.ID, "kind": architect.Kind}}, "created_at": at,
			"named": true, "number": 1, "summary": "The first version of the spec"},
	})
}

// approves is a person approving version 1 of key's spec at at (artifact.approved), the approval
// request ask answering it, "" for an approval no request asked for.
func (b *windowBuilder) approves(at time.Time, key, ask string) {
	b.t.Helper()
	var askID any
	if ask != "" {
		askID = ask
	}
	b.event(at, key, "artifact.approved", builtPerson, map[string]any{
		"actor": map[string]string{"id": builtPerson.ID, "kind": builtPerson.Kind}, "artifact_id": b.artifact(key),
		"ask_id": askID, "name": "spec.md", "reason": nil, "version": 1,
	})
}

// asksApproval is key's architect asking a person at at to approve version 1 of its spec
// (ask.opened of an approval request), with the ask's id.
func (b *windowBuilder) asksApproval(at time.Time, key, ask string) {
	b.t.Helper()
	architect := eventActor{Kind: "session", ID: string(b.token(key, claim.RoleArchitect))}
	b.event(at, key, "ask.opened", architect, map[string]any{
		"id": ask, "issue_key": key, "author": architect, "kind": record.AskApproval, "state": "open",
		"approval": map[string]any{"artifact_id": b.artifact(key), "version": 1, "requested_version": 1},
	})
}

// rootPromoted is the admission pass promoting key, a root waiting in the line, at at: its slot,
// its architect's first turn, and the in_progress the promotion writes to Dispatch a second later.
func (b *windowBuilder) rootPromoted(at time.Time, key string) {
	b.t.Helper()
	b.slotTaken(at, key)
	b.turnStarts(at, key, claim.RoleArchitect)
	b.daemonWrites(at.Add(time.Second), key, "in_progress")
}

// rootDesigned is key's architect ending its first turn at at with its design registered and its
// gate open (a deployment that gates no step opens it at the registration), and the root's own
// first phase starting in the same pass, as a root with no open child's does.
func (b *windowBuilder) rootDesigned(at time.Time, key string) {
	b.t.Helper()
	b.turnEnds(at, key, claim.RoleArchitect)
	b.registered(at, key, true)
	b.rootFirstPhase(at, key)
}

// rootFirstPhase is key, a root whose gate stands open, moving into planning at at by gate_opened
// (workflow.(*Engine).StartFirstPhase), and its planner's first turn.
func (b *windowBuilder) rootFirstPhase(at time.Time, key string) {
	b.t.Helper()
	b.moved(at, key, phase.Admitted, phase.Planning, workflow.TriggerGateOpened)
	b.info(at, "workflow: the root of this tree takes its slot and starts its own first phase", "issue", key)
	b.turnStarts(at, key, claim.RolePlanner)
}

// childStarts is the admission pass starting key, a child at admitted, at at: its slot, its move
// into planning by gate_opened, the line saying why it starts now (the issues it waited on, none
// where it waited on none), its own architect's plain start and its planner's first turn
// (workflow.(*Engine).StartFirstPhase).
func (b *windowBuilder) childStarts(at time.Time, key string, waited ...string) {
	b.t.Helper()
	b.slotTaken(at, key)
	b.moved(at, key, phase.Admitted, phase.Planning, workflow.TriggerGateOpened)
	if len(waited) == 0 {
		b.info(at, "workflow: this child names no issue it waits on any longer, so it starts", "issue", key, "tree", b.tree(key))
	} else {
		b.info(at, "workflow: every issue this child waited on is done, so it starts", "issue", key, "tree", b.tree(key), "blockers", waited)
	}
	b.launched(at, key, claim.RoleArchitect, false)
	b.turnStarts(at, key, claim.RolePlanner)
}

// segmentEnds is the end at at of the work segment of key in from: its role's turn ends, the
// issue moves to to by trigger, the slot follows the rule main (taken back where to keeps none and
// from kept one, taken where to keeps one and from kept none), and the role of to starts its turn.
func (b *windowBuilder) segmentEnds(at time.Time, key string, from, to phase.Phase, trigger workflow.TriggerKind) {
	b.t.Helper()
	b.turnEnds(at, key, phase.RoleFor(from))
	b.moved(at, key, from, to, trigger)
	keptFrom, keepsTo := record.MainRule.PhaseKeepsSlot(from), record.MainRule.PhaseKeepsSlot(to)
	if keptFrom && !keepsTo {
		b.slotReleased(at, key)
	}
	if !keptFrom && keepsTo {
		b.slotTaken(at, key)
	}
	if role := phase.RoleFor(to); role != "" {
		b.turnStarts(at, key, role)
	}
}

// leaves is key leaving the workflow at at because its status became status (workflow's leave):
// the line of it, a root's linger, the slot it held taken back and each open turn of its claims
// ended by their suspend.
func (b *windowBuilder) leaves(at time.Time, key, status string) {
	b.t.Helper()
	tree := b.tree(key)
	b.info(at, "workflow: issue left the workflow", "issue", key, "tree", tree, "status", status)
	if key == tree {
		b.info(at, "workflow: tree lingers", "tree", key, "generation", b.generation(key), "until", at.Add(builtLinger))
	}
	if b.slots[key] {
		b.slotReleased(at, key)
	}
	for _, role := range claim.Roles {
		if _, open := b.turns[b.token(key, role)]; open {
			b.turnEnds(at, key, role)
		}
	}
}

// lingerCloses is the linger of root's tree closing at at with issues members in the tree
// (workflow's lingerExpired).
func (b *windowBuilder) lingerCloses(at time.Time, root string, issues int) {
	b.t.Helper()
	b.info(at, "workflow: linger closed the tree", "tree", root, "generation", b.generation(root), "issues", issues)
}
