package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	// claims is the incarnation of every claim the record has launched, numbered by its launch among
	// all of them (launches), and turns the delivery of each claim's open turn.
	claims   map[claim.Token]int
	launches int
	turns    map[claim.Token]int
}

// builtLine is one line of the record's log at its moment, with the msg it was written under.
type builtLine struct {
	at   time.Time
	msg  string
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
	// Priority is the issue's Dispatch priority, nil for none.
	Priority *int
	// Claim is who holds the issue's Dispatch claim, since when; nil for nobody.
	Claim *builtClaim
	// Assignee is the issue's Dispatch assignee, "" for nobody.
	Assignee string

	tree       string
	generation int
	seq        int64
	last       time.Time
}

// builtClaim is an issue's Dispatch claim as an issue event's payload carries it: its holder, and
// the moment the holder took it.
type builtClaim struct {
	Actor eventActor `json:"actor"`
	At    time.Time  `json:"at"`
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
	Priority  *int
	Assignee  string
}

// eventActor is a Dispatch event's actor, and the origin Dispatch records a session with: nil for a
// person, the daemon's own session and a session named by its claim token, as the fixtures name a
// worker's session.
type eventActor struct {
	Kind   string       `json:"kind"`
	ID     string       `json:"id"`
	Origin *eventOrigin `json:"origin,omitempty"`
}

// eventOrigin is where a session runs as Dispatch records it: the harness, and the title the
// session runs under.
type eventOrigin struct {
	Host         string `json:"host"`
	SessionTitle string `json:"session_title"`
}

// recordedSession is the session id running key's role's claim, as Dispatch records a worker's
// session: by an id of its own, under the title Legion gives the role's session (sessionTitle).
func recordedSession(key string, role claim.Role, id string) eventActor {
	return eventActor{Kind: "session", ID: id, Origin: &eventOrigin{Host: "omp", SessionTitle: sessionTitle(role, key)}}
}

// sessionTitle is the title Legion gives the session of key's role, `Legion <role> · <KEY>`.
func sessionTitle(role claim.Role, key string) string {
	return fmt.Sprintf("Legion %s · %s", role, key)
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
		seen[line.msg] = true
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
		}{ID: int64(100 + i + 1), IssueKey: event.key, Project: builtProjectOf(event.key), Seq: event.seq, Type: event.kind,
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

// builtProjectOf is the Dispatch project key names: the window's own (builtProject) for its issues,
// and another project's for an issue of that project, which the window holds where an issue of its
// own names it, as a blocker the daemon of that project works.
func builtProjectOf(key string) string {
	project, _, _ := strings.Cut(key, "-")
	return project
}

// jsonLine is one line of a daemon's log at at, as the daemon's JSON handler writes it.
func jsonLine(t *testing.T, at time.Time, level slog.Level, msg string, args ...any) json.RawMessage {
	t.Helper()
	var written bytes.Buffer
	entry := slog.NewRecord(at, level, msg, 0)
	entry.Add(args...)
	if err := slog.NewJSONHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug}).Handle(context.Background(), entry); err != nil {
		t.Fatalf("write the line %q: %v", msg, err)
	}
	return bytes.TrimSuffix(written.Bytes(), []byte("\n"))
}

// line is one line of the record's log at at, as the daemon's JSON handler writes it.
func (b *windowBuilder) line(at time.Time, level slog.Level, msg string, args ...any) {
	b.t.Helper()
	b.lines = append(b.lines, builtLine{at: at, msg: msg, line: jsonLine(b.t, at, level, msg, args...)})
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
	// The assignee as it stands now: a later change of the issue's must not reach this event.
	var assignee *string
	if person := issue.Assignee; person != "" {
		assignee = &person
	}
	// The issue's next sequence is the one this event takes, which the payload's last_seq names.
	next := issue.seq + 1
	payload := struct {
		Assignee      *string     `json:"assignee"`
		BlockedBy     []string    `json:"blocked_by"`
		Claim         *builtClaim `json:"claim"`
		ClosedAt      *time.Time  `json:"closed_at"`
		Components    any         `json:"components"`
		CreatedAt     time.Time   `json:"created_at"`
		CreatedBy     any         `json:"created_by"`
		ExternalLinks []string    `json:"external_links"`
		Key           string      `json:"key"`
		Labels        []string    `json:"labels"`
		LastSeq       int64       `json:"last_seq"`
		Number        int         `json:"number"`
		Parent        *string     `json:"parent"`
		Primary       string      `json:"primary_artifact_id"`
		Priority      *int        `json:"priority"`
		Project       string      `json:"project"`
		Rank          string      `json:"rank"`
		Repo          *string     `json:"repo"`
		ResolvedRepo  string      `json:"resolved_repo"`
		Route         *string     `json:"route"`
		Status        string      `json:"status"`
		Title         string      `json:"title"`
		UpdatedAt     time.Time   `json:"updated_at"`
	}{
		Assignee:  assignee,
		BlockedBy: append([]string{}, issue.BlockedBy...),
		Claim:     issue.Claim,
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
		Priority:      issue.Priority,
		Parent:        parent,
		Primary:       b.artifact(key),
		Project:       builtProjectOf(key),
		Rank:          issue.Rank,
		ResolvedRepo:  builtRepository,
		Status:        issue.Status,
		Title:         issue.Title,
		UpdatedAt:     at,
	}
	b.event(at, key, kind, actor, payload)
}

// claimedBy is actor taking key's Dispatch claim at at (issue.claimed), which each later issue
// event of key carries.
func (b *windowBuilder) claimedBy(at time.Time, key string, actor eventActor) {
	b.t.Helper()
	issue := b.issue(key)
	issue.Claim = &builtClaim{Actor: actor, At: at}
	b.event(at, key, "issue.claimed", actor, map[string]any{"claim": *issue.Claim, "key": key, "reason": nil, "repo": nil,
		"resolved_repo": builtRepository, "status": issue.Status})
}

// file is a person filing key at at (issue.created). The record holds it under the tree filing
// names, or as a tree of its own.
func (b *windowBuilder) file(at time.Time, key string, f filing) {
	b.t.Helper()
	b.filedBy(at, key, f, builtPerson)
}

// filedBy is actor filing key at at (issue.created): a person, or an architect's session filing a
// child of its tree. The record holds it under the tree filing names, at that tree's generation, as
// the daemon records a child (workflow's enterChild), or as a tree of its own at its first.
func (b *windowBuilder) filedBy(at time.Time, key string, f filing, actor eventActor) {
	b.t.Helper()
	if _, filed := b.issues[key]; filed {
		b.t.Fatalf("%s is filed twice", key)
	}
	var number int
	if _, err := fmt.Sscanf(key, builtProjectOf(key)+"-%d", &number); err != nil {
		b.t.Fatalf("%s is no issue key: %v", key, err)
	}
	tree, generation := f.Tree, 1
	if tree == "" {
		tree = key
	} else if root, filed := b.issues[tree]; filed {
		generation = root.generation
	}
	b.issues[key] = &builtIssue{Key: key, Number: number, Title: f.Title, Status: f.Status, Parent: f.Parent, Rank: f.Rank,
		Labels: slices.Clone(f.Labels), BlockedBy: slices.Clone(f.BlockedBy), Created: at, Priority: f.Priority, Assignee: f.Assignee, tree: tree,
		generation: generation}
	b.issueEvent(at, key, "issue.created", actor)
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

// daemonPosts is the recorded daemon's post of body on key at at (message.created by its own
// session), as its outbox posts a status write's reason before the write (internal/daemon's
// outbox.status).
func (b *windowBuilder) daemonPosts(at time.Time, key, body string) {
	b.t.Helper()
	b.event(at, key, "message.created", eventActor{Kind: "session", ID: dispatch.DaemonSession(key)},
		map[string]any{"id": fmt.Sprintf("00000000-0000-4000-9000-%012x", b.issue(key).seq+1), "body": body, "body_truncated": false})
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
	unlabelled(issue)
	issue.Labels = append(issue.Labels, dispatch.LegionLabel)
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
	b.info(at, "admission: slot taken", "issue", key, "slots", len(b.slots), "cap", capAt(b.window, at))
}

func (b *windowBuilder) slotReleased(at time.Time, key string) {
	b.t.Helper()
	if !b.slots[key] {
		b.t.Fatalf("%s gives a slot back at %s while it holds none", key, at.Format(time.RFC3339))
	}
	delete(b.slots, key)
	b.info(at, "admission: slot released", "issue", key, "slots", len(b.slots), "cap", capAt(b.window, at))
}

// heldWithNoLine is key holding a slot the record writes no slot line of (assumption 34): one it
// took by a way that writes none, or one held from before the window's inputs reach.
func (b *windowBuilder) heldWithNoLine(key string) {
	b.t.Helper()
	if b.slots[key] {
		b.t.Fatalf("%s holds a slot with no line while it holds one", key)
	}
	b.slots[key] = true
}

// waitEnded and workCameBack are the admission pass giving key a slot back at at in the phase in,
// for a wait of it that ended or for its work that came back.
func (b *windowBuilder) waitEnded(at time.Time, key string, in phase.Phase) {
	b.t.Helper()
	b.info(at, "admission gives a slot back to an issue whose wait ended", "issue", key, "phase", string(in))
	b.slotTaken(at, key)
}

func (b *windowBuilder) workCameBack(at time.Time, key string, in phase.Phase) {
	b.t.Helper()
	b.info(at, "admission gives a slot back to an issue whose work came back", "issue", key, "phase", string(in))
	b.slotTaken(at, key)
}

// waitsForChildren is the admission pass's line of key waiting at at for the children open under
// it (admit's tellChildrenFirst), at the tree and generation the record holds key at.
func (b *windowBuilder) waitsForChildren(at time.Time, key string, children int) {
	b.t.Helper()
	b.info(at, waitsForChildrenLine, "issue", key, "tree", b.tree(key), "generation", b.generation(key), "children", children)
}

// builtToken is the claim token of key's role in a built window.
func builtToken(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(builtTokenProject, key, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// token is the claim token of key's role.
func (b *windowBuilder) token(key string, role claim.Role) claim.Token {
	b.t.Helper()
	return builtToken(b.t, key, role)
}

// session is the session of key's role's claim as the fixtures name a worker's session: by its claim
// token.
func (b *windowBuilder) session(key string, role claim.Role) eventActor {
	b.t.Helper()
	return eventActor{Kind: "session", ID: string(b.token(key, role))}
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
	b.launches++
	b.claims[token] = b.launches
	b.info(at, "supervise: launched", "claim", string(token), "generation", b.generation(key),
		"incarnation", fmt.Sprintf("5e0c1a%02x", b.claims[token]), "resumed", false, "resumedBy", "none")
}

// launchedBefore is key's role's claim standing launched from before the window's inputs reach: the
// log holds no launch of it, and its turns launch nothing.
func (b *windowBuilder) launchedBefore(key string, role claim.Role) {
	b.t.Helper()
	b.launches++
	b.claims[b.token(key, role)] = b.launches
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
	b.claimFailedAs(at, key, role, "died", "the worker's process died three times")
}

// claimFailedAs is claimFailed written with cause and why, or, where cause is "", with no cause, as
// the releases before 97519812 wrote it.
func (b *windowBuilder) claimFailedAs(at time.Time, key string, role claim.Role, cause, why string) {
	b.t.Helper()
	token := b.token(key, role)
	delete(b.turns, token)
	args := []any{"claim", string(token)}
	if cause != "" {
		args = append(args, "cause", cause)
	}
	args = append(args, "why", why, "launchFailures", 0, "deaths", 3, "promptFailures", 0, "promptRetires", 0)
	b.line(at, slog.LevelError, "supervise: claim failed", args...)
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
	b.info(at, phaseChangedLine, "issue", key, "tree", issue.tree, "from", string(from), "to", string(to),
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

// gateOpened and gateClosed are the daemon's lines of key's design gate opening and closing at at
// (workflow's applyArtifact), at the artifact event of kind on version of its spec that flipped it.
func (b *windowBuilder) gateOpened(at time.Time, key string, kind intake.DispatchArtifactKind, version int) {
	b.t.Helper()
	b.info(at, gateOpenedLine, "issue", key, "artifact", b.artifact(key), "event", string(kind), "version", version)
}

func (b *windowBuilder) gateClosed(at time.Time, key string, kind intake.DispatchArtifactKind, version int) {
	b.t.Helper()
	b.info(at, gateClosedLine, "issue", key, "artifact", b.artifact(key), "event", string(kind), "version", version)
}

// specWritten is key's architect writing version 1 of its spec at at (artifact.version, by the
// architect's session, named by its claim token).
func (b *windowBuilder) specWritten(at time.Time, key string) {
	b.t.Helper()
	b.specVersionBy(at, key, b.session(key, claim.RoleArchitect), 1)
}

// specWrittenBy is specWritten by the architect's session architect.
func (b *windowBuilder) specWrittenBy(at time.Time, key string, architect eventActor) {
	b.t.Helper()
	b.specVersionBy(at, key, architect, 1)
}

// specRevised is specWritten of version, a version after the first.
func (b *windowBuilder) specRevised(at time.Time, key string, version int) {
	b.t.Helper()
	b.specVersionBy(at, key, b.session(key, claim.RoleArchitect), version)
}

// specVersionBy is the architect's session architect writing version of key's spec at at
// (artifact.version), summarised as the first version or a later one.
func (b *windowBuilder) specVersionBy(at time.Time, key string, architect eventActor, version int) {
	b.t.Helper()
	summary := "The first version of the spec"
	if version > 1 {
		summary = "A later version of the spec"
	}
	b.event(at, key, "artifact.version", architect, map[string]any{
		"artifact_id": b.artifact(key), "diff": "", "name": "spec.md",
		"version": map[string]any{"authors": []map[string]string{{"id": architect.ID, "kind": architect.Kind}}, "created_at": at,
			"named": true, "number": version, "summary": summary},
	})
}

// approves is a person approving version 1 of key's spec at at (artifact.approved), the approval
// request ask answering it, "" for an approval no request asked for.
func (b *windowBuilder) approves(at time.Time, key, ask string) {
	b.t.Helper()
	b.approvesVersion(at, key, ask, 1)
}

// approvesVersion is approves of version of key's spec.
func (b *windowBuilder) approvesVersion(at time.Time, key, ask string, version int) {
	b.t.Helper()
	var askID any
	if ask != "" {
		askID = ask
	}
	b.event(at, key, "artifact.approved", builtPerson, map[string]any{
		"actor": map[string]string{"id": builtPerson.ID, "kind": builtPerson.Kind}, "artifact_id": b.artifact(key),
		"ask_id": askID, "name": "spec.md", "reason": nil, "version": version,
	})
}

// asksApproval is key's architect asking a person at at to approve version 1 of its spec
// (ask.opened of an approval request), with the ask's id, by its session named by its claim token.
func (b *windowBuilder) asksApproval(at time.Time, key, ask string) {
	b.t.Helper()
	b.asksApprovalBy(at, key, ask, b.session(key, claim.RoleArchitect))
}

// asksApprovalBy is asksApproval by the architect's session architect.
func (b *windowBuilder) asksApprovalBy(at time.Time, key, ask string, architect eventActor) {
	b.t.Helper()
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

// rootWaitsForChildren is key's architect ending its first turn at at with its design registered
// and its gate open, and the root, with children open under it, waiting for them and giving its
// slot back (admit's tellChildrenFirst): the counterpart of rootDesigned for a root with open
// children.
func (b *windowBuilder) rootWaitsForChildren(at time.Time, key string, children int) {
	b.t.Helper()
	b.turnEnds(at, key, claim.RoleArchitect)
	b.registered(at, key, true)
	b.waitsForChildren(at, key, children)
	b.slotReleased(at, key)
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

// through is the ends of key's work segments by moves in turn, the first in from: each ends the
// segment of the phase the move before it entered (segmentEnds).
func (b *windowBuilder) through(key string, from phase.Phase, moves ...builtMove) {
	b.t.Helper()
	for _, move := range moves {
		b.segmentEnds(move.at, key, from, move.to, move.trigger)
		from = move.to
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

// ranOwnTreeWindow is a window from 10:00 to 12:00 under a cap of 2, its inputs reaching back to
// the daemon's boot at 05:00. ACME-1, a root at rank a1, holds a slot from its promotion at 05:05
// while its architect designs its tree to the window's end. ACME-3, a root at rank a3 handed to
// Legion at 05:10, is promoted at once and runs as a tree of its own: a person sets it back to
// backlog at 05:40, it leaves the workflow, and its tree lingers until the linger closes it at
// 07:40. At 10:30 a person sets ACME-3 back to todo under ACME-1. The record's daemon adopts no
// root that ran as a tree of its own (workflow's RanAsRoot, which reads the tree lifecycle that
// ACME-3's promotion opened and that its close kept), so ACME-3 queues as its own tree again, at
// its next generation, and the pass promotes it at once.
func ranOwnTreeWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(5 * time.Hour)
	b.capped(2)
	b.boot(clock(5, 0))
	promotedRoot(b, clock(5, 5), "ACME-1", "a1")
	promotedRoot(b, clock(5, 10), "ACME-3", "a3")
	b.edit(clock(5, 40), "ACME-3", setStatus("backlog"))
	b.leaves(clock(5, 40), "ACME-3", "backlog")
	b.lingerCloses(clock(7, 40), "ACME-3", 1)
	b.edit(clock(10, 30), "ACME-3", func(issue *builtIssue) { issue.Status, issue.Parent = "todo", "ACME-1" })
	b.ownTree("ACME-3")
	b.rootPromoted(clock(10, 30), "ACME-3")
	return b.build()
}

// A root that ran as a tree of its own before the window is not adopted into another tree in a
// replay, as the daemon adopts none: the start state holds the closed lifecycle of each tree the
// trace shows promoted before the window (seeder.closedLifecycles), the row the daemon never
// deletes, which workflow's RanAsRoot reads. The test replays ranOwnTreeWindow under main: at
// 10:30 ACME-3 joins no tree and takes the free slot as a tree of its own, as the record shows.
// It fails where the start state opens no lifecycle for a tree that is closed at the window's
// start, for which the replay's daemon finds no run of ACME-3's own, adopts it into ACME-1's tree
// at 10:30, and holds it there behind ACME-1's design.
func TestARootThatRanAsATreeOfItsOwnBeforeTheWindowIsNotAdopted(t *testing.T) {
	_, log := knobRun(t, ranOwnTreeWindow(t), mainRule(t), "main")
	joined := linesAt(log, func(line replayLine) bool {
		return line.Msg == "workflow: a queued root joined a live tree" && line.Issue == "ACME-3"
	})
	if len(joined) > 0 {
		t.Errorf("the replay adopted ACME-3 into a live tree at %s, want it kept as a tree of its own: it ran as one before the window",
			describeTimes(joined))
	}
	if given := slotsGiven(log); !slices.Contains(given, "ACME-3 at 10:30") {
		t.Errorf("the replay gave the slots %v, want ACME-3's at 10:30, its promotion as a tree of its own", given)
	}
}

// The issues of each window of TestATreeLosesTheRankOfAStartAHoldKeepsBack, by their part in it:
// the root that holds a slot until 11:00 in the replay or in the record; tree A's root, its child
// whose recorded first start the hold keeps back, and a second child of it in backlog, which keeps
// the root waiting for its children; and tree B's root and the child the record starts after
// tree A's.
const (
	heldHolder = "ACME-1"
	heldRootA  = "ACME-2"
	heldChildA = "ACME-3"
	heldRootB  = "ACME-5"
	heldChildB = "ACME-6"
	heldOtherA = "ACME-7"
)

// heldAsk is the id of the approval request tree A's architect opens in rootAwaitingHeldWindow.
const heldAsk = "00000000-0000-4000-8000-0000000000b7"

// builtChild is a child of a built tree: its key and its rank.
type builtChild struct {
	key, rank string
}

// waitingTree is root, a root at rank promoted at at, its children filed in backlog under it 5
// minutes later, and its architect's design 15 minutes in, after which the root waits for those
// children and gives its slot back. In a deployment that gates the spec its architect writes
// version 1 at 6 minutes, and a person approves it at 8.
func waitingTree(b *windowBuilder, at time.Time, root, rank string, children ...builtChild) {
	b.t.Helper()
	promotedRoot(b, at, root, rank)
	for _, child := range children {
		b.file(at.Add(5*time.Minute), child.key, filing{Title: "Ship the " + child.key + " change", Parent: root, Tree: root,
			Status: "backlog", Rank: child.rank})
	}
	if len(b.window.GateSteps) > 0 {
		b.specWritten(at.Add(6*time.Minute), root)
		b.approves(at.Add(8*time.Minute), root, "")
	}
	b.rootWaitsForChildren(at.Add(15*time.Minute), root, len(children))
}

// heldStart starts a window of TestATreeLosesTheRankOfAStartAHoldKeepsBack from 09:00 to 12:00
// under a cap of limit, its inputs reaching back to the daemon's boot at 06:00, recorded under
// keep-past-review, in a deployment that gates the spec where gated is set. Its record is written
// in time order wherever a slot moves (heldTreeB, holderToReview, holderMerges), since each slot
// line carries the slots in use once it is written.
func heldStart(t *testing.T, limit int, gated bool) *windowBuilder {
	t.Helper()
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.window.Parts[0].Rule = "keep-past-review"
	b.reaches(3 * time.Hour)
	b.capped(limit)
	if gated {
		b.gated(record.DesignStepSpec)
	}
	b.boot(clock(6, 0))
	return b
}

// heldTreeB is tree B: heldRootB, a root at rank a2, is promoted at 06:30 and waits for its one
// child, heldChildB at rank b2, which a person releases to todo at 09:10.
func heldTreeB(b *windowBuilder) {
	b.t.Helper()
	waitingTree(b, clock(6, 30), heldRootB, "a2", builtChild{heldChildB, "b2"})
	b.edit(clock(9, 10), heldChildB, setStatus("todo"))
}

// holderToReview is heldHolder, a root at rank a0, promoted at 08:00 and working to its review
// from 09:30; in a deployment that gates the spec, its architect writes version 1 at 08:05 and a
// person approves it at 08:10.
func holderToReview(b *windowBuilder) {
	b.t.Helper()
	promotedRoot(b, clock(8, 0), heldHolder, "a0")
	if len(b.window.GateSteps) > 0 {
		b.specWritten(clock(8, 5), heldHolder)
		b.approves(clock(8, 10), heldHolder, "")
	}
	b.rootDesigned(clock(8, 15), heldHolder)
	b.through(heldHolder, phase.Planning, moveAt(8, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(9, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 30, phase.Reviewing, workflow.TriggerTesterPassed))
}

// holderMerges is heldHolder's review approving it into accepting at 10:15, past which it keeps
// its slot, as keep-past-review does, to its merge at 11:00, where the slot comes free. A replay
// under main gives the slot back at the accepting at 10:15.
func holderMerges(b *windowBuilder) {
	b.t.Helper()
	b.pastReviewKept(clock(10, 15), heldHolder, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.pastReviewKept(clock(10, 30), heldHolder, phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.pastReviewKept(clock(10, 45), heldHolder, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.turnEnds(clock(11, 0), heldHolder, claim.RoleMerger)
	b.moved(clock(11, 0), heldHolder, phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(clock(11, 0), heldHolder, record.DoneStatus)
}

// mergesPastReview is key, a child, starting at at and working 5 minutes in each phase to its
// merge, keeping its slot past its review as keep-past-review does: it gives the slot back at its
// merge 35 minutes in.
func mergesPastReview(b *windowBuilder, key string, at time.Time) {
	b.t.Helper()
	minutes := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	b.childStarts(at, key)
	b.through(key, phase.Planning, builtMove{minutes(5), phase.Implementing, workflow.TriggerPlannerCompleted},
		builtMove{minutes(10), phase.Testing, workflow.TriggerImplementationReady}, builtMove{minutes(15), phase.Reviewing, workflow.TriggerTesterPassed})
	b.pastReviewKept(minutes(20), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.pastReviewKept(minutes(25), key, phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	b.pastReviewKept(minutes(30), key, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.turnEnds(minutes(35), key, claim.RoleMerger)
	b.moved(minutes(35), key, phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	b.leaves(minutes(35), key, record.DoneStatus)
}

// waitingTreeHeldWindow is heldStart under a cap of 1 with tree A waiting for its children:
// heldRootA, a root at rank a1 promoted at 06:05, waits for heldChildA at rank b1 and heldOtherA
// at rank b7, filed in backlog, and a person releases heldChildA to todo at 09:05; then tree B
// (heldTreeB) and heldHolder (holderToReview, holderMerges), which holds the one slot from 08:00
// to 11:00. hold writes a hold on heldChildA from 10:00 to its end at 10:50. At 11:00 heldHolder's
// slot goes to heldChildA, which merges at 11:35 (mergesPastReview), and its slot then goes to
// heldChildB. heldOtherA, still in backlog, keeps heldRootA waiting for its children all the while.
func waitingTreeHeldWindow(t *testing.T, hold func(b *windowBuilder)) Window {
	t.Helper()
	b := heldStart(t, 1, false)
	waitingTree(b, clock(6, 5), heldRootA, "a1", builtChild{heldChildA, "b1"}, builtChild{heldOtherA, "b7"})
	heldTreeB(b)
	holderToReview(b)
	b.edit(clock(9, 5), heldChildA, setStatus("todo"))
	hold(b)
	holderMerges(b)
	mergesPastReview(b, heldChildA, clock(11, 0))
	b.childStarts(clock(11, 35), heldChildB)
	return b.build()
}

// blockerHeldWindow is waitingTreeHeldWindow with a blocker: ACME-4, an issue Legion is not
// given, stands in backlog from 06:00; at 10:00 a person links heldChildA as blocked by it, and at
// 10:50 closes it.
func blockerHeldWindow(t *testing.T) Window {
	return waitingTreeHeldWindow(t, func(b *windowBuilder) {
		b.file(clock(6, 0), "ACME-4", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "c4"})
		b.edit(clock(10, 0), heldChildA, func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-4"} })
		b.edit(clock(10, 50), "ACME-4", setStatus(record.DoneStatus))
	})
}

// openChildrenHeldWindow is waitingTreeHeldWindow with open children: at 10:00 a person files
// ACME-8 todo under heldChildA, which waits for it (children first), and at 10:50 moves it to
// icebox, where it leaves the workflow.
func openChildrenHeldWindow(t *testing.T) Window {
	return waitingTreeHeldWindow(t, func(b *windowBuilder) {
		b.file(clock(10, 0), "ACME-8", filing{Title: "Name the header row's columns", Parent: heldChildA, Tree: heldRootA, Status: "todo", Rank: "b8"})
		b.waitsForChildren(clock(10, 0), heldChildA, 1)
		b.edit(clock(10, 50), "ACME-8", setStatus("icebox"))
		b.leaves(clock(10, 50), "ACME-8", "icebox")
	})
}

// designGateHeldWindow is heldStart under a cap of 2, in a deployment that gates the spec, with
// tree A's design gate closed. heldRootA, a root at rank a1, is promoted at 06:05; its architect
// writes version 1 of the spec at 06:10, a person approves it at 06:15, and the root works its own
// first phase from 06:20 and implements from 06:50 to the window's end, holding its slot. Then tree
// B (heldTreeB) and heldHolder (holderToReview), which takes the second slot at 08:00. At 09:05 a
// person files heldChildA, at rank b1, todo under heldRootA: no slot is free. At 10:00 heldRootA's
// architect writes version 2 of the spec, which closes the gate, and at 10:50 a person approves it,
// which opens the gate again. At 11:00 heldHolder's slot (holderMerges) goes to heldChildA, which
// merges at 11:35 (mergesPastReview), and its slot then goes to heldChildB.
func designGateHeldWindow(t *testing.T) Window {
	b := heldStart(t, 2, true)
	promotedRoot(b, clock(6, 5), heldRootA, "a1")
	b.specWritten(clock(6, 10), heldRootA)
	b.approves(clock(6, 15), heldRootA, "")
	b.rootDesigned(clock(6, 20), heldRootA)
	b.segmentEnds(clock(6, 50), heldRootA, phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	heldTreeB(b)
	holderToReview(b)
	b.file(clock(9, 5), heldChildA, filing{Title: "Write the export's header row", Parent: heldRootA, Tree: heldRootA, Status: "todo", Rank: "b1"})
	b.specRevised(clock(10, 0), heldRootA, 2)
	b.gateClosed(clock(10, 0), heldRootA, intake.DispatchArtifactVersion, 2)
	b.approvesVersion(clock(10, 50), heldRootA, "", 2)
	b.gateOpened(clock(10, 50), heldRootA, intake.DispatchArtifactApproved, 2)
	holderMerges(b)
	mergesPastReview(b, heldChildA, clock(11, 0))
	b.childStarts(clock(11, 35), heldChildB)
	return b.build()
}

// rootAwaitingHeldWindow is heldStart under a cap of 2, in a deployment that gates the spec, with
// tree A's root waiting on a person: tree B (heldTreeB) and heldHolder (holderToReview), which
// takes the first slot at 08:00. heldRootA, a root at rank a1 handed to Legion at 09:05, is
// promoted then into the second slot; heldChildA, at rank b1, is filed todo under it at 09:06, and
// heldOtherA in backlog. Its architect writes version 1 of the spec at 09:17, asks a person to
// approve it at 09:18 and registers it at 09:20, which hands it to the person: the root waits on
// them, keeping its slot, from then. At 10:50 the person approves it: the gate opens, the root waits
// for its children and gives its slot back, and heldChildA takes it. At 11:00 heldHolder's slot
// (holderMerges) goes to heldChildB.
func rootAwaitingHeldWindow(t *testing.T) Window {
	b := heldStart(t, 2, true)
	heldTreeB(b)
	holderToReview(b)
	promotedRoot(b, clock(9, 5), heldRootA, "a1")
	b.file(clock(9, 6), heldChildA, filing{Title: "Write the export's header row", Parent: heldRootA, Tree: heldRootA, Status: "todo", Rank: "b1"})
	b.file(clock(9, 6), heldOtherA, filing{Title: "Sort the export by shelf", Parent: heldRootA, Tree: heldRootA, Status: "backlog", Rank: "b7"})
	b.specWritten(clock(9, 17), heldRootA)
	b.asksApproval(clock(9, 18), heldRootA, heldAsk)
	b.turnEnds(clock(9, 20), heldRootA, claim.RoleArchitect)
	b.registered(clock(9, 20), heldRootA, false)
	since := b.seq(heldRootA)
	b.info(clock(9, 20), "workflow: a registration hands a design document to the person", "issue", heldRootA,
		"step", string(record.DesignStepSpec), "version", 1, "since_seq", since)
	b.info(clock(9, 20), "workflow: root waits on a person and keeps its admission slot", "issue", heldRootA, "tree", heldRootA,
		"reason", "spec version 1 is awaiting a person's review", "since_seq", since)
	b.approves(clock(10, 50), heldRootA, heldAsk)
	b.waitsForChildren(clock(10, 50), heldRootA, 2)
	b.gateOpened(clock(10, 50), heldRootA, intake.DispatchArtifactApproved, 1)
	b.info(clock(10, 50), "workflow: a person's review or a newer spec version answered a root's wait", "issue", heldRootA,
		"version", 1, "event", "approved", "ask", "", "waited_version", 1)
	b.slotReleased(clock(10, 50), heldRootA)
	b.childStarts(clock(10, 50), heldChildA)
	holderMerges(b)
	b.childStarts(clock(11, 0), heldChildB)
	return b.build()
}

// acceptedAncestorHeldWindow is heldStart under a cap of 2 with tree A's root accepted. heldRootA,
// a root at rank a1, is promoted at 06:05, works its own phases, keeping its slot past its review,
// and is signed off into retro at 08:40; then tree B (heldTreeB) and heldHolder (holderToReview),
// which takes the second slot at 08:00. ACME-8, a root at rank a3, is handed to Legion at 08:50 and
// waits for a slot all window. At 10:00 a person files heldChildA, at rank b1, todo under heldRootA,
// which stands accepted, so the child waits beneath it; at 10:50 the root's retro sends it back to
// implementing, and it stands accepted no longer. At 11:00 heldHolder's slot (holderMerges) goes to
// heldChildA, which merges at 11:35 (mergesPastReview), and its slot then goes to heldChildB. A
// replay under main gives heldRootA's slot back at its first pass, in retro, and ACME-8 takes it.
func acceptedAncestorHeldWindow(t *testing.T) Window {
	b := heldStart(t, 2, false)
	promotedRoot(b, clock(6, 5), heldRootA, "a1")
	b.rootDesigned(clock(6, 20), heldRootA)
	b.through(heldRootA, phase.Planning, moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(7, 50, phase.Reviewing, workflow.TriggerTesterPassed))
	b.pastReviewKept(clock(8, 20), heldRootA, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.pastReviewKept(clock(8, 40), heldRootA, phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	heldTreeB(b)
	holderToReview(b)
	b.file(clock(8, 50), "ACME-8", filing{Title: "Archive the old exports", Status: "todo", Rank: "a3", Labels: handedToLegion})
	b.file(clock(10, 0), heldChildA, filing{Title: "Write the export's header row", Parent: heldRootA, Tree: heldRootA, Status: "todo", Rank: "b1"})
	b.pastReviewKept(clock(10, 50), heldRootA, phase.Retro, phase.Implementing, workflow.TriggerBackward)
	holderMerges(b)
	mergesPastReview(b, heldChildA, clock(11, 0))
	b.childStarts(clock(11, 35), heldChildB)
	return b.build()
}

// treeNotRunningWindow is a window from 09:00 to 12:00 under a cap of 1, its inputs reaching back
// to the daemon's boot at 06:00, recorded under main, with tree A's tree out of the workflow.
// heldRootA, a root at rank a1 promoted at 06:05, waits for heldChildA at rank b1 and heldOtherA at
// rank b7, filed in backlog; heldRootB, at rank a2 and promoted at 06:30, waits for heldChildB at
// rank b2. heldHolder, a root at rank a0, is promoted at 07:00 and works to its accepting at 10:00,
// where its slot goes back, and merges at 11:00. A person releases heldChildA to todo at 09:05 and
// heldChildB at 09:10. heldChildA takes the slot at 10:00 and works 3 minutes in each phase to its
// merge at 10:21, its slot going back at its accepting at 10:12, where heldChildB takes it. At 10:30
// a person moves heldRootA to backlog: it leaves the workflow and its tree lingers past the
// window's end. A replay under keep-past-review keeps heldHolder's slot from its accepting to its
// merge at 11:00.
func treeNotRunningWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.reaches(3 * time.Hour)
	b.capped(1)
	b.boot(clock(6, 0))
	waitingTree(b, clock(6, 5), heldRootA, "a1", builtChild{heldChildA, "b1"}, builtChild{heldOtherA, "b7"})
	waitingTree(b, clock(6, 30), heldRootB, "a2", builtChild{heldChildB, "b2"})
	promotedRoot(b, clock(7, 0), heldHolder, "a0")
	b.rootDesigned(clock(7, 15), heldHolder)
	b.through(heldHolder, phase.Planning, moveAt(7, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 30, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 0, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 0), heldHolder, record.DoneStatus)
	b.edit(clock(9, 5), heldChildA, setStatus("todo"))
	b.edit(clock(9, 10), heldChildB, setStatus("todo"))
	b.childStarts(clock(10, 0), heldChildA)
	b.through(heldChildA, phase.Planning, moveAt(10, 3, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 6, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 9, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 12, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 15, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 18, phase.Merging, workflow.TriggerRetroCompleted), moveAt(10, 21, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(10, 21), heldChildA, record.DoneStatus)
	b.childStarts(clock(10, 12), heldChildB)
	b.edit(clock(10, 30), heldRootA, setStatus("backlog"))
	b.leaves(clock(10, 30), heldRootA, "backlog")
	return b.build()
}

// A tree whose recorded next first start a hold keeps back in a replay ranks by that start no
// longer: the replay bars a recorded first start while the daemon's own test of the line's starting
// part holds it (record.FirstPhaseStartsNow), and ranks the tree's root again when that answer
// changes, so another tree's later recorded start takes its place. Each row is one hold that test
// reads, recorded on heldChildA, tree A's next recorded first start, ahead of heldChildB's of tree
// B, while a replay frees a slot that the record did not free then: the tree not running, the root
// waiting on a person, the design gate closed, a blocker, open children and an accepted ancestor.
// In each the replay's daemon records heldRootA at a rank below heldRootB's at its last settle
// before that slot comes free (at), the ranks the pass that gives it out orders the line by, and
// the slot goes to heldChildB. Every row fails where the replay bars a start by the issue's own
// record alone (record.FirstPhaseMayStart), for which heldRootA keeps heldChildA's rank through the
// hold, and a later issue of tree A that can start, such as the open child of the row of open
// children, goes ahead of heldChildB.
func TestATreeLosesTheRankOfAStartAHoldKeepsBack(t *testing.T) {
	dsn := replayDSN(t)
	for _, c := range []struct {
		hold   string
		window func(*testing.T) Window
		rule   string
		at     time.Time
	}{
		{hold: "the tree not running", window: treeNotRunningWindow, rule: "keep-past-review", at: clock(11, 0)},
		{hold: "the root waiting on a person", window: rootAwaitingHeldWindow, rule: record.MainRule.Name, at: clock(10, 15)},
		{hold: "the design gate closed", window: designGateHeldWindow, rule: record.MainRule.Name, at: clock(10, 15)},
		{hold: "a blocker", window: blockerHeldWindow, rule: record.MainRule.Name, at: clock(10, 15)},
		{hold: "open children", window: openChildrenHeldWindow, rule: record.MainRule.Name, at: clock(10, 15)},
		{hold: "an accepted ancestor", window: acceptedAncestorHeldWindow, rule: record.MainRule.Name, at: clock(10, 15)},
	} {
		t.Run(c.hold, func(t *testing.T) {
			ranks := map[string]string{}
			out := t.TempDir()
			cfg := RunConfig{Window: c.window(t), Rule: replayedRule(t, c.rule), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out}
			cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
				if !at.Before(c.at) {
					return nil
				}
				for _, key := range []string{heldRootA, heldRootB} {
					var rank string
					if err := st.Pool().QueryRow(ctx, `select coalesce((select rank from issues where key = $1), '')`, key).Scan(&rank); err != nil {
						return fmt.Errorf("read the rank of %s: %w", key, err)
					}
					ranks[key] = rank
				}
				return nil
			}
			if _, err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("replay the window under %s: %v", c.rule, err)
			}
			rootA, rootB := record.Issue{Key: heldRootA, Rank: ranks[heldRootA]}, record.Issue{Key: heldRootB, Rank: ranks[heldRootB]}
			if len(ranks) != 2 || !record.RankLess(rootB, rootA) {
				t.Errorf("before %s the replay's daemon records %s at %q and %s at %q, want %s below %s: %s's recorded first start is held",
					c.at.Format("15:04"), heldRootA, ranks[heldRootA], heldRootB, ranks[heldRootB], heldRootA, heldRootB, heldChildA)
			}
			if given, want := slotsGiven(replayLog(t, out)), heldChildB+" at "+c.at.Format("15:04"); !slices.Contains(given, want) {
				t.Errorf("the replay gave the slots %v, want %s", given, want)
			}
		})
	}
}

// acceptingLoopWindow is a window from 10:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00. ACME-2, a root promoted at 07:05, whose architect
// writes version 1 of its spec at 07:15, works 30 minutes in each phase to accepting at 09:20, where
// it stands at the window's start. At 10:30 a conflict moves it back to implementing; its loop is 30
// minutes in implementing and 15 in testing and in reviewing, and it is back in accepting at 11:30.
// Ten minutes into that accepting, at 11:40, its architect writes version 2 of the spec, and it
// signs ACME-2 off to retro at 12:00, from where it moves to merging at 12:15. No stretch begins and
// ends inside the window, so the fitted rate draws no conflict.
func acceptingLoopWindow(t *testing.T) Window {
	return acceptingLoopRecord(t).build()
}

// acceptingLoopRecord is the record of acceptingLoopWindow, unbuilt, for a window that adds to it.
func acceptingLoopRecord(t *testing.T) *windowBuilder {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(3 * time.Hour)
	b.boot(clock(7, 0))
	promotedRoot(b, clock(7, 5), "ACME-2", "a2")
	b.specWritten(clock(7, 15), "ACME-2")
	b.rootDesigned(clock(7, 20), "ACME-2")
	b.through("ACME-2", phase.Planning, moveAt(7, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 50, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 30, phase.Implementing, workflow.TriggerConflict),
		moveAt(11, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(11, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(11, 30, phase.Accepting, workflow.TriggerReviewApproved))
	b.specRevised(clock(11, 40), "ACME-2", 2)
	b.through("ACME-2", phase.Accepting, moveAt(12, 0, phase.Retro, workflow.TriggerSignOff),
		moveAt(12, 15, phase.Merging, workflow.TriggerRetroCompleted))
	return b
}

// A root architect's design document keeps its recorded time where the replay passes over the
// conflict loop before the work it falls in (assumption 37): the replay's daemon leaves a conflict
// in accepting to the merger, the stretch goes on past the recorded loop, and the root's next
// accepting work opens at once, but a version of its design is no act that moves with that work
// (driver.keepDesignTimes), since it opens or closes the tree's gate for every member. The test
// replays acceptingLoopWindow under main: ACME-2's stretch draws no conflict at 10:30, its
// accepting after the loop opens then and signs it off at 11:00, and the version its architect
// wrote at 11:40 enters the replay's copy of Dispatch at 11:40. It fails where the version moves
// with that accepting work, for which it enters at 10:40.
func TestARootArchitectsDesignVersionPastAPassedOverLoopKeepsItsRecordedTime(t *testing.T) {
	var copied *shadow
	out := t.TempDir()
	cfg := RunConfig{Window: acceptingLoopWindow(t), Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay acceptingLoopWindow under main: %v", err)
	}
	signedOff := linesAt(replayLog(t, out), func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-2" && line.To == string(phase.Retro)
	})
	if want := []time.Time{clock(11, 0)}; !slices.EqualFunc(signedOff, want, time.Time.Equal) {
		t.Errorf("ACME-2 moved into retro at %s, want at %s: its stretch goes on past the recorded loop", describeTimes(signedOff), describeTimes(want))
	}
	if applied, want := eventsApplied(t, copied, "ACME-2", "artifact.version"), []time.Time{clock(11, 40)}; !slices.EqualFunc(applied, want, time.Time.Equal) {
		t.Errorf("the copy of Dispatch holds ACME-2's spec versions applied at %s, want at %s, the recorded time", describeTimes(applied),
			describeTimes(want))
	}
}

// acceptingOpenLoopWindow is a window from 10:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00. ACME-2, a root promoted at 07:05, works 30 minutes
// in each phase to accepting at 09:20, where it stands at the window's start. At 10:30 a conflict
// moves it back to implementing, and the record shows no return to accepting: its implementer works
// 30 minutes to testing at 11:00, where its tester is in a turn to the window's end. So the stretch
// keeps its conflict under every rule, whatever the fitted rate gives (assumption 35).
func acceptingOpenLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(3 * time.Hour)
	b.boot(clock(7, 0))
	rootThrough(b, "ACME-2", "a2", clock(7, 5), clock(7, 20), moveAt(7, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 50, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 30, phase.Implementing, workflow.TriggerConflict),
		moveAt(11, 0, phase.Testing, workflow.TriggerImplementationReady))
	return b.build()
}

// A conflict in accepting takes the conflict path of the rule the replay runs (record.SlotRule.Conflict,
// assumption 37). The test replays acceptingOpenLoopWindow, whose conflict at 10:30 the stretch keeps,
// under each of the four paths. Under legion-v20.42.5 ACME-2 moves back to implementing by conflict
// at 10:30, as the table of 958173da moved it, and runs its own loop, its implementer from 10:30 to
// testing at 11:00. Under legion-v19.57.3 it moves back by checks_red at 10:30, as the table of
// 9542c6fb moved it, and runs the same loop. Under legion-v22.14.1 it keeps accepting, and the engine
// asks its implementer to merge the base forward at 10:30, as bafd3618 asked it: the implementer's
// merge forward lasts as the loop's implementing round and lands at 11:00. Under main it keeps
// accepting, the engine asks nobody, and the conflict waits for merging (cybermonklabs/legion#4979),
// which the run counts as a conflict left to the merger. The test fails for a replay that moves
// ACME-2 after 10:00 otherwise or at other moments, asks or lands its merge forward at other moments,
// starts its implementer's turns after 10:00 at other moments, keeps other than the one conflict,
// leaves another count of conflicts to the merger, or refuses a move.
func TestAConflictInAcceptingTakesTheRulesPath(t *testing.T) {
	replayDSN(t)
	window := acceptingOpenLoopWindow(t)
	movedBack := func(trigger workflow.TriggerKind) []fixtureMove {
		return []fixtureMove{{From: phase.Accepting, To: phase.Implementing, Trigger: trigger},
			{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady}}
	}
	for _, c := range []struct {
		rule                         string
		moves                        []fixtureMove
		movedAt, asked, landed, turn []time.Time
		toMerger                     int
	}{
		{rule: "legion-v20.42.5", moves: movedBack(workflow.TriggerConflict), movedAt: []time.Time{clock(10, 30), clock(11, 0)},
			turn: []time.Time{clock(10, 30)}},
		{rule: "legion-v19.57.3", moves: movedBack(workflow.TriggerChecksRed), movedAt: []time.Time{clock(10, 30), clock(11, 0)},
			turn: []time.Time{clock(10, 30)}},
		{rule: "legion-v22.14.1", asked: []time.Time{clock(10, 30)}, landed: []time.Time{clock(11, 0)}, turn: []time.Time{clock(10, 30)}},
		{rule: "main", toMerger: 1},
	} {
		t.Run(c.rule, func(t *testing.T) {
			result, log, _ := replayLoopWindow(t, window, c.rule, 1)
			afterTen := func(line replayLine) bool { return line.Issue == "ACME-2" && line.Time.After(clock(10, 0)) }
			var moves []fixtureMove
			var movedAt []time.Time
			for _, line := range log {
				if line.Msg == phaseChangedLine && afterTen(line) {
					moves = append(moves, fixtureMove{From: phase.Phase(line.From), To: phase.Phase(line.To), Trigger: line.Trigger})
					movedAt = append(movedAt, line.Time)
				}
			}
			if !slices.Equal(moves, c.moves) || !slices.EqualFunc(movedAt, c.movedAt, time.Time.Equal) {
				t.Errorf("ACME-2 moved %s at %s after 10:00, want %s at %s", describeMoves(moves), describeTimes(movedAt),
					describeMoves(c.moves), describeTimes(c.movedAt))
			}
			asked := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardAskedLine && afterTen(line) })
			landed := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardLandedLine && afterTen(line) })
			if !slices.EqualFunc(asked, c.asked, time.Time.Equal) || !slices.EqualFunc(landed, c.landed, time.Time.Equal) {
				t.Errorf("ACME-2's merge forward was asked at %s and landed at %s, want asked at %s and landed at %s", describeTimes(asked),
					describeTimes(landed), describeTimes(c.asked), describeTimes(c.landed))
			}
			implementer := string(builtToken(t, "ACME-2", claim.RoleImplementer))
			turns := linesAt(log, func(line replayLine) bool {
				return line.Msg == turnStartedLine && line.Claim == implementer && line.Time.After(clock(10, 0))
			})
			if !slices.EqualFunc(turns, c.turn, time.Time.Equal) {
				t.Errorf("ACME-2's implementer started turns at %s after 10:00, want at %s", describeTimes(turns), describeTimes(c.turn))
			}
			if result.Divergence.KeptOpenLoops != 1 || result.Divergence.ConflictsToMerger != c.toMerger {
				t.Errorf("the run kept %d conflicts and left %d to the merger, want 1 kept and %d left to the merger",
					result.Divergence.KeptOpenLoops, result.Divergence.ConflictsToMerger, c.toMerger)
			}
			if refused := result.Divergence.Refused; len(refused) > 0 {
				t.Errorf("the replay refused %+v", refused)
			}
		})
	}
}

// phaseCells is the stretches of phaseStretchesWindow by the phase each ends in and whether the
// base branch took a merge during it, with how many of them end in a conflict. In each cell the
// share of conflicts is the chance logistic(base + phase term + merges term × log2(1 + M)) gives,
// at a base of ln(1/9), a testing term of ln 3, a reviewing term of ln 9 and a merges term of ln 3,
// so the fit's estimates are those coefficients exactly.
var phaseCells = []struct {
	in                   phase.Phase
	merged               bool
	stretches, conflicts int
}{
	{phase.Merging, false, 10, 1}, {phase.Merging, true, 4, 1},
	{phase.Testing, false, 4, 1}, {phase.Testing, true, 2, 1},
	{phase.Reviewing, false, 2, 1}, {phase.Reviewing, true, 4, 3},
}

// phaseStretchesWindow is a window from 09:00 to 12:00 recorded under legion-v20.42.5 and a cap of
// 30, its inputs reaching back to the daemon's boot at 08:00, whose roots each end one stretch
// inside it, as phaseCells lists them. Each root, ACME-1 on, is promoted at 08:00 and designed at
// 08:10, and implements from 08:20; its stretch runs from 09:10 to 09:40, where the base branch
// takes no merge, or from 10:10 to 10:40, around the base's one merge at 10:20. A stretch in
// testing ends in the tester's failure or a conflict; one in reviewing, from 10 minutes in, in the
// reviewer's rejection or a conflict; and one in merging, from 20 minutes in, after 5 minutes in
// each phase from reviewing on, in its merge or a conflict.
func phaseStretchesWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(30)
	b.boot(clock(8, 0))
	n := 0
	for _, cell := range phaseCells {
		for i := range cell.stretches {
			n++
			key, conflict := fmt.Sprintf("ACME-%d", n), i < cell.conflicts
			start := clock(9, 10)
			if cell.merged {
				start = clock(10, 10)
			}
			at := func(minutes int) time.Time { return start.Add(time.Duration(minutes) * time.Minute) }
			promotedRoot(b, clock(8, 0), key, fmt.Sprintf("a%02d", n))
			b.rootDesigned(clock(8, 10), key)
			b.through(key, phase.Planning, moveAt(8, 20, phase.Implementing, workflow.TriggerPlannerCompleted),
				builtMove{start, phase.Testing, workflow.TriggerImplementationReady})
			end := builtMove{at(30), phase.Implementing, workflow.TriggerConflict}
			switch cell.in {
			case phase.Testing:
				if !conflict {
					end.trigger = workflow.TriggerTesterFailed
				}
				b.through(key, phase.Testing, end)
			case phase.Reviewing:
				if !conflict {
					end.trigger = workflow.TriggerReviewRejected
				}
				b.through(key, phase.Testing, builtMove{at(10), phase.Reviewing, workflow.TriggerTesterPassed}, end)
			case phase.Merging:
				if !conflict {
					end = builtMove{at(30), phase.Done, workflow.TriggerPullRequestMerged}
				}
				b.through(key, phase.Testing, builtMove{at(5), phase.Reviewing, workflow.TriggerTesterPassed},
					builtMove{at(10), phase.Accepting, workflow.TriggerReviewApproved}, builtMove{at(15), phase.Retro, workflow.TriggerSignOff},
					builtMove{at(20), phase.Merging, workflow.TriggerRetroCompleted}, end)
				if !conflict {
					b.leaves(at(30), key, record.DoneStatus)
				}
			}
		}
	}
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {{SHA: "4e7a1c9b2d5f8036a1b2c3d4e5f60718293a4b5c", At: clock(10, 20), Branch: "main"}}}
	return window
}

// writtenCoefficient is a coefficient as a run's run.json writes it: null for an end with none.
type writtenCoefficient struct {
	Estimate, Low, High *float64
}

// The conflict rate takes a term for each phase a stretch can end in, fitted on the window together
// with its base and its merges term (spec R2, plan D7), and the run's output writes each term with
// its interval. The test reads the conflict model of phaseStretchesWindow, whose stretches end in
// merging, testing and reviewing at shares that put the base at ln(1/9), testing's term at ln 3,
// reviewing's at ln 9 and the merges term at ln 3, as run.json writes it: it holds the 26 stretches
// and their 8 conflicts, the stretches and conflicts of each of the three phases, and the base,
// the merges term and the two phase terms each at its true value with an interval that holds it.
// It fails for a model with no term of a phase, which a rate fitted on the base and the merges term
// alone is, and for a term recovered elsewhere than its true value.
func TestAFittedRateRecoversEachPhasesTermFromBuiltStretches(t *testing.T) {
	window := phaseStretchesWindow(t)
	trace := placedTrace(t, window)
	raw, err := json.Marshal(windowConflicts(trace, newBaseMerges(window, trace)))
	if err != nil {
		t.Fatalf("write the conflict model: %v", err)
	}
	var written struct {
		Stretches, Conflicts int
		Base, Term           writtenCoefficient
		Phases               map[phase.Phase]struct {
			Stretches, Conflicts int
			Term                 *writtenCoefficient
		}
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("read the conflict model back: %v", err)
	}
	if written.Stretches != 26 || written.Conflicts != 8 {
		t.Errorf("the model was fitted on %d stretches, %d of them conflicts; want 26 and 8", written.Stretches, written.Conflicts)
	}
	for _, want := range []struct {
		in                   phase.Phase
		stretches, conflicts int
	}{{phase.Merging, 14, 2}, {phase.Testing, 6, 2}, {phase.Reviewing, 6, 4}} {
		if got := written.Phases[want.in]; got.Stretches != want.stretches || got.Conflicts != want.conflicts {
			t.Errorf("the model counts %d stretches in %s, %d of them conflicts; want %d and %d", got.Stretches, want.in, got.Conflicts,
				want.stretches, want.conflicts)
		}
	}
	ln3 := math.Log(3)
	for _, c := range []struct {
		name  string
		got   *writtenCoefficient
		truth float64
	}{
		{"the base", &written.Base, -2 * ln3},
		{"the merges term", &written.Term, ln3},
		{"testing's term", written.Phases[phase.Testing].Term, ln3},
		{"reviewing's term", written.Phases[phase.Reviewing].Term, 2 * ln3},
	} {
		switch got := c.got; {
		case got == nil:
			t.Errorf("the model's JSON holds no %s", c.name)
		case got.Estimate == nil || got.Low == nil || got.High == nil:
			t.Errorf("the model's JSON holds %s with no estimate or no end of its interval: %s", c.name, raw)
		case math.Abs(*got.Estimate-c.truth) > 1e-6 || *got.Low > c.truth || c.truth > *got.High:
			t.Errorf("%s is %v [%v, %v], want %v inside its interval", c.name, *got.Estimate, *got.Low, *got.High, c.truth)
		}
	}
}

// acceptingLoopChildWindow is acceptingLoopWindow with a child of ACME-2 that holds a slot. ACME-4,
// an issue Legion is not given, stands in backlog from 07:10. ACME-3, which a person files todo
// under ACME-2 at 07:30, starts at 07:40 and implements from 08:00 to the window's end, holding its
// slot. At 11:40, ten minutes into ACME-2's accepting after its loop, ACME-2's architect links
// ACME-3 as blocked by ACME-4, and admission gives ACME-3's slot back for the wait.
func acceptingLoopChildWindow(t *testing.T) Window {
	b := acceptingLoopRecord(t)
	b.file(clock(7, 10), "ACME-4", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "c4"})
	b.file(clock(7, 30), "ACME-3", filing{Title: "Write the export's header row", Parent: "ACME-2", Tree: "ACME-2", Status: "todo", Rank: "b3"})
	b.childStarts(clock(7, 40), "ACME-3")
	b.segmentEnds(clock(8, 0), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.changed(clock(11, 40), "ACME-3", b.session("ACME-2", claim.RoleArchitect), func(issue *builtIssue) { issue.BlockedBy = []string{"ACME-4"} })
	b.slotReleased(clock(11, 40), "ACME-3")
	return b.build()
}

// An event a piece of work's worker makes on another issue keeps its recorded time where the
// replay moves the piece (script.placeEvents): a link it adds or takes off, a child it removes, an
// edit of another issue, unlike a child the worker files under its own issue, whose filing moves
// with the piece's start (spec R2, assumption 17). The test replays acceptingLoopChildWindow under
// main, where ACME-2's stretch draws no conflict at 10:30 and its accepting after the recorded
// loop opens then, an hour before its recorded start: the link its architect adds to ACME-3 at
// 11:40 enters then, and ACME-3 gives its slot back for the wait at 11:40, as recorded. It fails
// where the link moves with the accepting work, for which ACME-3 gives its slot back at 10:40.
func TestAMovedPiecesEventOnAnotherIssueKeepsItsRecordedTime(t *testing.T) {
	_, log := knobRun(t, acceptingLoopChildWindow(t), mainRule(t), "main")
	released := linesAt(log, func(line replayLine) bool { return line.Msg == slotReleasedLine && line.Issue == "ACME-3" })
	if want := []time.Time{clock(11, 40)}; !slices.EqualFunc(released, want, time.Time.Equal) {
		t.Errorf("ACME-3 gave its slot back at %s, want at %s, when ACME-2's architect links it as blocked", describeTimes(released),
			describeTimes(want))
	}
}

// lateRootWindow is a window from 09:00 to 13:00 recorded under main with a cap of 1, its inputs
// reaching back to the daemon's boot at 06:00, in which a root's first slot comes an hour late in a
// replay under keep-past-review. ACME-1, a root at rank a0 promoted at 06:30, works to its review's
// approval at 10:00, where its slot goes back under main, and then through accepting, retro and
// merging to its merge at 11:00. ACME-2, a root at rank a1 handed to Legion at 07:00, waits for the
// slot and is promoted at 10:00, its architect in a turn from then; tail writes what that architect
// does next. Under keep-past-review ACME-1 keeps its slot to its merge, and ACME-2 is promoted at
// 11:00.
func lateRootWindow(t *testing.T, tail func(b *windowBuilder)) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.capped(1)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a0", clock(6, 30), clock(6, 45), moveAt(7, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 0, phase.Accepting, workflow.TriggerReviewApproved))
	b.file(clock(7, 0), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.rootPromoted(clock(10, 0), "ACME-2")
	b.through("ACME-1", phase.Accepting, moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 0), "ACME-1", record.DoneStatus)
	tail(b)
	return b.build()
}

// headerChild is the filing of ACME-3 under ACME-2 at status by ACME-2's architect.
func headerChild(b *windowBuilder, at time.Time, status string) {
	b.t.Helper()
	b.filedBy(at, "ACME-3", filing{Title: "Write the export's header row", Parent: "ACME-2", Tree: "ACME-2", Status: status, Rank: "b3"},
		b.session("ACME-2", claim.RoleArchitect))
}

// replayCopied replays window under rule on the main branch at seed 1 and f 1, and answers the
// replay's log and its copy of Dispatch.
func replayCopied(t *testing.T, window Window, rule string) ([]replayLine, *shadow) {
	t.Helper()
	var copied *shadow
	out := t.TempDir()
	cfg := RunConfig{Window: window, Rule: replayedRule(t, rule), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: replayDSN(t), Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay the window under %s: %v", rule, err)
	}
	return replayLog(t, out), copied
}

// startedAt is the moments key moved from admitted into planning in log: its first phase's
// starts.
func startedAt(log []replayLine, key string) []time.Time {
	return linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == key && line.From == string(phase.Admitted) && line.To == string(phase.Planning)
	})
}

// A child its parent's worker files under the parent enters at its recorded offset from the
// start of the work it is filed in (spec R2, assumption 17), and never vanishes: where that work
// never runs, the filing enters by its anchor, at its recorded time or once the replay knows the
// work cannot run. The test replays lateRootWindow under keep-past-review twice. In the first,
// ACME-2's architect files ACME-3 todo at 10:30, half an hour into its turn, and registers its
// design at 10:45, where ACME-2 waits for its child and ACME-3 starts; ACME-2 is promoted at 11:00,
// so ACME-3 is filed at 11:30 and starts at 11:45. In the second, a person moves ACME-2 to backlog
// at 10:30, right after its architect files ACME-3: in the replay ACME-2 leaves the workflow still
// waiting for its slot, its architect's work never runs, and ACME-3's filing enters at 10:30. The
// second fails where a filing of work that never runs is left with it, for which ACME-3 is never
// filed.
func TestAChildsFilingMovesWithItsParentsStartAndNeverVanishes(t *testing.T) {
	for _, c := range []struct {
		name           string
		tail           func(b *windowBuilder)
		filed, started []time.Time
	}{
		{name: "the parent's work runs late", tail: func(b *windowBuilder) {
			headerChild(b, clock(10, 30), "todo")
			b.rootWaitsForChildren(clock(10, 45), "ACME-2", 1)
			b.childStarts(clock(10, 45), "ACME-3")
		}, filed: []time.Time{clock(11, 30)}, started: []time.Time{clock(11, 45)}},
		{name: "the parent's work never runs", tail: func(b *windowBuilder) {
			headerChild(b, clock(10, 30), "todo")
			b.edit(clock(10, 30), "ACME-2", setStatus("backlog"))
			b.leaves(clock(10, 30), "ACME-2", "backlog")
		}, filed: []time.Time{clock(10, 30)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log, copied := replayCopied(t, lateRootWindow(t, c.tail), "keep-past-review")
			if filed := eventsApplied(t, copied, "ACME-3", "issue.created"); !slices.EqualFunc(filed, c.filed, time.Time.Equal) {
				t.Errorf("ACME-3 was filed in the copy of Dispatch at %s, want at %s", describeTimes(filed), describeTimes(c.filed))
			}
			if started := startedAt(log, "ACME-3"); !slices.EqualFunc(started, c.started, time.Time.Equal) {
				t.Errorf("ACME-3 started at %s, want at %s", describeTimes(started), describeTimes(c.started))
			}
		})
	}
}

// An architect's own writes on a child and the writes of its API that Dispatch records under the
// daemon's session keep their recorded order (driver.placeFollowers): a daemon session's write
// that follows a worker's act on the same issue comes after it, at its recorded offset from it, or
// at its recorded time where that is later. The test replays lateRootWindow under keep-past-review,
// in which ACME-2's architect files ACME-3 in triage at 10:20, its release writes ACME-3 todo under
// the daemon's session at 10:21, and ACME-2 waits for its child from 10:30, where ACME-3 starts.
// ACME-2 is promoted an hour late, so ACME-3 is filed at 11:20 and released at 11:21, and it starts
// at 11:30. It fails where the release keeps its recorded time ahead of the late filing, for which
// the filing's triage stands last, ACME-3 leaves the workflow and never starts.
func TestAnArchitectsReleaseOfAChildKeepsItsOrderWithTheFiling(t *testing.T) {
	log, _ := replayCopied(t, lateRootWindow(t, func(b *windowBuilder) {
		headerChild(b, clock(10, 20), "triage")
		b.daemonWrites(clock(10, 21), "ACME-3", "todo")
		b.rootWaitsForChildren(clock(10, 30), "ACME-2", 1)
		b.childStarts(clock(10, 30), "ACME-3")
	}), "keep-past-review")
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(11, 30)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s: filed at 11:20 and released at 11:21", describeTimes(started), describeTimes(want))
	}
}

// earlyAcceptingWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 06:00. ACME-2, a root promoted at 06:05, works 30 minutes
// in each phase to accepting at 08:20; at 09:20 a conflict moves it back to implementing, and its
// loop of 20 minutes in each phase brings it back to accepting at 10:20, where its architect works
// to the window's end. At 10:30 its architect files ACME-3 in triage under it, at 10:31 the
// architect's release writes ACME-3 todo under the daemon's session, ACME-3 starts at 10:40, and at
// 11:00 the architect edits ACME-3's title, its event showing ACME-3 in progress. No stretch begins
// and ends inside the window, so the fitted rate draws no conflict.
func earlyAcceptingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(3 * time.Hour)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-2", "a2", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(7, 50, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(8, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(9, 20, phase.Implementing, workflow.TriggerConflict),
		moveAt(9, 40, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 0, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 20, phase.Accepting, workflow.TriggerReviewApproved))
	headerChild(b, clock(10, 30), "triage")
	b.daemonWrites(clock(10, 31), "ACME-3", "todo")
	b.childStarts(clock(10, 40), "ACME-3")
	b.changed(clock(11, 0), "ACME-3", b.session("ACME-2", claim.RoleArchitect), func(issue *builtIssue) {
		issue.Title = "Write the export's header row in bold"
	})
	return b.build()
}

// A write of the daemon's own session is judged against the issue's event before it in the record
// (driver.keepsDaemonEvent), not against the last one the replay's copy applied, and a reopen it
// keeps is applied by the replay's own record of the issue. The test replays earlyAcceptingWindow
// under main: ACME-2's stretch draws no conflict at 09:20, and its accepting after the loop opens
// then, an hour before its recorded start, so the architect's filing of ACME-3 enters at 09:30; the
// release's todo at 10:31 is judged against that filing's triage and enters, and ACME-3 starts at
// 10:31, a slot being free. It fails where the release is judged against a later event the copy
// applied first, the architect's title edit carrying ACME-3 in progress, which then stands as a
// session's status and ACME-3 never enters.
func TestADaemonWriteIsJudgedInTheRecordsOrder(t *testing.T) {
	_, log := knobRun(t, earlyAcceptingWindow(t), mainRule(t), "main")
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(10, 31)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s, once its release enters", describeTimes(started), describeTimes(want))
	}
}

// reopenLateWindow is a window from 09:00 to 13:00 recorded under main with a cap of 2, its inputs
// reaching back to the daemon's boot at 06:00. ACME-2, a root promoted at 06:05, implements from
// 06:50 to the window's end, holding a slot. ACME-1, a root promoted at 06:30, holds the other slot
// to its review's approval at 10:00, and merges at 11:00. ACME-3, filed todo under ACME-2 at 06:35,
// takes ACME-1's slot at 10:00 and works 10 minutes in each phase to its merge at 11:10, giving its
// slot back at its accepting at 10:40. At 11:20 the daemon's session writes it todo again, the run
// again of a child out of the workflow, and it starts its second generation on the free slot.
// Under keep-past-review ACME-1 and ACME-3 keep their slots past their reviews: ACME-3 starts at
// 11:00, an hour late, and merges at 12:10.
func reopenLateWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.capped(2)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-2", "a2", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted))
	rootThrough(b, "ACME-1", "a0", clock(6, 30), clock(6, 45), moveAt(7, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 0, phase.Accepting, workflow.TriggerReviewApproved))
	b.file(clock(6, 35), "ACME-3", filing{Title: "Write the export's header row", Parent: "ACME-2", Tree: "ACME-2", Status: "todo", Rank: "b3"})
	b.childStarts(clock(10, 0), "ACME-3")
	b.through("ACME-1", phase.Accepting, moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 0), "ACME-1", record.DoneStatus)
	b.through("ACME-3", phase.Planning, moveAt(10, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 30, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 40, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 50, phase.Retro, workflow.TriggerSignOff),
		moveAt(11, 0, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 10, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 10), "ACME-3", record.DoneStatus)
	b.issue("ACME-3").generation++
	b.daemonWrites(clock(11, 20), "ACME-3", "todo")
	b.childStarts(clock(11, 20), "ACME-3")
	return b.build()
}

// A reopen the daemon's session writes is applied once the replay's own record of the issue stands
// out of the workflow: one that comes while the replay's issue is still in flight waits for its
// leave, and comes at its recorded offset from it. The test replays reopenLateWindow under
// keep-past-review: ACME-3 starts at 11:00 and merges at 12:10, so the reopen recorded at 11:20,
// ten minutes after its recorded merge, comes at 12:20, where ACME-3 takes its second slot, and no
// move is refused. It fails where the reopen is applied while ACME-3's first generation runs,
// which starts the second over it and refuses the first one's moves.
func TestAKeptReopenWaitsForTheReplaysOwnLeave(t *testing.T) {
	result, log := knobRun(t, reopenLateWindow(t), replayedRule(t, "keep-past-review"), "main")
	var given []string
	for _, slot := range slotsGiven(log) {
		if strings.HasPrefix(slot, "ACME-3 ") {
			given = append(given, slot)
		}
	}
	if want := []string{"ACME-3 at 11:00", "ACME-3 at 12:20"}; !slices.Equal(given, want) {
		t.Errorf("the replay gave ACME-3 the slots %v, want %v", given, want)
	}
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("the replay refused %+v", refused)
	}
}

// restartedRootWindow is the window of a root whose current run ran with no slot line, from 10:00
// to 13:00 under a cap of 2, in a deployment that gates the spec, its inputs reaching back to the
// daemon's boot at 05:00. ACME-5, a root promoted at 05:05, works to its review and reviews from
// 06:50 to its approval at 10:10, where its slot goes back. ACME-1, a root promoted at 07:05, is
// restarted at 08:00: the daemon's session writes it backlog, it leaves the workflow, its tree
// lingers, and ACME-4, a root handed to Legion at 07:30, is promoted into its slot and implements
// from 08:45 on; at 08:01 the daemon's session writes ACME-1 todo again. ACME-1's architect is
// launched at 08:30, with no slot line, writes the spec and registers it at 08:40; a person approves
// it at 09:00, which opens the gate, and at 09:10 a person files ACME-2 todo under ACME-1, which
// waits for it. At 10:10 ACME-2 takes ACME-5's slot, and ACME-1's architect works from 10:20 to
// 10:30.
func restartedRootWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.reaches(5 * time.Hour)
	b.capped(2)
	b.gated(record.DesignStepSpec)
	b.boot(clock(5, 0))
	rootThrough(b, "ACME-5", "a0", clock(5, 5), clock(5, 20), moveAt(5, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(6, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(6, 50, phase.Reviewing, workflow.TriggerTesterPassed))
	promotedRoot(b, clock(7, 5), "ACME-1", "a1")
	b.file(clock(7, 30), "ACME-4", filing{Title: "Archive the old exports", Status: "todo", Rank: "a3", Labels: handedToLegion})
	b.daemonWrites(clock(8, 0), "ACME-1", "backlog")
	b.leaves(clock(8, 0), "ACME-1", "backlog")
	b.rootPromoted(clock(8, 0), "ACME-4")
	b.daemonWrites(clock(8, 1), "ACME-1", "todo")
	b.issue("ACME-1").generation++
	b.rootDesigned(clock(8, 15), "ACME-4")
	b.launched(clock(8, 30), "ACME-1", claim.RoleArchitect, true)
	b.turnStarts(clock(8, 30), "ACME-1", claim.RoleArchitect)
	b.specWritten(clock(8, 35), "ACME-1")
	b.turnEnds(clock(8, 40), "ACME-1", claim.RoleArchitect)
	b.registered(clock(8, 40), "ACME-1", false)
	b.segmentEnds(clock(8, 45), "ACME-4", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.approves(clock(9, 0), "ACME-1", "")
	b.gateOpened(clock(9, 0), "ACME-1", intake.DispatchArtifactApproved, 1)
	b.file(clock(9, 10), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "b2"})
	b.waitsForChildren(clock(9, 10), "ACME-1", 1)
	b.segmentEnds(clock(10, 10), "ACME-5", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.childStarts(clock(10, 10), "ACME-2")
	b.turnStarts(clock(10, 20), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(10, 30), "ACME-1", claim.RoleArchitect)
	return b.build()
}

// The start state runs the tree of a root whose current run the record shows running though no
// slot line of that run is in the log (seeder.lifecycles): its design gate opened in the run, or
// its architect was launched afresh in it. The test replays restartedRootWindow under main: ACME-1's
// tree is live from the start, so at 10:10 ACME-2 takes the slot ACME-5 gives back, and ACME-1's
// architect's turn at 10:20 is written, no turn going without a lifecycle. It fails where the start
// state opens no lifecycle for that run, for which ACME-1's tree is closed, ACME-2 is held by no
// word the daemon has and takes no slot, and the turn is counted as one begun with no lifecycle.
func TestATreeThatRanWithNoSlotLineRunsFromTheStart(t *testing.T) {
	result, log := knobRun(t, restartedRootWindow(t), mainRule(t), "main")
	if given := slotsGiven(log); !slices.Contains(given, "ACME-2 at 10:10") {
		t.Errorf("the replay gave the slots %v, want ACME-2's at 10:10", given)
	}
	architect := string(builtToken(t, "ACME-1", claim.RoleArchitect))
	turns := linesAt(log, func(line replayLine) bool { return line.Msg == turnStartedLine && line.Claim == architect })
	if want := []time.Time{clock(10, 20)}; !slices.EqualFunc(turns, want, time.Time.Equal) {
		t.Errorf("ACME-1's architect started turns at %s, want at %s", describeTimes(turns), describeTimes(want))
	}
	if n := result.Divergence.NoLifecycleTurns; n != 0 {
		t.Errorf("the replay counted %d turns begun with no lifecycle, want 0", n)
	}
}

// A clone of a piece of work under way at the window's start opens as the piece would have opened
// had it not been under way (clonePieces): an accepting piece, whose start is the move into
// accepting, on that move; every other on its role's start.
func TestACloneOfASeededPieceOpensAsItsPhaseOpens(t *testing.T) {
	seeded := []*piece{
		{issue: "ACME-1", phase: phase.Accepting, role: claim.RoleArchitect, opens: opensAtSeed},
		{issue: "ACME-1", phase: phase.Testing, role: claim.RoleTester, opens: opensAtSeed},
	}
	clones := clonePieces(seeded, "ACME-9")
	if len(clones) != 2 || clones[0].opens != opensOnPhase || clones[1].opens != opensOnStart {
		var opens []pieceOpen
		for _, clone := range clones {
			opens = append(opens, clone.opens)
		}
		t.Errorf("the clones open by %v, want the accepting one on its phase (%d) and the testing one on its start (%d)", opens,
			opensOnPhase, opensOnStart)
	}
}

// seededSendBackWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5 with a cap of
// 10, its inputs reaching back to the daemon's boot at 06:00. ACME-1, a root promoted at 06:05,
// works 30 minutes in each phase to accepting at 08:50, where it stands at the window's start; its
// architect sends it back to implementing at 09:30, and it works 20 minutes in implementing and 10
// in each phase after it to merging at 10:30. ACME-2, a root promoted at 07:00, moves to testing at
// 09:00, and a conflict moves it back from accepting at 09:40; its loop is 20 minutes in
// implementing and 10 in testing and in reviewing, and it is back in accepting at 10:20. ACME-3, a
// root promoted at 08:00, moves to testing at 10:30, and its architect sends it back from accepting
// at 11:10. The base branch took one commit, at 09:25, inside ACME-2's stretch and none inside
// ACME-3's: the stretches inside the window separate, so the chance fitted at M = 1 is 1 and at
// M = 0 is 0, and ACME-1's stretch, begun before the window, draws a conflict at its send-back.
func seededSendBackWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(3 * time.Hour)
	b.capped(10)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(7, 50, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(8, 50, phase.Accepting, workflow.TriggerReviewApproved), moveAt(9, 30, phase.Implementing, workflow.TriggerBackward),
		moveAt(9, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 0, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 10, phase.Accepting, workflow.TriggerReviewApproved), moveAt(10, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(10, 30, phase.Merging, workflow.TriggerRetroCompleted))
	rootThrough(b, "ACME-2", "a2", clock(7, 0), clock(7, 15), moveAt(7, 45, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(9, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 20, phase.Accepting, workflow.TriggerReviewApproved), moveAt(9, 40, phase.Implementing, workflow.TriggerConflict),
		moveAt(10, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 20, phase.Accepting, workflow.TriggerReviewApproved))
	rootThrough(b, "ACME-3", "a3", clock(8, 0), clock(8, 15), moveAt(8, 45, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 30, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 40, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 50, phase.Accepting, workflow.TriggerReviewApproved), moveAt(11, 10, phase.Implementing, workflow.TriggerBackward))
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {{SHA: "5b8e2d7c1f4a3069b1c2d3e4f5a6b7c8d9e0f1a3", At: clock(9, 25), Branch: "main"}}}
	return window
}

// A drawn conflict that takes the place of an architect's send-back of a piece under way at the
// window's start gives the issue its loop and then that piece's work again, which opens on the
// issue's return to accepting (clonePieces). The test replays seededSendBackWindow under
// legion-v20.42.5: ACME-1's stretch draws a conflict at its send-back at 09:30, it runs ACME-2's
// loop back to accepting at 10:10, its architect's 40 minutes of accepting send it back at 10:50,
// the stretch after the loop drawing no conflict, and it works its own recorded way back to
// accepting and signs off into retro at 11:40. It fails where the clone of the accepting piece
// waits for a start no move into accepting writes, for which ACME-1 stands in accepting from 10:10
// to the window's end.
func TestAClonedAcceptingPieceOpensOnItsPhase(t *testing.T) {
	_, log, _ := replayLoopWindow(t, seededSendBackWindow(t), "legion-v20.42.5", 1)
	retro := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.To == string(phase.Retro)
	})
	if want := []time.Time{clock(11, 40)}; !slices.EqualFunc(retro, want, time.Time.Equal) {
		t.Errorf("ACME-1 moved into retro at %s, want at %s", describeTimes(retro), describeTimes(want))
	}
}

// bootAtStartWindow is a window from 10:00 to 12:00 recorded under keep-past-review, its inputs
// reaching back to the daemon's boot at 07:00, that opens inside the next boot. ACME-1, a root
// promoted at 07:05, works 30 minutes in each phase to its review's approval at 09:20, and keeps
// its slot in accepting to the window's end. A daemon process starts at 09:58:30, and its reconcile
// holds the roots for the Dispatch consumer at 10:00:40; at 10:00:10 a person files ACME-3, which
// Legion is not given.
func bootAtStartWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.window.Parts[0].Rule = "keep-past-review"
	b.reaches(3 * time.Hour)
	b.boot(clock(7, 0))
	rootThrough(b, "ACME-1", "a1", clock(7, 5), clock(7, 20), moveAt(7, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 20, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 50, phase.Reviewing, workflow.TriggerTesterPassed))
	b.pastReviewKept(clock(9, 20), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	b.boot(clock(9, 58).Add(30 * time.Second))
	b.file(clock(10, 0).Add(10*time.Second), "ACME-3", filing{Title: "Agree the shelf names with the warehouse", Status: "backlog", Rank: "c3"})
	b.info(clock(10, 0).Add(40*time.Second), reconcileHoldingLine, "count", 1, "target", 9, "ack_floor", 3)
	return b.build()
}

// The first pass of the daemon process that runs at the window's start waits for that process's
// recorded reconcile (driver.run): no fact is applied before it. The test replays bootAtStartWindow
// under main: ACME-1, past its review, keeps its slot from the window's start to the reconcile at
// 10:00:40, where main's pass gives it back, 40 seconds past the review holding a slot. It fails
// where the first fact of the window, the filing at 10:00:10, runs the first pass, for which the
// slot goes back at 10:00:10.
func TestTheFirstPassWaitsForTheRunningBootsReconcile(t *testing.T) {
	result, log := knobRun(t, bootAtStartWindow(t), mainRule(t), "main")
	released := linesAt(log, func(line replayLine) bool { return line.Msg == slotReleasedLine && line.Issue == "ACME-1" })
	if want := []time.Time{clock(10, 0).Add(40 * time.Second)}; !slices.EqualFunc(released, want, time.Time.Equal) {
		t.Errorf("ACME-1 gave its slot back at %s, want at %s, the boot's reconcile", describeTimes(released), describeTimes(want))
	}
	if hours, want := result.Scorecard.PastReviewHolding.Hours, 40.0/3600; math.Abs(hours-want) > fixtureTolerance {
		t.Errorf("pastReviewHolding is %v hours, want %v", hours, want)
	}
}

// freshSessionWindow is a window from 10:00 to 13:00 recorded under legion-v20.42.5, in a
// deployment that gates the spec, its inputs reaching back to the daemon's boot at 05:00. ACME-1, a
// root promoted at 05:05 whose spec a person approves at 05:15, implements from 05:50 to the
// window's end. ACME-2, filed todo under it at 05:30, starts at 06:00 and works 30 minutes in each
// phase to retro at 08:30. At 09:50 ACME-1's architect writes version 2 of the spec, which closes
// the gate to the window's end. ACME-2 moves to merging at 10:30, where its merger's start waits
// for the gate. At 11:00 the operator retries the merger with a fresh session: its claim is
// launched afresh, and the phase is released by the operator's retry with no hold standing. The
// merger works from 11:01 to 11:02 and sends ACME-2 back to implementing at 11:03.
func freshSessionWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(5 * time.Hour)
	b.gated(record.DesignStepSpec)
	b.boot(clock(5, 0))
	promotedRoot(b, clock(5, 5), "ACME-1", "a1")
	b.specWritten(clock(5, 10), "ACME-1")
	b.approves(clock(5, 15), "ACME-1", "")
	b.rootDesigned(clock(5, 20), "ACME-1")
	b.file(clock(5, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "b2"})
	b.segmentEnds(clock(5, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.childStarts(clock(6, 0), "ACME-2")
	b.through("ACME-2", phase.Planning, moveAt(6, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(7, 30, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(8, 0, phase.Accepting, workflow.TriggerReviewApproved), moveAt(8, 30, phase.Retro, workflow.TriggerSignOff))
	b.specRevised(clock(9, 50), "ACME-1", 2)
	b.gateClosed(clock(9, 50), "ACME-1", intake.DispatchArtifactVersion, 2)
	b.turnEnds(clock(10, 30), "ACME-2", claim.RoleImplementer)
	b.moved(clock(10, 30), "ACME-2", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	b.launched(clock(11, 0), "ACME-2", claim.RoleMerger, false)
	b.info(clock(11, 0), "workflow: a held phase is released", "issue", "ACME-2", "phase", string(phase.Merging), "reason", operatorRetryReason)
	b.turnStarts(clock(11, 1), "ACME-2", claim.RoleMerger)
	b.turnEnds(clock(11, 2), "ACME-2", claim.RoleMerger)
	b.moved(clock(11, 3), "ACME-2", phase.Merging, phase.Implementing, workflow.TriggerBackward)
	b.slotTaken(clock(11, 3), "ACME-2")
	b.turnStarts(clock(11, 3), "ACME-2", claim.RoleImplementer)
	return b.build()
}

// The operator's retry of a phase's worker with a fresh session, which launches its claim outside
// the start fence and then restarts the phase (api/freshsession.go at 958173da), enters a replay at
// its recorded offset from the start of the visit it retries (driver.freshRetry): the worker's
// claim is launched, its next piece of work in the phase opens, and the restart's own start waits
// for the fence as recorded. The test replays freshSessionWindow under legion-v20.42.5: ACME-2
// moves to merging at 10:30, its merger's start waits for the closed gate, the retry at 11:00 opens
// the merger's work, and ACME-2 moves back to implementing at 11:03, with no move refused. It fails
// where the replay enters no such retry, for which the merger's work waits for the gate and ACME-2
// stands in merging to the window's end.
func TestAnOperatorsFreshSessionRunsTheWorkerOutsideTheFence(t *testing.T) {
	result, log, _ := replayLoopWindow(t, freshSessionWindow(t), "legion-v20.42.5", 1)
	back := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-2" && line.From == string(phase.Merging)
	})
	if want := []time.Time{clock(11, 3)}; !slices.EqualFunc(back, want, time.Time.Equal) {
		t.Errorf("ACME-2 left merging at %s, want at %s", describeTimes(back), describeTimes(want))
	}
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("the replay refused %+v", refused)
	}
}

// bootConflictsWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00. ACME-1 and ACME-2, roots promoted at 09:00 and
// 09:05, work 15 minutes in each phase to accepting and 5 in accepting and in retro, so they reach
// merging at 10:25 and 10:30. A daemon process stops and the next starts at 10:45, and conflicts
// move ACME-1 back at 10:50 and ACME-2 at 10:55; each loop is 15 minutes in each phase, back to
// merging at 12:05 and 12:10, where each stands at the window's end. Both stretches inside the
// window span the boot and end in a conflict, so the fitted chance is 1.
func bootConflictsWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.boot(clock(7, 0))
	for i, key := range []string{"ACME-1", "ACME-2"} {
		at := func(hour, minute int) time.Time { return clock(hour, minute).Add(time.Duration(5*i) * time.Minute) }
		rootThrough(b, key, fmt.Sprintf("a%d", i), at(9, 0), at(9, 15), builtMove{at(9, 30), phase.Implementing, workflow.TriggerPlannerCompleted},
			builtMove{at(9, 45), phase.Testing, workflow.TriggerImplementationReady}, builtMove{at(10, 0), phase.Reviewing, workflow.TriggerTesterPassed},
			builtMove{at(10, 15), phase.Accepting, workflow.TriggerReviewApproved}, builtMove{at(10, 20), phase.Retro, workflow.TriggerSignOff},
			builtMove{at(10, 25), phase.Merging, workflow.TriggerRetroCompleted})
	}
	b.stopped(clock(10, 45))
	b.boot(clock(10, 45))
	for i, key := range []string{"ACME-1", "ACME-2"} {
		at := func(hour, minute int) time.Time { return clock(hour, minute).Add(time.Duration(5*i) * time.Minute) }
		b.through(key, phase.Merging, builtMove{at(10, 50), phase.Implementing, workflow.TriggerConflict},
			builtMove{at(11, 5), phase.Testing, workflow.TriggerImplementationReady}, builtMove{at(11, 20), phase.Reviewing, workflow.TriggerTesterPassed},
			builtMove{at(11, 35), phase.Accepting, workflow.TriggerReviewApproved}, builtMove{at(11, 50), phase.Retro, workflow.TriggerSignOff},
			builtMove{at(12, 5), phase.Merging, workflow.TriggerRetroCompleted})
	}
	return b.build()
}

// A drawn conflict of a stretch that spans a recorded boot enters at the first boot inside the
// stretch (assumption 3): the boot re-reads each pull request, so its read sees the conflict, at the
// chance the model gives a stretch that spans a boot. The test replays bootConflictsWindow under
// legion-v20.42.5 at the seeds 1 to 3: both merging stretches span the boot at 10:45, so both draw
// there at their fitted chance of 1, and both issues move back to implementing at 10:45. It fails
// where a conflict enters at its stretch's end, for which ACME-1 moves back at 10:50 and ACME-2 at
// 10:55.
func TestADrawnConflictEntersAtTheBootInsideItsStretch(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		_, log, _ := replayLoopWindow(t, bootConflictsWindow(t), "legion-v20.42.5", seed)
		for _, key := range []string{"ACME-1", "ACME-2"} {
			back := linesAt(log, func(line replayLine) bool {
				return line.Msg == phaseChangedLine && line.Issue == key && line.From == string(phase.Merging) && line.Trigger == workflow.TriggerConflict
			})
			if len(back) == 0 || !back[0].Equal(clock(10, 45)) {
				t.Errorf("at seed %d %s moved back from merging by conflict at %s, want first at 10:45:00, the boot", seed, key, describeTimes(back))
			}
		}
	}
}

// A filing the replay applies late takes back no change of the child's later events: none of them
// enters before it (driver.awaitsFiling), an architect's edit of a child it filed follows the
// filing, and a write of the daemon's session follows that edit (anchorFollow), each at its recorded
// offset. The test replays lateRootWindow under keep-past-review, in which ACME-2's architect files
// ACME-3 in triage at 10:10, inside its turn, and edits ACME-3's title at 10:20, the daemon's session
// writes ACME-3 todo at 10:25 (the release the architect's API makes), and ACME-2 waits for its child
// from 10:30, where ACME-3 starts. ACME-2 is promoted an hour late, so its filing of ACME-3, an act of
// its work, enters at 11:10, the edit at 11:20 and the todo at 11:25: ACME-3 stands todo and starts
// at 11:30. It fails where the filing's triage stands over the todo, for which ACME-3 leaves the
// workflow and never starts.
func TestALateFilingTakesBackNoLaterChange(t *testing.T) {
	log, _ := replayCopied(t, lateRootWindow(t, func(b *windowBuilder) {
		headerChild(b, clock(10, 10), "triage")
		b.changed(clock(10, 20), "ACME-3", b.session("ACME-2", claim.RoleArchitect), func(issue *builtIssue) {
			issue.Title = "Write the export's header row in bold"
		})
		b.daemonWrites(clock(10, 25), "ACME-3", "todo")
		b.rootWaitsForChildren(clock(10, 30), "ACME-2", 1)
		b.childStarts(clock(10, 30), "ACME-3")
	}), "keep-past-review")
	if started, want := startedAt(log, "ACME-3"), []time.Time{clock(11, 30)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-3 started at %s, want at %s: the late filing's triage takes back no later todo", describeTimes(started),
			describeTimes(want))
	}
}

// lateBlockerWindow is a window from 09:00 to 14:00 recorded under main with a cap of 2, its inputs
// reaching back to the daemon's boot at 06:00. ACME-5, a root promoted at 06:05, implements from
// 06:50 to the window's end, holding a slot. ACME-1, a root promoted at 06:30, holds the other slot
// to its review's approval at 09:00, and merges at 11:00. ACME-2, a root handed to Legion at 07:00,
// takes that slot at 09:00 and works 5 or 10 minutes in each phase to its merge at 10:00, giving its
// slot back at its accepting at 09:45. ACME-3, which a person files todo under ACME-5 at 07:30,
// blocked by ACME-2, starts at 10:00 once ACME-2 is done, and its planner works to 10:30. At 10:40
// ACME-5's architect parks ACME-3 (the daemon's session writes it backlog), and at 12:30 releases it
// again (todo), where ACME-3 starts its second generation and its planner works to 13:00. Under
// keep-past-review ACME-1 keeps its slot to its merge at 11:00, so ACME-2 starts at 11:00 and is done
// at 12:00, two hours late, and ACME-3 is parked before it ever starts.
func lateBlockerWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.reaches(3 * time.Hour)
	b.capped(2)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-5", "a5", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted))
	rootThrough(b, "ACME-1", "a0", clock(6, 30), clock(6, 45), moveAt(7, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 15, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 0, phase.Accepting, workflow.TriggerReviewApproved))
	b.file(clock(7, 0), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.file(clock(7, 30), "ACME-3", filing{Title: "Write the export's header row", Parent: "ACME-5", Tree: "ACME-5", Status: "todo", Rank: "b3",
		BlockedBy: []string{"ACME-2"}})
	b.rootPromoted(clock(9, 0), "ACME-2")
	b.rootDesigned(clock(9, 5), "ACME-2")
	b.through("ACME-2", phase.Planning, moveAt(9, 15, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(9, 25, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 35, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 45, phase.Accepting, workflow.TriggerReviewApproved), moveAt(9, 50, phase.Retro, workflow.TriggerSignOff),
		moveAt(9, 55, phase.Merging, workflow.TriggerRetroCompleted), moveAt(10, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(10, 0), "ACME-2", record.DoneStatus)
	b.childStarts(clock(10, 0), "ACME-3", "ACME-2")
	b.segmentEnds(clock(10, 30), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.daemonWrites(clock(10, 40), "ACME-3", "backlog")
	b.leaves(clock(10, 40), "ACME-3", "backlog")
	b.through("ACME-1", phase.Accepting, moveAt(9, 20, phase.Retro, workflow.TriggerSignOff),
		moveAt(9, 40, phase.Merging, workflow.TriggerRetroCompleted), moveAt(11, 0, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(11, 0), "ACME-1", record.DoneStatus)
	b.issue("ACME-3").generation++
	b.daemonWrites(clock(12, 30), "ACME-3", "todo")
	b.childStarts(clock(12, 30), "ACME-3")
	b.segmentEnds(clock(13, 0), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	return b.build()
}

// The work of a generation the replay never ran, its issue having left the workflow before that
// work opened, runs at the generation the issue stands at in the replay when it opens, as a drawn
// piece's does (moveGeneration): the record's generation is one the replay's issue has left. The test
// replays lateBlockerWindow under keep-past-review: ACME-3 is parked at 10:40 before its blocker is
// done, enters again at 12:30 at its second generation and starts, and its planner's work of the
// first generation, half an hour long, moves it to implementing at 13:00, with no move refused. It
// fails where that work's move is entered at the record's first generation, which the daemon refuses
// for its generation.
func TestAnUnrunGenerationsWorkMovesAtTheReplaysGeneration(t *testing.T) {
	result, log := knobRun(t, lateBlockerWindow(t), replayedRule(t, "keep-past-review"), "main")
	planned := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-3" && line.From == string(phase.Planning) && line.To == string(phase.Implementing)
	})
	if want := []time.Time{clock(13, 0)}; !slices.EqualFunc(planned, want, time.Time.Equal) {
		t.Errorf("ACME-3 moved from planning to implementing at %s, want at %s", describeTimes(planned), describeTimes(want))
	}
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("the replay refused %+v", refused)
	}
}

// bootAcceptingWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 06:00. ACME-1, a root promoted at 06:05, moves into testing
// at 08:00 and stands in accepting from 08:50; its architect signs it off at 11:30, and it moves to
// merging at 11:45 and merges at 12:30. ACME-2, a root promoted at 06:10, moves into testing at 10:00
// and works 10 minutes in testing and in reviewing to accepting at 10:20. A daemon process stops and
// the next starts at 10:45, and at 10:50 a conflict moves ACME-2 back from accepting to implementing,
// where it implements to the window's end. ACME-2's is the window's one stretch that begins and ends
// inside it, so the fitted chance is 1.
func bootAcceptingWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(3 * time.Hour)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 20, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(8, 50, phase.Accepting, workflow.TriggerReviewApproved))
	rootThrough(b, "ACME-2", "a2", clock(6, 10), clock(6, 25), moveAt(6, 55, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(10, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 20, phase.Accepting, workflow.TriggerReviewApproved))
	b.stopped(clock(10, 45))
	b.boot(clock(10, 45))
	b.through("ACME-2", phase.Accepting, moveAt(10, 50, phase.Implementing, workflow.TriggerConflict))
	b.through("ACME-1", phase.Accepting, moveAt(11, 30, phase.Retro, workflow.TriggerSignOff),
		moveAt(11, 45, phase.Merging, workflow.TriggerRetroCompleted), moveAt(12, 30, phase.Done, workflow.TriggerPullRequestMerged))
	b.leaves(clock(12, 30), "ACME-1", record.DoneStatus)
	return b.build()
}

// A boot draws every stretch under way, whatever piece of its work runs (driver.bootConflicts): an
// issue that stands in accepting at a boot, whose stretch the record ends in merging, draws there at
// merging's chance for a stretch that spans a boot (assumption 3). The test replays
// bootAcceptingWindow under legion-v20.42.5 at the seeds 1 to 3: ACME-1's stretch, under way in its
// architect's accepting at the boot at 10:45, draws its conflict there at the fitted chance of 1,
// and ACME-1 moves back from accepting to implementing at 10:45. It fails where a boot draws only a
// running piece whose own move ends its stretch, for which ACME-1 is signed off at 11:30 and moves
// back from merging at 12:30.
func TestAStretchUnderWayDrawsAtABootWhateverPieceRuns(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		_, log, _ := replayLoopWindow(t, bootAcceptingWindow(t), "legion-v20.42.5", seed)
		back := linesAt(log, func(line replayLine) bool {
			return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.Trigger == workflow.TriggerConflict
		})
		from := linesAt(log, func(line replayLine) bool {
			return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.Trigger == workflow.TriggerConflict && line.From == string(phase.Accepting)
		})
		if want := []time.Time{clock(10, 45)}; !slices.EqualFunc(back, want, time.Time.Equal) || !slices.EqualFunc(from, want, time.Time.Equal) {
			t.Errorf("at seed %d ACME-1 moved back by conflict at %s, from accepting at %s; want once, from accepting at 10:45:00, the boot", seed,
				describeTimes(back), describeTimes(from))
		}
	}
}

// A run counts the recorded conflict loops it passes over, each of a stretch whose recorded end is a
// conflict and which draws none, and the conflicts it draws at stretches the record ended otherwise,
// in run.json (divergence.skippedLoops and divergence.addedLoops) and in its line of counts. The test
// replays acceptingLoopWindow under main, where ACME-2's stretch draws no conflict at 10:30 and goes
// on past its recorded loop, and bootAcceptingWindow under legion-v20.42.5, where ACME-1's stretch,
// which the record ended in its merge, draws a conflict and ACME-2's open loop is kept: one loop
// passed over and no conflict added, then none passed over and one added. It fails for a run that
// counts neither.
func TestARunCountsTheLoopsItPassesOverAndAdds(t *testing.T) {
	for _, c := range []struct {
		name           string
		window         Window
		rule           string
		skipped, added int
	}{
		{"a loop passed over", acceptingLoopWindow(t), "main", 1, 0},
		{"a conflict added", bootAcceptingWindow(t), "legion-v20.42.5", 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := t.TempDir()
			result, err := Run(context.Background(), RunConfig{Window: c.window, Rule: replayedRule(t, c.rule), Branch: MainBranch, Seed: 1,
				Slowdown: 1, PGDSN: replayDSN(t), Out: out})
			if err != nil {
				t.Fatalf("replay the window under %s: %v", c.rule, err)
			}
			var ran struct {
				Divergence map[string]json.RawMessage `json:"divergence"`
			}
			if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
				t.Fatalf("read the run's %s: %v", runFile, err)
			}
			if skipped, added := string(ran.Divergence["skippedLoops"]), string(ran.Divergence["addedLoops"]); skipped != fmt.Sprint(c.skipped) ||
				added != fmt.Sprint(c.added) {
				t.Errorf("run.json counts %q loops passed over and %q conflicts added, want %d and %d", skipped, added, c.skipped, c.added)
			}
			if want := fmt.Sprintf("%d recorded conflict loops passed over, %d conflicts drawn where the record shows none", c.skipped,
				c.added); !strings.Contains(result.Counts(), want) {
				t.Errorf("the run's line of what it counted, %q, does not say %q", result.Counts(), want)
			}
		})
	}
}

// runOutWindow is a window from 09:00 to 13:00 recorded under main with a run-out of four hours after
// it, its inputs reaching back to the daemon's boot at 06:00. ACME-2, a root promoted at 06:05,
// works to its review at 08:20, where its reviewer works, holding its slot, until a conflict moves
// it back to implementing at 12:00. Its loop, 90 minutes in implementing and 15 in testing, brings
// it back to reviewing at 13:45, inside the run-out, where its reviewer works to a rejection at
// 16:00, and it implements to the run-out's end. No stretch begins and ends inside the window, so
// the fitted rate draws no conflict.
func runOutWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-2", "a2", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 20, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(12, 0, phase.Implementing, workflow.TriggerConflict), moveAt(13, 30, phase.Testing, workflow.TriggerImplementationReady),
		moveAt(13, 45, phase.Reviewing, workflow.TriggerTesterPassed), moveAt(16, 0, phase.Implementing, workflow.TriggerReviewRejected))
	b.window.After = Duration(4 * time.Hour)
	return b.build()
}

// A window with a run-out replays past its end, to the run-out's, so the work the record shows after
// --to runs where the replay moves it, and its every measure reads the window alone (the architect's
// D12). The test replays runOutWindow under main: ACME-2's stretch draws no conflict at 12:00 and
// goes on past its recorded loop into its review recorded inside the run-out, so its reviewer works
// on across 13:00 and rejects it at 14:15, a move the replay's log shows. Its slot-hours of the
// window are each in a turn, and no scorecard, the replay's two nor the record's, counts a move, a
// slot or a loop after 13:00. It fails where the replay stops at the window's end, for which the
// rejection never comes.
func TestARunOutReplaysPastTheWindowAndMeasuresInsideIt(t *testing.T) {
	window := runOutWindow(t)
	result, log := knobRun(t, window, mainRule(t), "main")
	rejected := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-2" && line.From == string(phase.Reviewing) &&
			line.Trigger == workflow.TriggerReviewRejected
	})
	if want := []time.Time{clock(14, 15)}; !slices.EqualFunc(rejected, want, time.Time.Equal) {
		t.Errorf("ACME-2's review rejected it at %s, want at %s, inside the run-out", describeTimes(rejected), describeTimes(want))
	}
	if card := result.Scorecard; math.Abs(card.SlotHoursInTurn-4) > fixtureTolerance || card.SlotHoursNoTurn > fixtureTolerance {
		t.Errorf("ACME-2's slot-hours of the window are %v in a turn and %v in none, want 4 and 0", card.SlotHoursInTurn, card.SlotHoursNoTurn)
	}
	recorded, err := Score(window)
	if err != nil {
		t.Fatalf("score the record: %v", err)
	}
	for name, card := range map[string]Scorecard{"snapshot": result.Scorecard, "line": result.LineScorecard, "record": recorded} {
		if !card.From.Equal(clock(9, 0)) || !card.To.Equal(clock(13, 0)) || card.Done != 0 || len(card.DoneAt) != 0 {
			t.Errorf("the %s scorecard reads [%s, %s) with %d done, want [09:00, 13:00) with none", name, stamp(card.From), stamp(card.To), card.Done)
		}
		for key, first := range card.FirstSlot {
			if !first.Before(clock(13, 0)) {
				t.Errorf("the %s scorecard gives %s a first slot at %s, after the window", name, key, stamp(first))
			}
		}
		wantLoops := map[string]LoopHours{}
		if name == "record" {
			wantLoops[string(CauseConflict)] = LoopHours{Rounds: 1, Hours: 1}
		}
		if !maps.EqualFunc(card.Loops, wantLoops, func(a, b LoopHours) bool { return a.Rounds == b.Rounds && math.Abs(a.Hours-b.Hours) < fixtureTolerance }) {
			t.Errorf("the %s scorecard counts the loops %+v, want %+v: nothing after 13:00", name, card.Loops, wantLoops)
		}
	}
}
