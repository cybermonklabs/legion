package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// registerControllerFor is registerController under a Dispatch project key of its own, for the
// test of two projects below: a controller is recorded under the project token, which is what
// the wake asks about (wakeController), so a second project's controller is a second record.
func registerControllerFor(t *testing.T, pool *pgxpool.Pool, project string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(ctx, token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability of %s: %v", project, err)
	}
	if ok, err := st.RegisterController(ctx, token, generation, "ses-"+project, []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller of %s = %t, %v", project, ok, err)
	}
}

// tickNoticeOf is the tick notice the outbox holds for project, decoded and the row's own JSON
// beside it. The decode is the validator that reads an outbox payload against its kind
// (record.DecodeOutboxPayload, which the executor does before it publishes), so a payload the
// rules refused would fail here rather than reach the controller; the JSON is read beside it
// because a field a notice does not carry is absent from the row, which no decoded value shows.
func tickNoticeOf(t *testing.T, pool *pgxpool.Pool, project string) (record.ControllerNotice, string) {
	t.Helper()
	var row record.OutboxRow
	err := pool.QueryRow(context.Background(),
		`select id, kind, issue, payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindControllerNotice), project).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload)
	if err != nil {
		t.Fatalf("read the controller notice of %s: %v", project, err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("the controller notice of %s does not pass the validator: %v", project, err)
	}
	notice, ok := payload.(record.ControllerNotice)
	if !ok {
		t.Fatalf("the controller notice of %s decoded to %T, want a controller notice", project, payload)
	}
	if notice.Kind != record.TickNotice {
		t.Fatalf("the controller notice of %s is a %s, want the tick", project, notice.Kind)
	}
	return notice, string(row.Payload)
}

// The tick notice carries what the project holds: every issue the daemon's own record holds in
// the workflow that stands in neither the active set nor the waiting line, each with the one
// word for why, beside the open capabilities and the blocked rows the notice carries already
// (C3 of dispatch://INFRA-3107). A member of a parked tree, a child under a shut design gate and a
// member whose tree root the live read leaves out each take a row of their own, and a project holding nothing carries no field at all, as a tick with
// nothing blocked carries no blocked field.
//
// The word the controller is told and the word `legion state` shows are read here from two
// reads: the notice from the tick's own fact, the state from internal/projection over the same
// records. They are one judgement (record.HeldList) over two candidate reads, and the point of
// the comparison is that the two reads agree, so neither answer is copied from the other.
func TestTheTickNoticeCarriesTheHeldIssues(t *testing.T) {
	const parked, parkedMember = "LEGION-PARKED", "LEGION-PARKED-MEMBER"
	const gated, gatedChild = "LEGION-GATED", "LEGION-GATED-CHILD"
	const triaged, triagedMember = "LEGION-TRIAGED", "LEGION-TRIAGED-MEMBER"
	const blockedRoot, blocker = "LEGION-BLOCKED", "LEGION-BLOCKER"
	const otherProject, otherRoot = "OPS", "OPS-1"

	pool := migratedPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	admission := newAdmission(t, 2, log)
	admission.ReportCapabilities(func() []string { return []string{"secrets"} })
	registerController(t, pool)

	member := func(key, tree string, at phase.Phase, status string) record.Issue {
		parent := tree
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: key, Tree: tree, Parent: &parent, Phase: at, Generation: 1,
			Status: status, Rank: "M", HandedOver: true}
	}

	// A tree a person parked: its root is out of the workflow at backlog, and the member the park
	// left behind is in it, held by the tree rather than by anything of its own.
	putIssue(t, pool, record.Issue{Key: parked, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: parked, Tree: parked, Phase: phase.Admitted, Generation: 1,
		Status: "backlog", Rank: "A", HandedOver: true})
	seedLifecycle(t, pool, parked)
	putIssue(t, pool, member(parkedMember, parked, phase.Admitted, "todo"))
	// A tree whose design gate stands shut: its root holds a slot and no step of its gate is
	// registered, so its child waits for the gate.
	seedSlotted(t, pool, gated, "B")
	putIssue(t, pool, member(gatedChild, gated, phase.Admitted, "todo"))
	// No subject stands for `at_work`: cybermonklabs/legion#2539 deleted the phase
	// `awaiting_merge`, so no phase holds no admission slot and nothing produces that word today.
	approved := 1
	// A tree whose root a person took out of the workflow at triage, with a person's approval
	// standing on its design gate. Its root stands at phase done, so the tick's live read leaves
	// it out (record.Store.LiveIssues) and the member's tree is read from the records that read
	// leaves behind (record.ReadTreeRoots), while the state's read holds the root itself: the
	// two readers meet one issue over two reads, and the gates of those roots are read where the
	// facts are built (record.ReadLineFacts), so both classify over the same gates. Nothing of
	// the tree is closed, parked or waiting on a person and the gate is open, so no word above
	// `unclassified` is true of the member, in either reader.
	putIssue(t, pool, record.Issue{Key: triaged, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: triaged, Tree: triaged, Phase: phase.Done, Generation: 1,
		Status: "triage", Rank: "E", HandedOver: true})
	seedGate(t, pool, record.DesignGate{Issue: triaged, ArtifactID: "artifact-triaged", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true})
	putIssue(t, pool, member(triagedMember, triaged, phase.Admitted, "todo"))
	// A root an unfinished Dispatch issue holds, so that the notice carries blocked rows beside
	// the held ones: it keeps its place in the waiting line, which is where the state shows it
	// too, so it is a blocked row and no held row. The two lists answer two questions of one
	// pass, and nothing stands in both.
	seedWaiting(t, pool, blockedRoot, "D")
	seedMirror(t, pool, blockedRoot, "todo", blocker)
	seedMirror(t, pool, blocker, "in_progress")

	tick(t, pool, admission, "tick-held")

	notice, _ := tickNoticeOf(t, pool, testProject)
	want := record.ControllerNotice{
		Kind:             record.TickNotice,
		OpenCapabilities: []string{"secrets"},
		Blocked:          []record.BlockedIssue{{Issue: blockedRoot, BlockedBy: []string{blocker}}},
		Held: []record.HeldIssue{
			{Issue: gatedChild, Reason: record.HeldDesignGate, HeldBy: []string{gated}},
			{Issue: parkedMember, Reason: record.HeldTreeParked, HeldBy: []string{parked}},
			{Issue: triagedMember, Reason: record.HeldUnclassified},
		},
	}
	if !reflect.DeepEqual(notice, want) {
		t.Fatalf("the tick notice = %#v, want %#v: one held row for each issue nothing starts, beside the open capabilities and the blocked rows", notice, want)
	}

	// The state's own word for each of those issues, read through the projection that builds
	// `admission.held` over its own candidate read rather than copied from the notice above.
	var held map[string]record.HeldReason
	inTx(t, pool, func(tx pgx.Tx) {
		state, err := projection.Project(context.Background(), tx, record.NewStore(), testProject,
			admission.engine.GatedSteps(), nil, nil, nil, fixedNow)
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		held = make(map[string]record.HeldReason, len(state.Admission.Held))
		for _, row := range state.Admission.Held {
			held[row.Issue] = row.Reason
		}
	})
	for _, row := range notice.Held {
		if got := held[row.Issue]; got != row.Reason {
			t.Fatalf("the state holds %s for %q and the tick for %q: one issue reads one word wherever it is read", row.Issue, got, row.Reason)
		}
	}

	// A project holding nothing: its one issue stands in the active set, so its tick carries no
	// held rows at all, and the field is absent from the row as the blocked field is.
	other := newAdmissionForProject(t, otherProject, 1, nil, log)
	registerControllerFor(t, pool, otherProject)
	seedSlottedForProject(t, pool, otherProject, otherRoot, "A")

	tick(t, pool, other, "tick-other")

	otherNotice, otherPayload := tickNoticeOf(t, pool, otherProject)
	if len(otherNotice.Held) != 0 || len(otherNotice.Blocked) != 0 {
		t.Fatalf("the tick of a project holding nothing = %#v, want neither held nor blocked rows", otherNotice)
	}
	for _, field := range []string{`"held"`, `"blocked"`} {
		if strings.Contains(otherPayload, field) {
			t.Fatalf("the tick of a project holding nothing carries %s: %s", field, otherPayload)
		}
	}
}

// An issue the record holds in the workflow that no word is true of takes its place in the list
// with the word `unclassified`, and the daemon writes one line for it: the state body already
// names the issue, so the line is the daemon's own record of the gap and is written once for
// that issue and run, as a children-first wait is told once. A second tick over the same record
// writes nothing more.
func TestAnUnclassifiedHeldIssueIsLoggedOnceForTheRun(t *testing.T) {
	const root, stranded = "LEGION-OPEN", "LEGION-STRANDED"
	pool := migratedPool(t)
	var lines strings.Builder
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(&lines, nil)))
	registerController(t, pool)

	// A tree that runs with its gate open, and a member of it standing mid-phase with no slot:
	// nothing holds it that the ladder has a word for, and nothing starts it either.
	seedSlotted(t, pool, root, "A")
	approved := 1
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-open", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true})
	parent := root
	putIssue(t, pool, record.Issue{Key: stranded, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: stranded, Tree: root, Parent: &parent, Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "B", HandedOver: true})

	tick(t, pool, admission, "tick-1")

	notice, _ := tickNoticeOf(t, pool, testProject)
	want := []record.HeldIssue{{Issue: stranded, Reason: record.HeldUnclassified}}
	if !reflect.DeepEqual(notice.Held, want) {
		t.Fatalf("the tick notice holds %#v, want %#v: an issue no word is true of has a place in the list", notice.Held, want)
	}

	tick(t, pool, admission, "tick-2")

	const line = "no held word is true of"
	if got := strings.Count(lines.String(), line); got != 1 {
		t.Fatalf("the daemon logged the unclassified issue %d times over two ticks, want once for the run: %s", got, lines.String())
	}
	for _, named := range []string{stranded, string(phase.Implementing), "in_progress", string(record.HeldUnclassified)} {
		if !strings.Contains(lines.String(), named) {
			t.Fatalf("the line does not name %q: %s", named, lines.String())
		}
	}
}
