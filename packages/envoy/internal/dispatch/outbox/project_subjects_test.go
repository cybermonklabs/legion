package outbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// subjectRecorder is a test publisher that records the subject of every send the outbox makes, a
// publish's (its envelope's topic) and a role forward's (the subject the Request goes to) alike,
// with the envelope's own topic beside it.
type subjectRecorder struct {
	publishOnly
	mu    sync.Mutex
	sends []recordedSend
}

// recordedSend is one send: the subject it went out on and the topic its envelope carries.
type recordedSend struct {
	subject, topic, event string
}

func (p *subjectRecorder) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *subjectRecorder) PublishAsync(item contracts.Envelope) <-chan error {
	if !bus.StreamCaptures(item.Topic) {
		return answered(fmt.Errorf("%w: %q", bus.ErrNotCaptured, item.Topic))
	}
	p.record(recordedSend{subject: item.Topic, topic: item.Topic, event: item.SourceEventID})
	return answered(nil)
}

func (p *subjectRecorder) Request(subject string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	p.record(recordedSend{subject: subject, topic: item.Topic, event: item.SourceEventID})
	return answered(nil)
}

func (p *subjectRecorder) record(send recordedSend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sends = append(p.sends, send)
}

func (p *subjectRecorder) all() []recordedSend {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.sends)
}

// missingTheirProject is every subject of names that does not name project as a token of its own
// (contracts.SubjectProject, contracts.CanonicalTopic): an issue event named by its key alone, a
// session's message or a role's lane under no project, or any of them under another project.
func missingTheirProject(names []string, project string) []string {
	var missing []string
	for _, name := range names {
		named, _, ok := contracts.SubjectProject(name)
		if _, legacy := contracts.CanonicalTopic(name); !ok || legacy || named != project {
			missing = append(missing, name)
		}
	}
	return missing
}

// projectFamilies are the families whose subject the outbox puts the event's project in, each by
// its prefix: an issue's topic, a session's own subject and a role's lane.
var projectFamilies = []string{contracts.DispatchIssueTopicPrefix, contracts.AgentTopicPrefix, contracts.RoleTopicPrefix}

// onBus is the subject the copy a record names went out on for an event of project: the record
// (model.IssueDestinationKey, SessionDestinationKey, RoleDestinationKey) with the project put after
// its family's prefix, as a test writes it out by hand. A Legion role's lane, whose project is its
// own name's (roleProject), is written out by its test instead.
func onBus(project string, records ...string) []string {
	subjects := make([]string, len(records))
	for i, record := range records {
		subjects[i] = record
		for _, prefix := range projectFamilies {
			if rest, ok := strings.CutPrefix(record, prefix); ok {
				subjects[i] = prefix + project + "." + rest
				break
			}
		}
	}
	return subjects
}

// recordOf is the record of the copy each subject names (model.*DestinationKey): the subject with
// its project token taken out, as published_destinations, owed_keys and comment_fallback_copies
// hold it.
func recordOf(subjects ...string) []string {
	records := make([]string, len(subjects))
	for i, subject := range subjects {
		records[i] = subject
		for _, prefix := range projectFamilies {
			if rest, ok := strings.CutPrefix(subject, prefix); ok {
				if _, after, found := strings.Cut(rest, "."); found {
					records[i] = prefix + after
				}
				break
			}
		}
	}
	return records
}

// TestEveryOutboxSubjectNamesItsProject: PUB's issues are routed to a free-form role, to a session
// and to a Legion role of another deployment, and each has a message. Every subject the outbox
// sends on names the project of the event it carries: the issue's topic is
// notifications.dispatch.issue.PUB.<KEY>.message.created, the session's copy goes to
// notifications.agent.PUB.s-route, and each role route's forward goes to
// notifications.agent.PUB.role-holder. The topic a role forward carries names the project the role
// works for: PUB for the free-form role, and OTHER, its own, for the Legion role of OTHER, whose
// claim is only ever made under that project (contracts.ClaimProject).
//
// Control, in the test: the same subjects in the form the record names them, with no project
// (contracts.LegacyTwin), fail the check, each named, so the check can see a subject with no
// project.
func TestEveryOutboxSubjectNamesItsProject(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	ctx := context.Background()
	const legionRole = "legion-other-pub-3-architect"
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, owner) values ('PUB', 'PUB', 'alice', 'alice')
	`); err != nil {
		t.Fatalf("create project PUB: %v", err)
	}
	routes := map[string]string{"PUB-1": "role:reviewer", "PUB-2": "session:s-route", "PUB-3": "role:" + legionRole}
	for number, key := range []string{"PUB-1", "PUB-2", "PUB-3"} {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			values ($1, 'PUB', $2, $1, $3, '{"kind":"user","id":"alice"}', 'U')
		`, key, number+1, routes[key]); err != nil {
			t.Fatalf("create issue %s: %v", key, err)
		}
	}
	byEvent := map[string]string{}
	for _, key := range []string{"PUB-1", "PUB-2", "PUB-3"} {
		byEvent[fmt.Sprint(appendHoldMessage(t, database, broker, key).ID)] = key
	}

	publisher := &subjectRecorder{}
	deps := withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker})
	if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}

	sent := publisher.all()
	var subjects []string
	got := map[string][]recordedSend{}
	for _, send := range sent {
		subjects = append(subjects, send.subject)
		got[byEvent[send.event]] = append(got[byEvent[send.event]], send)
	}
	if missing := missingTheirProject(subjects, "PUB"); len(missing) != 0 || len(subjects) == 0 {
		t.Fatalf("the outbox sent on %v, of which %v name no project or not PUB, want every one to name PUB", subjects, missing)
	}
	want := map[string][]recordedSend{
		"PUB-1": {
			{subject: contracts.DispatchIssueSubject("PUB", "PUB-1", "message.created"), topic: contracts.DispatchIssueSubject("PUB", "PUB-1", "message.created")},
			{subject: contracts.AgentSubject("PUB", roleHolder), topic: contracts.RoleSubject("PUB", "reviewer")},
		},
		"PUB-2": {
			{subject: contracts.DispatchIssueSubject("PUB", "PUB-2", "message.created"), topic: contracts.DispatchIssueSubject("PUB", "PUB-2", "message.created")},
			{subject: contracts.AgentSubject("PUB", "s-route"), topic: contracts.AgentSubject("PUB", "s-route")},
		},
		"PUB-3": {
			{subject: contracts.DispatchIssueSubject("PUB", "PUB-3", "message.created"), topic: contracts.DispatchIssueSubject("PUB", "PUB-3", "message.created")},
			{subject: contracts.AgentSubject("PUB", roleHolder), topic: contracts.RoleSubject("OTHER", legionRole)},
		},
	}
	for key, sends := range want {
		var have []string
		for _, send := range got[key] {
			have = append(have, send.subject+" (topic "+send.topic+")")
		}
		var wanted []string
		for _, send := range sends {
			wanted = append(wanted, send.subject+" (topic "+send.topic+")")
		}
		slices.Sort(have)
		slices.Sort(wanted)
		if !slices.Equal(have, wanted) {
			t.Errorf("%s's message went out on %v, want %v", key, have, wanted)
		}
	}

	var recordForm []string
	for _, subject := range subjects {
		if twin, ok := contracts.LegacyTwin(subject); ok {
			recordForm = append(recordForm, twin)
		}
	}
	if missing := missingTheirProject(recordForm, "PUB"); len(recordForm) == 0 || len(missing) != len(recordForm) {
		t.Fatalf("control: the check passed %d of the %d subjects in the form with no project (%v), want it to fail every one of them",
			len(recordForm)-len(missing), len(recordForm), strings.Join(recordForm, ", "))
	}
}

// TestADestinationKeyIsTheOneTheRecordAlreadyHolds (model.IssueDestinationKey,
// SessionDestinationKey, RoleDestinationKey): a key this build makes is byte for byte the one a
// release before it stored, and an event whose published_destinations a release before this one
// wrote is read as that release left it. PUB-1 is routed to session s1, and its message's record
// holds the issue's topic family, reached, as that release wrote it, in the form with no project.
// The batch sends the message to s1 alone, on s1's subject under PUB, and records s1's key beside
// the stored one: the topic family is not sent again.
//
// Control, in the test: the bus subject of each key, used as the key, is not what the record
// holds, so a build that recorded and read its bus subjects would send the topic family again.
func TestADestinationKeyIsTheOneTheRecordAlreadyHolds(t *testing.T) {
	stored := map[string]string{
		model.IssueDestinationKey("PUB-1", "message.created"): "notifications.dispatch.issue.PUB-1.message.created",
		model.SessionDestinationKey("s1"):                     "notifications.agent.s1",
		model.RoleDestinationKey("reviewer"):                  "notifications.role.reviewer",
	}
	for built, wrote := range stored {
		if built != wrote {
			t.Errorf("this build keys a destination %q, want %q, the bytes every release before it stored", built, wrote)
		}
		if bus := busSubject(built, "PUB"); bus == wrote {
			t.Errorf("control: the bus subject of %q is %q, the stored key itself, want one that names PUB", built, bus)
		}
	}

	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, owner) values ('PUB', 'PUB', 'alice', 'alice');
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('PUB-1', 'PUB', 1, 'PUB-1', 'session:s1', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create PUB-1: %v", err)
	}
	event := appendHoldMessage(t, database, broker, "PUB-1")
	owner := "notifications.dispatch.issue.PUB-1.message.created"
	if _, err := database.Pool.Exec(ctx, `
		update events set published_destinations = array[$2], attempt_count = 1 where id = $1
	`, event.ID, owner); err != nil {
		t.Fatalf("record the topic family reached, as the release before this one does: %v", err)
	}
	if slices.Contains(onBus("PUB", owner), owner) {
		t.Fatalf("control: the bus subject of %s is the stored key itself", owner)
	}

	publisher := &subjectRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker}
	if _, err := scanBatch(ctx, deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}
	var sent []string
	for _, send := range publisher.all() {
		sent = append(sent, send.subject)
	}
	if want := []string{contracts.AgentSubject("PUB", "s1")}; !slices.Equal(sent, want) {
		t.Fatalf("the batch sent %v, want %v alone: the topic family the stored record names reached is reached", sent, want)
	}
	destinations, _ := publishedDestinations(t, database, event.ID)
	if want := []string{owner, model.SessionDestinationKey("s1")}; !slices.Equal(destinations, want) {
		t.Fatalf("published_destinations = %v, want %v: the stored key and s1's beside it, in the form with no project", destinations, want)
	}
	if publishedAt(t, database, event.ID) == nil {
		t.Fatal("the message is unpublished, want it published once s1 is reached")
	}
}
