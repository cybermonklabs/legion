package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// sharedEnvelopes is the directory of the issue event envelopes the Legion daemon's intake
// decodes, shared with it (its TestTheSharedIssueEnvelopesCarryEveryFieldTheDaemonReads) and with
// packages/contracts, which parses each with the schemas it declares: the envelope this package
// publishes is the one the daemon and the schemas read.
const sharedEnvelopes = "../../../../daemon/internal/intake/testdata/dispatch/"

// sharedIssueEnvelope is one shared example: a write an issue handler makes, as it hands it to the
// event broker - the event, at the id and the time the example fixes, and the logins the event's
// own insert reads (events.Broker.QueueAppendWithLogins; nil for none) - and the row of its issue
// that the insert takes the event's sequence from, at the last_seq it held before the write.
type sharedIssueEnvelope struct {
	file    string
	number  int
	lastSeq int
	logins  *events.Logins
	event   model.Event
}

// sharedIssueEnvelopes is every shared example. Together they hold each field Dispatch writes on
// these events, in each form it writes it in: set, and null or [] where Go writes it so when it
// holds nothing (requireEveryWrittenField):
//
//   - issue-updated-every-field.json: a handed-over child with a repository of its own and the
//     components its parent chose, one of them since retired, held by a session, whose patch
//     moves its status, its assignee and a blocker, so it names the blocker and the two assignees.
//   - issue-claimed-every-field.json: a takeover of that child, so it carries the claim it
//     replaced and the child's own repository.
//   - issue-claimed-forced.json: a forced takeover of that child by a session a person's personal
//     token names in its request body, so its actor carries the token's person and no `proven`.
//   - issue-updated-no-field-moved.json: a closed child, whose own components say why it has none,
//     given the links it already has again: a closed issue takes that write, which moves no field
//     `changed` names, so `changed` is [].
//   - issue-released-closed.json: the release that child's close made, which the closing person
//     wrote.
//   - issue-updated-null-fields.json: the reopen of a root nobody holds, assigns or routes, with
//     no priority and no repository, so each of those is null; its thread_logins are what the
//     reopen's own insert reads, [] for an issue with no open ask.
//   - issue-claimed-first.json: the first claim of that root, so it replaces no claim.
//   - issue-released-null-claim.json: its holder's release of that root, so the claim is null.
//   - issue-claimed-agent-token.json: a claim of that root under the shared agent token, whose
//     actor is the session its body names, from where its body says it runs, and nothing else.
//
// So the claim events hold each reason Dispatch writes (api.claimIssue, releaseIssueClaim and
// patchIssue's close): claimed, takeover, forced, released and closed.
//
// Each issue.updated's `changed` is what model.IssueChangedFields, the list the API's patch
// writes, gives for the issue before and after the write, so an example states only a write
// Dispatch can make.
//
// The proven session actors are the three a broker-issued token records (api.brokerTokenActor): a
// token names a person or a service, never both and never neither, and names a project only beside
// a person (brokertoken.sessionCaller). The unproven one is the session a personal token's body
// names (api.bearerSessionActor): the token proves its person, recorded as the actor's owner, and
// no session, so the actor carries no `proven` mark. The person who reopens the root and closes the
// child, and the issues' creator, is a person at a browser, whom the cookie proves; a person's
// write notifies. The shared agent token every Dispatch takes (DISPATCH_AGENT_TOKEN) proves no
// person, service, project or session: its actor is the session its body names, with the origin
// its body sends (api.bearerSessionActor), and no `owner`, `service`, `project` or `proven`.
func sharedIssueEnvelopes(t *testing.T) []sharedIssueEnvelope {
	at := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	origin := func(pane string) *model.ActorOrigin {
		return &model.ActorOrigin{Host: "worker-7", Machine: "worker-7", Cwd: "/work/widgets", Tmux: "legion", Pane: pane, SessionTitle: "CAPTURE-7 implementer"}
	}
	implementer7 := model.Actor{Kind: "session", ID: "ses_implementer7", Origin: origin("%3"), Service: new("legion-fleet"), Proven: true}
	implementer8 := model.Actor{Kind: "session", ID: "ses_implementer8", Origin: origin("%4"), Owner: new("alice@example.com"), Project: new("CAPTURE"), Proven: true}
	architect3 := model.Actor{Kind: "session", ID: "ses_architect3", Owner: new("bob@example.com"), Proven: true}
	reviewer4 := model.Actor{Kind: "session", ID: "ses_reviewer4", Owner: new("bob@example.com"),
		Origin: &model.ActorOrigin{Host: "worker-9", Machine: "worker-9", Cwd: "/work/widgets"}}
	person := model.Actor{Kind: "user", ID: "alice@example.com", Proven: true}
	creator := &person
	held := &model.IssueClaim{Actor: implementer7, At: at("2026-11-19T06:00:00.25Z")}
	taken := &model.IssueClaim{Actor: implementer8, At: at("2026-11-19T06:31:00.654321Z")}
	forced := &model.IssueClaim{Actor: reviewer4, At: at("2026-11-19T06:45:00.5Z")}
	rootClaim := &model.IssueClaim{Actor: architect3, At: at("2026-11-19T05:15:00Z")}
	operator2 := model.Actor{Kind: "session", ID: "ses_operator2", Origin: origin("%5")}
	sharedTokenClaim := &model.IssueClaim{Actor: operator2, At: at("2026-11-19T05:25:00.5Z")}
	// updated is an issue.updated's payload for a write that took before to after:
	// IssueEventPayload as api.newIssueEventPayload builds it, with the changed list the patch
	// writes.
	updated := func(before, after model.Issue, changes model.ReferenceChanges) model.IssueEventPayload {
		payload := model.NewIssueEventPayload(after, changes)
		payload.Changed = model.IssueChangedFields(before, after)
		return payload
	}

	// CAPTURE-7, a child of CAPTURE-1 with a repository of its own, and the components CAPTURE-1
	// chose (componentsScan.resolve), one of which a re-import has since retired.
	child := model.Issue{
		Key: "CAPTURE-7", Project: "CAPTURE", Number: 7, Title: "Read every field Dispatch writes",
		Status: "in_progress", Priority: new(1), Rank: "UUUk", Labels: []string{"legion"},
		Parent: new("CAPTURE-1"), BlockedBy: []string{"CAPTURE-2", "CAPTURE-5"},
		Advances: []model.IssueAdvance{
			{ID: "contracts-hold-both-sides", Inherited: false},
			{ID: "contracts-inherited-from-parent", Inherited: true},
		},
		AdvancesRevision: 3,
		Assignee:         new("bob@example.com"),
		Claim:            held,
		Components: model.IssueComponents{Mode: "explicit", IDs: []string{"dispatch-outbox"}, Unknown: []string{"dispatch-intake-v1"},
			InheritedFrom: new("CAPTURE-1")},
		ExternalLinks:          []model.ExternalLink{{URL: "https://github.com/acme/widgets/issues/12", Kind: "github"}},
		Route:                  new("role:legion-implementer"),
		Repo:                   new("acme/widgets"),
		ResolvedRepo:           new("acme/widgets"),
		CreatedBy:              creator,
		CreatedAt:              at("2026-11-18T09:15:00.5Z"),
		UpdatedAt:              at("2026-11-19T06:30:00.123456Z"),
		PrimaryArtifactID:      "0d6f4a52-7d1e-4c1b-9a51-3c2b8e6f7a01",
		VerificationArtifactID: "0d6f4a52-7d1e-4c1b-9a51-3c2b8e6f7a02",
		PlanArtifactID:         "0d6f4a52-7d1e-4c1b-9a51-3c2b8e6f7a03",
	}
	// The patch took the child from todo, alice's and blocked by CAPTURE-2, advancing only the
	// priority its parent states, which the patch did not move, and gave it one of its own.
	unpatched := child
	unpatched.Status = "todo"
	unpatched.Assignee = new("alice@example.com")
	unpatched.BlockedBy = []string{"CAPTURE-2"}
	unpatched.Advances = []model.IssueAdvance{{ID: "contracts-inherited-from-parent", Inherited: true}}
	unpatched.UpdatedAt = at("2026-11-19T06:10:00Z")
	moved := updated(unpatched, child, model.ReferenceChanges{Targets: []model.ChangedReference{{Kind: "issue", ID: "CAPTURE-5"}}})
	moved.Ancestors = []string{"CAPTURE-1"}
	// An assignee move names the person the issue's questions reached before it and the one
	// they reach after it, which the handler knows (api.movedLogins).
	moved.ThreadLogins = []string{"alice@example.com", "bob@example.com"}

	// CAPTURE-3, a child of CAPTURE-1 a person closed, which released its claim, and whose own
	// components say why it has none. Its repository is its parent's. A closed issue takes a write
	// of the links it already has (api.patchIssue): it rewrites the rows and appends issue.updated,
	// and touches no column of the issue's row.
	closed := model.Issue{
		Key: "CAPTURE-3", Project: "CAPTURE", Number: 3, Title: "Name the outbox's components", Status: "done",
		Priority: new(2), Rank: "UUUj", Labels: []string{"legion"}, Parent: new("CAPTURE-1"), BlockedBy: []string{},
		Advances: []model.IssueAdvance{}, Assignee: new("alice@example.com"),
		Components: model.IssueComponents{Mode: "none", IDs: []string{}, Unknown: []string{},
			Reason: new("a chore of the build no component owns")},
		ExternalLinks:     []model.ExternalLink{{URL: "https://github.com/acme/widgets/pull/31", Kind: "github"}},
		ResolvedRepo:      new("acme/widgets"),
		CreatedBy:         creator,
		CreatedAt:         at("2026-11-17T11:00:00Z"),
		UpdatedAt:         at("2026-11-18T16:40:00.5Z"),
		ClosedAt:          new(at("2026-11-18T16:40:00.5Z")),
		PrimaryArtifactID: "0d6f4a52-7d1e-4c1b-9a51-3c2b8e6f7a04",
	}
	linksAgain := updated(closed, closed, model.ReferenceChanges{})
	linksAgain.Ancestors = []string{"CAPTURE-1"}

	// CAPTURE-9, a root nobody holds, assigns or routes, with no priority, label, blocker,
	// priority it advances, component, link or repository: each as an issue read gives it
	// (api.scanIssueRow), the lists [] and the rest null. The reopen takes it from done.
	root := model.Issue{
		Key: "CAPTURE-9", Project: "CAPTURE", Number: 9, Title: "Reopen a root nobody holds", Status: "todo",
		Rank: "UUUm", Labels: []string{}, BlockedBy: []string{}, Advances: []model.IssueAdvance{},
		Components:    model.IssueComponents{Mode: "inherit", IDs: []string{}, Unknown: []string{}},
		ExternalLinks: []model.ExternalLink{},
		CreatedBy:     creator,
		CreatedAt:     at("2026-11-17T14:00:00Z"),
		UpdatedAt:     at("2026-11-19T05:10:00.5Z"),
		// The primary document every create makes.
		PrimaryArtifactID: "0d6f4a52-7d1e-4c1b-9a51-3c2b8e6f7a09",
	}
	done := root
	done.Status = "done"
	done.ClosedAt = new(at("2026-11-18T12:00:00Z"))
	done.UpdatedAt = at("2026-11-18T12:00:00Z")
	reopened := updated(done, root, model.ReferenceChanges{})
	reopened.Ancestors = []string{}

	return []sharedIssueEnvelope{
		{
			file: "issue-updated-every-field.json", number: 7, lastSeq: 8,
			event: model.Event{ID: 4821, IssueKey: new("CAPTURE-7"), Type: "issue.updated", Actor: implementer7,
				CreatedAt: at("2026-11-19T06:30:00.123456Z"), Payload: moved},
		},
		{
			file: "issue-claimed-every-field.json", number: 7, lastSeq: 9,
			event: model.Event{ID: 4822, IssueKey: new("CAPTURE-7"), Type: "issue.claimed", Actor: implementer8,
				CreatedAt: taken.At, Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-7", Status: "in_progress", Repo: child.Repo, ResolvedRepo: child.ResolvedRepo,
					Claim: taken, Previous: held, Reason: "takeover",
				}},
		},
		{
			// A person's credential takes a live session's claim with force (api.claimIssue); the
			// actor is the session its body names.
			file: "issue-claimed-forced.json", number: 7, lastSeq: 10,
			event: model.Event{ID: 4823, IssueKey: new("CAPTURE-7"), Type: "issue.claimed", Actor: reviewer4,
				CreatedAt: forced.At, Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-7", Status: "in_progress", Repo: child.Repo, ResolvedRepo: child.ResolvedRepo,
					Claim: forced, Previous: taken, Reason: "forced",
				}},
		},
		{
			file: "issue-updated-no-field-moved.json", number: 3, lastSeq: 11,
			event: model.Event{ID: 4815, IssueKey: new("CAPTURE-3"), Type: "issue.updated", Actor: implementer8,
				CreatedAt: at("2026-11-19T06:20:00.5Z"), Payload: linksAgain},
		},
		{
			// The close appended its issue.closed as seq 10 (api.patchIssue appends issue.closed
			// for a status moved to done) and this release, of the claim the child held, as seq 11,
			// both with the closing person as their actor.
			file: "issue-released-closed.json", number: 3, lastSeq: 10,
			event: model.Event{ID: 4790, IssueKey: new("CAPTURE-3"), Type: "issue.released", Actor: person,
				CreatedAt: *closed.ClosedAt, Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-3", Status: "done", ResolvedRepo: closed.ResolvedRepo,
					Previous: &model.IssueClaim{Actor: implementer8, At: at("2026-11-18T09:00:00Z")}, Reason: "closed",
				}},
		},
		{
			// A reopen's thread_logins are read by its own insert, after the asks the reopen
			// restored (api.patchIssue, events.IssueLoginsOf).
			file: "issue-updated-null-fields.json", number: 9, lastSeq: 3, logins: events.IssueLoginsOf("CAPTURE-9"),
			event: model.Event{ID: 4801, IssueKey: new("CAPTURE-9"), Type: "issue.updated", Actor: person,
				CreatedAt: at("2026-11-19T05:10:00.5Z"), Payload: reopened},
		},
		{
			file: "issue-claimed-first.json", number: 9, lastSeq: 4,
			event: model.Event{ID: 4802, IssueKey: new("CAPTURE-9"), Type: "issue.claimed", Actor: architect3,
				CreatedAt: rootClaim.At, Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-9", Status: "todo", Claim: rootClaim, Reason: "claimed",
				}},
		},
		{
			file: "issue-released-null-claim.json", number: 9, lastSeq: 5,
			event: model.Event{ID: 4803, IssueKey: new("CAPTURE-9"), Type: "issue.released", Actor: architect3,
				CreatedAt: at("2026-11-19T05:20:00.75Z"), Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-9", Status: "todo", Previous: rootClaim, Reason: "released",
				}},
		},
		{
			file: "issue-claimed-agent-token.json", number: 9, lastSeq: 6,
			event: model.Event{ID: 4804, IssueKey: new("CAPTURE-9"), Type: "issue.claimed", Actor: operator2,
				CreatedAt: sharedTokenClaim.At, Payload: model.IssueClaimEventPayload{
					Key: "CAPTURE-9", Status: "todo", Claim: sharedTokenClaim, Reason: "claimed",
				}},
		},
	}
}

// The envelope this package publishes for each shared example is, field for field, its file, and
// the examples together hold every field Dispatch writes on these events. Each envelope is built as
// production builds it: the event is appended through the broker's insert, which writes the stored
// payload's last_seq and a reopen's thread_logins in SQL, read back by the outbox's batch read, and
// encoded by envelope. So a field of the payload renamed, dropped or added changes a built envelope
// or the walk of the examples, and this test fails until the files, and so the daemon's and the
// contracts' tests that read them, say the same.
func TestTheBuiltIssueEnvelopesAreTheSharedFixtures(t *testing.T) {
	examples := sharedIssueEnvelopes(t)
	t.Run("the examples hold every field Dispatch writes", func(t *testing.T) {
		requireEveryWrittenField(t, examples)
	})
	for _, example := range examples {
		t.Run(example.file, func(t *testing.T) {
			built := appendAndPublish(t, example)
			want, err := json.MarshalIndent(built, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(sharedEnvelopes + example.file)
			if err != nil {
				t.Fatalf("read the shared fixture: %v\nthe built envelope is:\n%s", err, want)
			}
			var fixture contracts.Envelope
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatalf("decode the shared fixture: %v", err)
			}
			if !reflect.DeepEqual(built, fixture) {
				t.Fatalf("built %s envelope differs from its fixture\nbuilt payload:   %s\nfixture payload: %s\nthe built envelope is:\n%s",
					example.event.Type, built.Payload, fixture.Payload, want)
			}
		})
	}
}

// appendAndPublish appends example's event as its handler does, through the broker's insert, on a
// database holding the event's issue at the last_seq it held before the write, and answers the
// envelope the outbox publishes for it: the event as the outbox's batch reads it back out of the
// events table (readBatch), encoded by envelope. The id and the time are the example's: the id is
// the next value of the events table's sequence, and the time, which the insert takes from the
// database's clock, is set on the row before the batch reads it.
//
// Without DISPATCH_TEST_DATABASE_URL it skips, as every Postgres-backed test of the package does,
// except on a GitHub Actions runner, where it fails naming the variable (testdb.SkipOutsideCI):
// go test reads a skip as a pass, and the envoy-go job that runs this package sets the variable.
func appendAndPublish(t *testing.T, example sharedIssueEnvelope) contracts.Envelope {
	t.Helper()
	if os.Getenv(storetest.DatabaseURLEnv) == "" {
		testdb.SkipOutsideCI(t, "%s must be set to build the shared issue envelopes through the broker's insert and the outbox's batch read", storetest.DatabaseURLEnv)
	}
	ctx := context.Background()
	database := storetest.Open(t)
	key := *example.event.IssueKey
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into projects (key, name, default_assignee) values ('CAPTURE', 'Capture', 'alice@example.com')`, nil},
		{`insert into issues (key, project_key, number, title, created_by, rank, last_seq)
			values ($1, 'CAPTURE', $2, $1, '{"kind":"user","id":"alice@example.com","proven":true}', 'U', $3)`,
			[]any{key, example.number, example.lastSeq}},
		{`select setval(pg_get_serial_sequence('events', 'id'), $1)`, []any{example.event.ID - 1}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
		return events.NewBroker().QueueAppendWithLogins(batch, example.event, example.logins, func(model.Event) {})
	}); err != nil {
		t.Fatalf("append the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update events set created_at = $1 where id = $2`, example.event.CreatedAt, example.event.ID); err != nil {
		t.Fatalf("fix the event's time: %v", err)
	}
	pending, stopped, _, err := readBatch(ctx, Deps{Store: database})
	if err != nil || stopped {
		t.Fatalf("read the batch: stopped=%v err=%v", stopped, err)
	}
	if len(pending) != 1 || pending[0].failure != nil || pending[0].event.ID != example.event.ID {
		t.Fatalf("the batch read %d events (first %+v), want the one appended, id %d", len(pending), pending, example.event.ID)
	}
	built, err := envelope(pending[0].event, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := built.Validate(); err != nil {
		t.Fatalf("validate the built envelope: %v", err)
	}
	return built
}

// writtenPayloads is the payload type the issue handlers write for each event type the examples
// hold (api.patchIssue, api.claimEvent).
var writtenPayloads = map[string]reflect.Type{
	"issue.updated":  reflect.TypeFor[model.IssueEventPayload](),
	"issue.claimed":  reflect.TypeFor[model.IssueClaimEventPayload](),
	"issue.released": reflect.TypeFor[model.IssueClaimEventPayload](),
}

// neverWritten is each field of the types these envelopes are built from that Dispatch never
// writes on an issue.updated, issue.claimed or issue.released published on its issue's topic,
// with the reason. Every other field must be in an example.
var neverWritten = map[string]string{
	"model.Event.trees": "the outbox publishes the event log's row, which holds none: trees ride only on an API " +
		"answer to a caller that sent Dispatch-Trees: 1 and on /ws/live frames (model.Event.Trees)",
	"model.IssueEventPayload.forced": "it is on an issue.created alone (model.IssueEventPayload)",
	"model.ForcedCreate.past":        "it is the list model.IssueEventPayload.forced carries",
	"model.ReferenceChangesPayload.references_changed_truncated": "an issue.updated names at most 41 issues: its " +
		"parent and its blockers before and after the patch (api.issueEdgeChanges), each list at most " +
		"contracts.MaxIssueBlockers (20) long, and refs.CountedChanges truncates a kind only past " +
		"refs.CountedChangeLimit (100); a claim or a release names none",
	"model.ReferenceChangesPayload.references_changed_truncated_kinds": "as references_changed_truncated",
	"model.ChangedReference.issue_key": "an issue write names issue targets alone (api.issueEdgeChanges), and the " +
		"address fields are an ask's and a document's (model.ChangedReference)",
	"model.ChangedReference.artifact_id": "as model.ChangedReference.issue_key",
	"model.ChangedReference.ref":         "as model.ChangedReference.issue_key",
	"model.Actor.act": "actingPerson sets it (api/server.go), and no issue handler calls it: a create, a patch and " +
		"a claim record requireActor's actor, a release optionalActor's or bearerSessionActor's",
	"model.ActorAct.session":           "it is the act model.Actor.act carries",
	"model.ActorAct.session_proven":    "it is the act model.Actor.act carries",
	"model.ActorAct.token_id":          "it is the act model.Actor.act carries",
	"model.ActorAct.token_name":        "it is the act model.Actor.act carries",
	"model.ActorAct.scopes":            "it is the act model.Actor.act carries",
	"listenerwire.Envelope.expires_at": "envelope sets none",
	"listenerwire.Envelope.kept_at": "the outbox sets it on the copy of a role's route it kept for a holder (role.go), " +
		"never on the envelope envelope builds, the issue topic's copy these examples are",
	"listenerwire.Envelope.mention_dedupe_keys": "as listenerwire.Envelope.kept_at",
	"listenerwire.Envelope.role": "the listener sets it on a direct send Dispatch's API makes to a role's holder " +
		"(api/comment_delivery.go's routeRole), never on the envelope envelope builds, the issue topic's copy these examples are",
	"listenerwire.Envelope.payload_ref":      "envelope sets none",
	"listenerwire.Envelope.sender":           "envelope sets none",
	"listenerwire.Envelope.in_reply_to":      "envelope sets it on an ask.answered, an ask.resolved, a comment and a message alone",
	"listenerwire.Envelope.supersedes":       "envelope sets none",
	"listenerwire.Envelope.urgency":          "envelope sets it on an ask.* event alone",
	"listenerwire.Envelope.expects_reply":    "envelope leaves it out of every event's envelope, for the reason its comment gives",
	"listenerwire.EnvelopeSender.session_id": "it is the sender envelope never sets",
	"listenerwire.EnvelopeSender.machine":    "it is the sender envelope never sets",
	"listenerwire.EnvelopeSender.cwd":        "it is the sender envelope never sets",
	"listenerwire.EnvelopeSender.title":      "it is the sender envelope never sets",
	"listenerwire.EnvelopeSender.roles":      "it is the sender envelope never sets",
}

// neverEmpty is each field Go writes as null, [] or {} when it holds nothing that never holds
// nothing on these events, with the reason. Every other such field must be in an example in that
// form.
var neverEmpty = map[string]string{
	"model.Event.issue_key": "an issue event is its issue's: envelope publishes it on that issue's topic",
	"model.Event.payload":   "every event the issue handlers append carries its payload (writtenPayloads)",
	"model.Issue.created_by": "every issue holds its creator: issues.created_by is jsonb not null from migration 0001, " +
		"and createIssue writes the creating actor",
}

// neverSet is each pointer, list, map or interface field that never holds a value on these
// events, with the reason. Every other such field must be in an example holding one, so the type
// of the value, and not only of null or [], is what the examples and the schemas that read them
// agree on.
var neverSet = map[string]string{
	"model.Event.artifact_id": "an issue event is its issue's and no document's: the issue handlers append it with no " +
		"artifact (api.issueOwner)",
}

// requireEveryWrittenField fails t for each field of the types these envelopes are built from that
// no example carries, for each field Go writes as null, [] or {} when it holds nothing that no
// example carries in that form, and for each pointer, list, map or interface field that no example
// carries holding a value, unless neverWritten, neverEmpty or neverSet names it with its reason.
// The types are contracts.Envelope, its payload as envelope encodes it (publishedEvent) and that
// event's payload as its handler writes it (writtenPayloads), nested types and embedded ones
// included. So a field Dispatch writes only when it is set - a person's or a service's credential,
// an omitempty field added tomorrow - fails here until an example sets it; a field written as null
// or [] fails until an example holds nothing there, where an omitempty added to it would drop the
// key; and a field no example sets fails until one does, so the schemas that read the examples
// are held to the type of its value. Each example is also what encoding/json writes of it: each of
// its three levels - the envelope, the event and the event's payload - decoded into its Go type and
// encoded again must give the example back, so an example cannot hold a key no type declares, a
// value an omitempty or omitzero drops, or a value in a form Go does not write.
func requireEveryWrittenField(t *testing.T, examples []sharedIssueEnvelope) {
	walk := fieldWalk{
		declared: map[string]writtenField{}, visited: map[reflect.Type]bool{},
		carried: map[string]bool{}, carriedEmpty: map[string]bool{}, carriedSet: map[string]bool{},
	}
	walk.declare(t, walkedEnvelope)
	for _, example := range examples {
		data, err := os.ReadFile(sharedEnvelopes + example.file)
		if err != nil {
			t.Fatalf("read the shared fixture: %v", err)
		}
		requireWhatGoWrites(t, example.file, data)
		var envelope any
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatalf("decode %s: %v", example.file, err)
		}
		walk.value(t, example.file, walkedEnvelope, envelope)
	}
	for _, list := range []struct {
		name    string
		entries map[string]string
	}{{"neverWritten", neverWritten}, {"neverEmpty", neverEmpty}, {"neverSet", neverSet}} {
		for key := range list.entries {
			field, ok := walk.declared[key]
			switch {
			case !ok:
				t.Errorf("%s names %s, which no type these envelopes are built from declares", list.name, key)
			case list.name == "neverEmpty" && len(field.empty) == 0:
				t.Errorf("neverEmpty names %s, which Go leaves out when it holds nothing", key)
			case list.name == "neverSet" && !field.settable:
				t.Errorf("neverSet names %s, which is no pointer, list, map or interface", key)
			}
		}
	}
	keys := make([]string, 0, len(walk.declared))
	for key := range walk.declared {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		field := walk.declared[key]
		if _, never := neverWritten[key]; never {
			if walk.carried[key] {
				t.Errorf("an example carries %s, which neverWritten says these events never carry: drop it from the list", key)
			}
			continue
		}
		if !walk.carried[key] {
			t.Errorf("Dispatch writes %s and no example carries it: add an example that sets it, or name it in neverWritten with the reason these events cannot", key)
		}
		if len(field.empty) > 0 {
			_, full := neverEmpty[key]
			switch {
			case full && walk.carriedEmpty[key]:
				t.Errorf("an example carries %s holding nothing, which neverEmpty says these events never do: drop it from the list", key)
			case !full && !walk.carriedEmpty[key]:
				t.Errorf("Dispatch writes %s as %s when it holds nothing and no example carries it so: add an example that holds nothing there, or name it in neverEmpty with the reason these events always hold one", key, strings.Join(field.empty, " or "))
			}
		}
		if field.settable {
			_, unset := neverSet[key]
			switch {
			case unset && walk.carriedSet[key]:
				t.Errorf("an example carries %s holding a value, which neverSet says these events never do: drop it from the list", key)
			case !unset && walk.carried[key] && !walk.carriedSet[key]:
				t.Errorf("no example carries %s holding a value, only null or empty: add an example that sets it, or name it in neverSet with the reason these events never do", key)
			}
		}
	}
}

// requireWhatGoWrites fails t for each difference between the example data and what encoding/json
// writes of it, level by level: the envelope decoded into contracts.Envelope and encoded again,
// its payload into publishedEvent, and that event's payload into the type its handler writes
// (writtenPayloads). A key the example holds that the Go type does not declare, or holds in a
// form the encoder drops (a zero value under omitempty, a nil one under omitzero), is missing from
// what the encoder writes; a value Go writes in another form differs from it.
func requireWhatGoWrites(t *testing.T, file string, data []byte) {
	t.Helper()
	envelope := reencode[contracts.Envelope](t, file, "", data)
	var event struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(envelope.Payload), &event); err != nil {
		t.Errorf("%s.payload is not the event's JSON: %v", file, err)
		return
	}
	reencode[publishedEvent](t, file, ".payload", []byte(envelope.Payload))
	payload, ok := writtenPayloads[event.Type]
	if !ok {
		t.Errorf("%s is a %q, which writtenPayloads names no payload type for", file, event.Type)
		return
	}
	held := reflect.New(payload)
	if err := json.Unmarshal(event.Payload, held.Interface()); err != nil {
		t.Errorf("%s.payload.payload does not decode into %s: %v", file, payload, err)
		return
	}
	requireSameJSON(t, file+".payload.payload", event.Payload, held.Elem().Interface())
}

// reencode decodes data into a T, requires that encoding/json writes the same JSON of it, and
// answers the T.
func reencode[T any](t *testing.T, file, path string, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Errorf("%s%s does not decode into %T: %v", file, path, value, err)
		return value
	}
	requireSameJSON(t, file+path, data, value)
	return value
}

// requireSameJSON fails t for each difference between the JSON data and what encoding/json writes
// of value, by path.
func requireSameJSON(t *testing.T, path string, data []byte, value any) {
	t.Helper()
	written, err := json.Marshal(value)
	if err != nil {
		t.Errorf("encode %s as %T: %v", path, value, err)
		return
	}
	for _, difference := range jsonDifferences(path, decodeNumbers(t, data), decodeNumbers(t, written)) {
		t.Error(difference)
	}
}

// decodeNumbers decodes data with each number kept as written, so two numbers compare by their
// text.
func decodeNumbers(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}

// jsonDifferences names each place the example's value and the value Go writes differ.
func jsonDifferences(path string, example, written any) []string {
	exampleObject, exampleIsObject := example.(map[string]any)
	writtenObject, writtenIsObject := written.(map[string]any)
	if exampleIsObject && writtenIsObject {
		var differences []string
		names := make([]string, 0, len(exampleObject)+len(writtenObject))
		for name := range exampleObject {
			names = append(names, name)
		}
		for name := range writtenObject {
			if _, both := exampleObject[name]; !both {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			exampleValue, inExample := exampleObject[name]
			writtenValue, inWritten := writtenObject[name]
			switch {
			case !inWritten:
				differences = append(differences, fmt.Sprintf("%s.%s is %s in the example, and Go does not write it: no field declares it, or its value is one the field's omitempty or omitzero leaves out", path, name, jsonText(exampleValue)))
			case !inExample:
				differences = append(differences, fmt.Sprintf("Go writes %s.%s as %s, and the example does not hold it", path, name, jsonText(writtenValue)))
			default:
				differences = append(differences, jsonDifferences(path+"."+name, exampleValue, writtenValue)...)
			}
		}
		return differences
	}
	exampleList, exampleIsList := example.([]any)
	writtenList, writtenIsList := written.([]any)
	if exampleIsList && writtenIsList && len(exampleList) == len(writtenList) {
		var differences []string
		for index := range exampleList {
			differences = append(differences, jsonDifferences(fmt.Sprintf("%s[%d]", path, index), exampleList[index], writtenList[index])...)
		}
		return differences
	}
	if !reflect.DeepEqual(example, written) {
		return []string{fmt.Sprintf("%s is %s in the example, and Go writes %s", path, jsonText(example), jsonText(written))}
	}
	return nil
}

func jsonText(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

// writtenField is one JSON field a type these envelopes are built from declares: each form
// encoding/json writes it in when it holds nothing and keeps the key ("null" for a nil pointer,
// list, map or interface with neither omitempty nor omitzero, "[]" for an empty list and "{}" for
// an empty map that the field's options keep), and whether it is a pointer, list, map or interface,
// which holds a value or holds nothing.
type writtenField struct {
	empty    []string
	settable bool
}

// fieldWalk is requireEveryWrittenField's walk: every field the types declare, by owner and JSON
// name ("model.Actor.owner"), and which of them the examples carry, carry holding nothing, and
// carry holding a value.
type fieldWalk struct {
	declared     map[string]writtenField
	visited      map[reflect.Type]bool
	carried      map[string]bool
	carriedEmpty map[string]bool
	carriedSet   map[string]bool
}

var (
	walkedEnvelope       = reflect.TypeFor[contracts.Envelope]()
	walkedPublishedEvent = reflect.TypeFor[publishedEvent]()
	walkedEvent          = reflect.TypeFor[model.Event]()
	walkedMarshaler      = reflect.TypeFor[json.Marshaler]()
)

// jsonFields calls visit for each field encoding/json writes of struct type typ: by the struct that
// declares it, so an embedded struct's fields are its own, with the field's JSON name.
func jsonFields(typ reflect.Type, visit func(owner reflect.Type, field reflect.StructField, name string)) {
	for field := range typ.Fields() {
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" && options == "" {
			continue
		}
		if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
			jsonFields(field.Type, visit)
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		visit(typ, field, name)
	}
}

// encodedWhole is whether encoding/json writes a value of typ by its own MarshalJSON (time.Time,
// json.RawMessage), which this walk does not look into.
func encodedWhole(typ reflect.Type) bool {
	return typ.Implements(walkedMarshaler) || reflect.PointerTo(typ).Implements(walkedMarshaler)
}

// settable is whether a field of kind holds either a value or nothing.
func settable(kind reflect.Kind) bool {
	return kind == reflect.Pointer || kind == reflect.Slice || kind == reflect.Map || kind == reflect.Interface
}

// emptyForms is each form encoding/json writes field of owner in when it holds nothing and keeps
// the key: an owner whose field is nil, and for a list or a map one whose field is empty, encoded,
// and the field's key read from what it wrote.
func emptyForms(t *testing.T, owner reflect.Type, field reflect.StructField, name string) []string {
	t.Helper()
	if !settable(field.Type.Kind()) {
		return nil
	}
	nothing := []reflect.Value{reflect.Zero(field.Type)}
	switch field.Type.Kind() {
	case reflect.Slice:
		nothing = append(nothing, reflect.MakeSlice(field.Type, 0, 0))
	case reflect.Map:
		nothing = append(nothing, reflect.MakeMap(field.Type))
	}
	var forms []string
	for _, value := range nothing {
		holder := reflect.New(owner).Elem()
		holder.FieldByIndex(field.Index).Set(value)
		data, err := json.Marshal(holder.Interface())
		if err != nil {
			t.Fatalf("encode a %s whose %s holds nothing: %v", owner, name, err)
		}
		var written map[string]json.RawMessage
		if err := json.Unmarshal(data, &written); err != nil {
			t.Fatalf("decode the encoded %s: %v", owner, err)
		}
		if form, kept := written[name]; kept && !slices.Contains(forms, string(form)) {
			forms = append(forms, string(form))
		}
	}
	return forms
}

// heldTypes is the types a field's value is written from: its own, except an envelope's payload,
// which is the published event encoded as a string, and an event's payload, which is the
// payload its handler writes for the event's type.
func heldTypes(owner reflect.Type, name string, typ reflect.Type) []reflect.Type {
	switch {
	case owner == walkedEnvelope && name == "payload":
		return []reflect.Type{walkedPublishedEvent}
	case owner == walkedEvent && name == "payload":
		var payloads []reflect.Type
		for _, payload := range writtenPayloads {
			if !slices.Contains(payloads, payload) {
				payloads = append(payloads, payload)
			}
		}
		return payloads
	}
	return []reflect.Type{typ}
}

// declare records every field typ declares, and those of every type a field of it holds.
func (w *fieldWalk) declare(t *testing.T, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		if encodedWhole(typ) {
			return
		}
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || encodedWhole(typ) || w.visited[typ] {
		return
	}
	w.visited[typ] = true
	jsonFields(typ, func(owner reflect.Type, field reflect.StructField, name string) {
		w.declared[owner.String()+"."+name] = writtenField{
			empty: emptyForms(t, owner, field, name), settable: settable(field.Type.Kind()),
		}
		for _, held := range heldTypes(owner, name, field.Type) {
			w.declare(t, held)
		}
	})
}

// holdsNothing is whether a decoded JSON value is null, [] or {}.
func holdsNothing(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

// value walks one example's value at path, written from typ, recording each field it carries.
func (w *fieldWalk) value(t *testing.T, path string, typ reflect.Type, value any) {
	t.Helper()
	if value == nil || encodedWhole(typ) {
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			t.Errorf("%s is %T, where %s writes a list", path, value, typ)
			return
		}
		for index, item := range items {
			w.value(t, fmt.Sprintf("%s[%d]", path, index), typ.Elem(), item)
		}
		return
	case reflect.Map:
		entries, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s is %T, where %s writes an object", path, value, typ)
			return
		}
		for name, entry := range entries {
			w.value(t, path+"."+name, typ.Elem(), entry)
		}
		return
	case reflect.Struct:
	default:
		return
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Errorf("%s is %T, where %s writes an object", path, value, typ)
		return
	}
	jsonFields(typ, func(owner reflect.Type, field reflect.StructField, name string) {
		held, ok := object[name]
		if !ok {
			return
		}
		key := owner.String() + "." + name
		w.carried[key] = true
		if holdsNothing(held) {
			w.carriedEmpty[key] = true
		} else {
			w.carriedSet[key] = true
		}
		switch {
		case owner == walkedEnvelope && name == "payload":
			text, _ := held.(string)
			var event any
			if err := json.Unmarshal([]byte(text), &event); err != nil {
				t.Errorf("%s.payload is not the event's JSON: %v", path, err)
				return
			}
			w.value(t, path+".payload", walkedPublishedEvent, event)
		case owner == walkedEvent && name == "payload":
			typeName, _ := object["type"].(string)
			payload, ok := writtenPayloads[typeName]
			if !ok {
				t.Errorf("%s is a %q, which writtenPayloads names no payload type for", path, typeName)
				return
			}
			w.value(t, path+".payload", payload, held)
		default:
			w.value(t, path+"."+name, field.Type, held)
		}
	})
}
