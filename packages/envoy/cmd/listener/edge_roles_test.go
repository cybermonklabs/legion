package main

// edge_roles_test.go holds the edge listener's rows for the roles a session brings to the edge
// and a role read naming a holder of no project of the reader's: the release at the registration
// of every claim of another project's role, also while the role claim copy refills, the 503 of a
// registration whose release fails, and the place of an outside holder left out of a read. The
// other request rules are in edge_mode_test.go, the GitHub rows in edge_repositories_test.go, and
// the rig every edge row runs on is in edge_rig_test.go.
//
// As there, every row reads the projects its listener serves from ENVOY_EDGE_PROJECTS when the
// test's own environment sets it, and serves PRIV otherwise (edgeTestProjects), so the control
// every row is run under once is the same command with ENVOY_EDGE_PROJECTS set empty.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The role rule: an edge session claims and reads only a role of its token's projects, the
// project contracts.ClaimProject gives it, and its claim is keyed by contracts.RoleClaimKey.
func TestEdgeHoldsRolesToTheTokensProjects(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_roles"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the registration")
	claim := func(role string) *httptest.ResponseRecorder {
		return rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, sessionID, role))
	}
	for _, role := range []string{"legion-infra-infra-1-tester", "INFRA.legion-infra-infra-1-tester", "lead-reviewer", "INFRA.lead-reviewer"} {
		requireEdgeRefusal(t, claim(role), http.StatusForbidden, "role_not_in_project", "a claim of "+role)
	}
	const own = "legion-priv-priv-1-tester"
	requireStatus(t, claim(own), http.StatusOK, "a claim of "+own)
	key := contracts.RoleClaimKey(mustClaimProject(t, own, ""), own)
	held, err := rig.deps.registry.RoleClaim(t.Context(), key)
	if err != nil || held.HolderSessionID != sessionID {
		t.Fatalf("the claim keyed %s holds %+v (%v), want %s", key, held, err, sessionID)
	}
	requireEdgeRefusal(t, rig.call(t, http.MethodGet, token, "/v1/roles/legion-infra-infra-1-tester", ""), http.StatusForbidden, "role_not_in_project", "a read of legion-infra-infra-1-tester")
	requireStatus(t, rig.call(t, http.MethodGet, token, "/v1/roles/"+own, ""), http.StatusOK, "a read of "+own)
}

func mustClaimProject(t *testing.T, role, requested string) string {
	t.Helper()
	project, err := contracts.ClaimProject(role, requested)
	if err != nil {
		t.Fatalf("ClaimProject(%s, %q): %v", role, requested, err)
	}
	return project
}

// On a listener in no edge mode, a free-form role claimed under a project is that
// project's own claim, keyed by contracts.RoleClaimKey (PRIV.lead-reviewer), beside the claim of
// the same name under no project (lead-reviewer), which keeps its bare key. Each is read, and
// reached by a publish and by the role lane, under its own project.
func TestAFreeFormRoleUnderAProjectIsClaimedReadAndReachedUnderThatProject(t *testing.T) {
	t.Parallel()
	rig := newEdgeRigServing(t, nil)
	requireFreeFormRoleUnderItsProject(t, rig, "ses_ff_bare", "ses_ff_priv", "ses_ff_sender")
}

// At the edge, an edge session claims, reads and is reached through a free-form role of
// its token's project, and a role of the same name under no project, claimed elsewhere, keeps its
// own holder. The edge refuses the bare name, which works for no project, to claim and to publish.
func TestAnEdgeClaimsPublishesAndReadsAFreeFormRoleOfItsProject(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const bare = "ses_edge_ff_bare"
	rig.placeElsewhere(t, bare, "INFRA")
	if _, err := rig.deps.registry.SetRole(bare, "shared-listener", "lead-reviewer", false); err != nil {
		t.Fatalf("claim lead-reviewer for %s: %v", bare, err)
	}
	const holder, sender = "ses_edge_ff_holder", "ses_edge_ff_sender"
	token := rig.personToken(holder, "PRIV")
	senderToken := rig.namedToken(sender, "PRIV")
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	requireStatus(t, rig.subscribe(t, token, holder), http.StatusOK, "the holder's registration")
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	requireEdgeRefusal(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":"lead-reviewer","soft":true}`, holder)),
		http.StatusForbidden, "role_not_in_project", "a claim of lead-reviewer")
	body := fmt.Sprintf(`{"topic":"notifications.role.lead-reviewer","message":"for no project's reviewer","source":"agent","source_session":%q}`, sender)
	requireEdgeRefusal(t, rig.call(t, http.MethodPost, senderToken, "/v1/messages/publish", body), http.StatusForbidden, "topic_not_published", "a publish to lead-reviewer")

	role := "PRIV.lead-reviewer"
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, holder, role)), http.StatusOK, "a claim of "+role)
	requireRoleHolder(t, rig, senderToken, role, holder)
	requireRoleKeys(t, rig, map[string]string{"PRIV.lead-reviewer": holder, "lead-reviewer": bare})
	published := publishToHeldRole(t, rig, senderToken, sender, contracts.RoleTopicPrefix+role)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), published)
}

// On every listener, a role message on one project's lane that names a Legion role of another
// project reaches no holder: the claim of a Legion role is keyed on its bare name, so without the
// rule a message on notifications.role.PRIV.legion-infra-reviewer would be forwarded to the holder
// of INFRA's legion-infra-reviewer. The role lane drops it before it resolves a holder, and the
// publish route refuses it 400, as POST /v1/roles/set refuses the claim of that role under PRIV.
// Control: a message on the role's own lane, before and after, reaches its holder.
func TestARoleLaneOfAnotherProjectReachesNoHolderOfItsLegionRole(t *testing.T) {
	t.Parallel()
	rig := newEdgeRigServing(t, nil)
	const holder, sender, role = "ses_lane_infra_holder", "ses_lane_sender", "legion-infra-reviewer"
	holderToken, senderToken := rig.personToken(holder, "INFRA"), rig.personToken(sender, "INFRA")
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	requireStatus(t, rig.subscribe(t, holderToken, holder), http.StatusOK, "the holder's registration")
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	requireStatus(t, rig.call(t, http.MethodPost, holderToken, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)),
		http.StatusOK, "the claim of "+role)
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	lane := func(project, key string) contracts.Envelope {
		t.Helper()
		item := listenerTestEnvelope(contracts.RoleSubject(project, role), key+"-"+id.New())
		coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
		return item
	}

	own := lane("INFRA", "lane-own")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), own)

	other := lane("PRIV", "lane-other")
	// The lane records its forward before it hands it on, in the call that took the message, so
	// a forward to the holder is seen here as soon as that call returns.
	if ledgerAttempted(t, cfg, other.DedupeKey, holder) || ledgerDelivered(t, cfg, other.DedupeKey, holder) {
		t.Fatalf("the role lane forwarded the message on %s to %s, the holder of INFRA's %s", other.Topic, holder, role)
	}
	after := lane("INFRA", "lane-after")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == other.EventID {
		t.Fatalf("the message on %s reached %s, the holder of INFRA's %s", other.Topic, holder, role)
	}
	requireFrames(t, frames, after)

	body := fmt.Sprintf(`{"topic":%q,"message":"for INFRA's reviewer, under PRIV","source":"agent","source_session":%q}`, contracts.RoleSubject("PRIV", role), sender)
	recorder := rig.call(t, http.MethodPost, senderToken, "/v1/messages/publish", body)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "works for project INFRA") {
		t.Fatalf("a publish to %s: status %d (%s), want 400 naming INFRA, the project the role works for",
			contracts.RoleSubject("PRIV", role), recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// The role lane's drop of a message whose lane names a Legion role under a project it does not
// work for is reported as every other role message that reaches no holder is: a delivery
// exception with the reason no_holder on the lane's exception twin, which a core NATS publisher
// watching notifications.envoy.exceptions.notifications.role.> reads. The probe reads the
// exceptions in the order the listener's one connection published them, so a sentinel published
// after the drop on that connection comes second. Control: with the drop logged and no exception
// published (roleLaneProjectError's branch of resolveRoleForward before this row), the sentinel is
// the first exception, and the row fails.
func TestARoleLaneDropOfALegionRoleOfAnotherProjectReportsNoHolder(t *testing.T) {
	t.Parallel()
	rig := newEdgeRigServing(t, nil)
	conn := rig.harness.client.Conn
	exceptions, err := conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the role lanes' exceptions: %v", err)
	}
	t.Cleanup(func() { _ = exceptions.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleSubject("PRIV", "legion-infra-reviewer"), "lane-dropped-"+id.New())
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	sentinel := bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + "sentinel"
	if err := conn.Publish(sentinel, []byte("{}")); err != nil {
		t.Fatalf("publish the sentinel exception: %v", err)
	}
	message, err := exceptions.NextMsg(10 * time.Second)
	if err != nil {
		t.Fatalf("UNKNOWN: the probe read no exception, not even the sentinel: %v", err)
	}
	if want := bus.ExceptionTopicPrefix + item.Topic; message.Subject != want {
		t.Fatalf("the first role lane exception is on %s, want %s: the lane dropped the message on %s and reported nothing", message.Subject, want, item.Topic)
	}
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the exception: %v", err)
	}
	var payload struct {
		Reason        string `json:"reason"`
		OriginalTopic string `json:"original_topic"`
	}
	if err := json.Unmarshal([]byte(exception.Payload), &payload); err != nil || payload.Reason != "no_holder" || payload.OriginalTopic != item.Topic {
		t.Fatalf("the exception's payload reads %+v (%v), want reason no_holder about %s", payload, err, item.Topic)
	}
}

// requireFreeFormRoleUnderItsProject is the free-form role row on rig: bare claims lead-reviewer,
// priv claims PRIV.lead-reviewer, both soft, and neither takes the other's role; a read, a publish
// through the route and a message on the role lane each reach the holder of their own name's
// claim.
func requireFreeFormRoleUnderItsProject(t *testing.T, rig *edgeRig, bare, priv, sender string) {
	t.Helper()
	tokens := map[string]string{}
	standIns := map[string]*sessionStandIn{}
	for _, sessionID := range []string{bare, priv, sender} {
		tokens[sessionID] = rig.personToken(sessionID, "PRIV")
		standIns[sessionID] = startSessionStandIn(t, rig.url, sessionID, answerDone)
		requireStatus(t, rig.subscribe(t, tokens[sessionID], sessionID), http.StatusOK, "the registration of "+sessionID)
	}
	for sessionID, role := range map[string]string{bare: "lead-reviewer", priv: "PRIV.lead-reviewer"} {
		recorder := rig.call(t, http.MethodPost, tokens[sessionID], "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, sessionID, role))
		requireStatus(t, recorder, http.StatusOK, sessionID+"'s soft claim of "+role)
	}
	requireRoleKeys(t, rig, map[string]string{"PRIV.lead-reviewer": priv, "lead-reviewer": bare})
	requireRoleHolder(t, rig, tokens[sender], "PRIV.lead-reviewer", priv)
	requireRoleHolder(t, rig, tokens[sender], "lead-reviewer", bare)

	toPriv := publishToHeldRole(t, rig, tokens[sender], sender, contracts.RoleSubject("PRIV", "lead-reviewer"))
	requireFrames(t, standIns[priv].collect(t, 1, 10*time.Second), toPriv)
	toBare := publishToHeldRole(t, rig, tokens[sender], sender, contracts.RoleTopicPrefix+"lead-reviewer")
	requireFrames(t, standIns[bare].collect(t, 1, 10*time.Second), toBare)

	// The role lane, which takes every role message published on the bus itself (Dispatch's
	// among them), resolves the claim the same way.
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleSubject("PRIV", "lead-reviewer"), "free-form-lane-"+priv)
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	frames := standIns[priv].collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != item.EventID {
		t.Fatalf("the role lane's message reached %s as %s, want %s", priv, frames[0].item.EventID, item.EventID)
	}
	// The lane's message reached bare not at all: an event stored for bare after it is bare's
	// next frame, since the relay sends in stream order and a forward is stored on bare's own
	// subject before the call that took the message returns.
	later, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(bare))
	if next := standIns[bare].collect(t, 1, 10*time.Second); next[0].item.EventID != later.EventID {
		t.Fatalf("%s's next frame is %s on %s, want the event stored after the role lane's message: that message reached the holder of the bare name", bare, next[0].item.EventID, next[0].item.Topic)
	}
}

// requireNamesNone fails the test unless body, an answer to asked, names none of names and has no
// field naming a holder, its process, its state or its last registration, nor a kept reason that
// says a holder lapsed.
func requireNamesNone(t *testing.T, asked, body string, names ...string) {
	t.Helper()
	for _, name := range names {
		if strings.Contains(body, name) {
			t.Errorf("%s answers %s, which names %s", asked, body, name)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("%s: decode %s: %v", asked, body, err)
	}
	for _, field := range []string{"holder", "holder_state", "client_instance_id", "last_seen", "handed_back"} {
		if value, ok := fields[field]; ok && string(value) != `""` {
			t.Errorf("%s answers %s, whose %s is %s: a holder outside the reader's projects is dated or named", asked, body, field, value)
		}
	}
	if reason, ok := fields["kept_reason"]; ok && string(reason) != `"`+string(roles.Unclaimed)+`"` {
		t.Errorf("%s answers %s, whose kept_reason is %s: a holder outside the reader's projects is said to stand", asked, body, reason)
	}
}

// stalledSessionGetKV is a session bucket whose read of key does not answer until released is
// closed, as a bucket whose direct get of one entry stopped answering; every other call is the
// bucket's own.
type stalledSessionGetKV struct {
	natsgo.KeyValue
	key      string
	released <-chan struct{}
}

func (kv stalledSessionGetKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key != kv.key {
		return kv.KeyValue.Get(key)
	}
	<-kv.released
	return nil, errors.New("the session bucket's read of the entry never answered")
}

// stalledSessionReadKV is stalledSessionGetKV whose read of key from the stream's leader
// (bus.LeaderReads) does not answer either, until released is closed or its context ends: a
// bucket whose stream stopped answering for that entry. A leader read of any other key goes to
// the stream through leader, a handle on the bucket that is no stall.
type stalledSessionReadKV struct {
	stalledSessionGetKV
	leader bus.KeyValue
}

func (kv stalledSessionReadKV) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	if key != kv.key {
		return kv.leader.NewestFromLeader(ctx, key)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-kv.released:
		return nil, errors.New("the session bucket's leader read of the entry never answered")
	}
}

// The edge names a role's holder only when that holder is a session of the reader's projects
// (edgeScope.holderConfirmed): a role of the reader's project can be claimed by any session, and its claim
// record can be written by any writer of the role bucket, so a holder outside the reader's
// projects is no holder the edge names, in a role read or in the answers of a role send. A PRIV
// role's claim names S_pub, a live in-house session of PUB, under a distinctive process, and
// another PRIV role's claim names S_gone, a PUB session with no record left. To S_priv, an outside
// session of PRIV, each role reads as one no claim holds: GET /v1/roles/<role> is the 404 of an
// unclaimed role, with no holder, process, state, last registration or claim revision, and its
// text names neither session; a role send to either role names no holder in its answer; and the
// 409 of a claim of S_pub's role, a soft one or one naming the revision 0 the read gave, names no
// holder either, nor the claim revision the read did not give. The positive control: a third PRIV
// role held by S_peer, a session of PRIV, is answered whole to the same token, a send to it names
// S_peer, and so do the 409 of a soft claim of it and the 409 of a claim naming revision 0, which
// gives the claim's revision too. A read of a holder's session that does not answer names no
// holder either: with the session bucket's read of S_gone stalled, GET /v1/roles/<role>, a role
// send to S_gone's role and a soft claim of it, with and without S_gone named as its predecessor,
// are each the 503 of a bus that did not answer, with Retry-After, in the listener's own words,
// where the read's own error names the session it read. Control: with edge mode off each answer
// names its holder.
func TestAnEdgeRoleAnswerNamesNoForeignHolder(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const reader, peer, foreign, gone = "ses_edge_answer_reader", "ses_edge_answer_peer", "ses_edge_answer_pub", "ses_edge_answer_pub_gone"
	const foreignInstance, goneInstance = "instance-of-the-pub-process", "instance-of-the-gone-process"
	const foreignRole, goneRole, peerRole = "legion-priv-priv-6-tester", "legion-priv-priv-7-tester", "legion-priv-priv-8-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	rig.placeElsewhere(t, foreign, "PUB")
	for _, claim := range []store.RoleClaimRequest{
		{SessionID: foreign, MachineID: "shared-listener", ClientInstanceID: foreignInstance, Role: foreignRole},
		{SessionID: gone, MachineID: "shared-listener", ClientInstanceID: goneInstance, Role: goneRole},
		{SessionID: peer, MachineID: "test-machine", Role: peerRole},
	} {
		if _, err := rig.deps.registry.SetRoleWithPrevious(context.Background(), claim); err != nil {
			t.Fatalf("claim %s for %s: %v", claim.Role, claim.SessionID, err)
		}
	}
	names := []string{foreign, gone, foreignInstance, goneInstance}

	for _, role := range []string{foreignRole, goneRole} {
		asked := "GET /v1/roles/" + role
		recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
		requireNamesNone(t, asked, recorder.Body.String(), names...)
		var refusal listenerwire.RoleRefusal
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		if recorder.Code != http.StatusNotFound || refusal.Reason != roles.Unclaimed || refusal.ClaimRevision != 0 || refusal.ClaimReleased != nil {
			t.Errorf("%s: status %d %s, want the 404 of a role no claim holds: reason unclaimed, claim revision 0",
				asked, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		}
	}

	recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+peerRole, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/roles/"+peerRole)
	var whole listenerwire.RoleHolder
	if err := json.Unmarshal(recorder.Body.Bytes(), &whole); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if whole.Holder != peer || whole.HolderState != session.Live || whole.MachineID == "" || whole.Host == "" || whole.LastSeen == nil || whole.ClaimRevision == 0 {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s whole: live, with its machine, host, last registration and claim revision", peerRole, whole, peer)
	}

	send := func(role, keep string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"topic":%q,"source":"agent","source_session":%q,"message":"to the role"%s}`,
			contracts.RoleTopicPrefix+"PRIV."+role, reader, keep)
		recorder := rig.call(t, http.MethodPost, token, "/v1/messages/publish", body)
		t.Logf("the send to %s%s: status %d %s", role, keep, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		return recorder.Code, recorder.Body.String()
	}
	for _, sent := range []struct{ role, keep string }{
		{foreignRole, ""},
		{goneRole, ""},
		{goneRole, `,"keep":false`},
	} {
		status, body := send(sent.role, sent.keep)
		if status/100 != 2 && status != http.StatusNotFound {
			t.Errorf("the send to %s%s: status %d %s, want it delivered, kept or refused as a role no claim holds", sent.role, sent.keep, status, body)
		}
		requireNamesNone(t, "the send to "+sent.role+sent.keep, body, names...)
	}
	status, body := send(peerRole, "")
	var delivered listenerwire.PublishResponse
	_ = json.Unmarshal([]byte(body), &delivered)
	if status != http.StatusOK || delivered.Holder != peer || delivered.HolderState != session.Live {
		t.Errorf("the send to %s: status %d %s, want it delivered to %s, named live", peerRole, status, body, peer)
	}

	claim := func(body string) (int, string) {
		t.Helper()
		recorder := rig.call(t, http.MethodPost, token, "/v1/roles/set", body)
		t.Logf("POST /v1/roles/set %s: status %d %s", body, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		return recorder.Code, recorder.Body.String()
	}
	for _, body := range []string{
		fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, foreignRole),
		fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, reader, foreignRole),
	} {
		status, answer := claim(body)
		if status != http.StatusConflict {
			t.Errorf("POST /v1/roles/set %s: status %d %s, want 409: the role is held", body, status, answer)
		}
		requireNamesNone(t, "POST /v1/roles/set "+body, answer, names...)
		var conflict map[string]json.RawMessage
		_ = json.Unmarshal([]byte(answer), &conflict)
		if revision, ok := conflict["claim_revision"]; ok {
			t.Errorf("POST /v1/roles/set %s answers %s, whose claim_revision is %s: the claim of a holder outside the reader's projects is given a revision its read gave as 0", body, answer, revision)
		}
	}
	if status, answer := claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, peerRole)); status != http.StatusConflict || !strings.Contains(answer, `"holder":"`+peer+`"`) {
		t.Errorf("a soft claim of %s: status %d %s, want the 409 naming %s, a session of PRIV", peerRole, status, answer, peer)
	}
	if status, answer := claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, reader, peerRole)); status != http.StatusConflict ||
		!strings.Contains(answer, `"holder":"`+peer+`"`) || !strings.Contains(answer, `"claim_revision":`+strconv.FormatUint(whole.ClaimRevision, 10)) {
		t.Errorf("a claim of %s at revision 0: status %d %s, want the 409 naming %s, a session of PRIV, and the claim's revision %d", peerRole, status, answer, peer, whole.ClaimRevision)
	}

	// The read of S_gone's session from the bucket, which a resolution makes for a holder the
	// listener's copy does not hold live (roleHolderSessions.Holder), stalls until the test ends,
	// and so does its read from the stream's leader, which a soft claim makes of the holder it
	// would take the role from (roleSupersessionVerdict).
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	swapSessionHandle(t, rig.deps.sessions, func(old bus.KeyValue) bus.KeyValue {
		stalled := stalledSessionReadKV{stalledSessionGetKV{KeyValue: old.Legacy(), key: gone, released: released}, old}
		return bus.NewKeyValue(stalled, old.WatchHandle(), old.Streams())
	})
	sent := fmt.Sprintf(`{"topic":%q,"source":"agent","source_session":%q,"message":"to the role"}`, contracts.RoleTopicPrefix+"PRIV."+goneRole, reader)
	for _, unread := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/roles/" + goneRole, ""},
		{http.MethodPost, "/v1/messages/publish", sent},
		{http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, reader, goneRole)},
		{http.MethodPost, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"previous_session_id":%q}`, reader, goneRole, gone)},
	} {
		asked := strings.TrimSpace(unread.method+" "+unread.path+" "+unread.body) + " while the read of the holder's session does not answer"
		recorder := rig.call(t, unread.method, token, unread.path, unread.body)
		t.Logf("%s: status %d %s", asked, recorder.Code, strings.TrimSpace(recorder.Body.String()))
		var refusal listenerwire.APIError
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" || !strings.HasPrefix(refusal.Error, busUnansweredPrefix) {
			t.Errorf("%s: status %d, Retry-After %q, %s; want the 503 of a bus that did not answer, with Retry-After: 1",
				asked, recorder.Code, recorder.Header().Get("Retry-After"), strings.TrimSpace(recorder.Body.String()))
		}
		requireNamesNone(t, asked, recorder.Body.String(), names...)
	}
}

// A role send whose forward the bus refuses names no holder the edge does not show: the refusal's
// own text names the subject the forward went to, the holder's agent subject, so at the edge the
// answer for a holder the scope does not show is the listener's own text, under the status the
// refusal gets (403 for a publish the server denied), and the refusal goes to the log. A holder
// the scope shows is answered with the refusal as it is. The forward is the edge's own only once
// a server denies it, which the edge's grant does not for an agent subject, so the row hands the
// send its refusal. Each holder's record is stored as another listener's registration, so the
// leader read that confirms a holder the scope shows (edgeScope.holderConfirmed) finds a writer's
// record. Control: with edge mode off, the scope shows every holder and the row fails.
func TestAnEdgeRefusedRoleForwardNamesNoHiddenHolder(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	var scope *edgeScope
	if rig.edgeOn() {
		scope = &edgeScope{projects: rig.projects, logger: rig.harness.config.logger}
	}
	const hidden, shown = "ses_edge_forward_pub", "ses_edge_forward_priv"
	for _, holder := range []struct {
		id       string
		projects []string
	}{{hidden, []string{"PUB"}}, {shown, []string{"PRIV"}}} {
		rig.placeElsewhere(t, holder.id, holder.projects...)
		recorder := httptest.NewRecorder()
		send := &roleSend{w: recorder, record: &requestRecord{logger: rig.harness.config.logger, line: "listener publish", laneKey: "topic"},
			d: rig.deps, ctx: t.Context(), scope: scope, role: "legion-priv-priv-5-tester", arrived: time.Now()}
		send.hold(roles.Resolution{State: roles.Live, Holder: holder.id, HolderState: session.Live, Entry: session.SessionEntry{SessionID: holder.id, Projects: holder.projects}})
		send.refused(fmt.Errorf("%w: nats: Permissions Violation for Publish to %q", bus.ErrPublishDenied, contracts.LegacyAgentSubject(holder.id)))
		body := strings.TrimSpace(recorder.Body.String())
		t.Logf("the refused forward to %s: status %d %s", holder.id, recorder.Code, body)
		if recorder.Code != http.StatusForbidden || !strings.Contains(body, publishDeniedPrefix) {
			t.Errorf("the refused forward to %s: status %d %s, want the 403 of a denied publish", holder.id, recorder.Code, body)
		}
		if names := strings.Contains(body, holder.id); names != (holder.id == shown) {
			t.Errorf("the refused forward to %s answers %s, which names the holder: %v; want it named only for the holder of PRIV", holder.id, body, names)
		}
	}
}

// A role send whose window ends after its last forward ended while an earlier forward under its
// Nats-Msg-Id was still in process (roleSend.maybeStored) names no holder the edge does not
// show either. That forward's error can name the holder's agent subject: a send of it after the
// stream's in-process refusal that publishConfirmed could not confirm carries the subject it went
// to. So at the edge the answer for a holder the scope does not show is the listener's own text,
// still the 503 maybe_stored with Retry-After that every listener gives such a send, and the
// error goes to the log; a holder the scope shows is answered with the error as it is. Each send
// runs through sendToRole, whose every forward ends in process until its window ends. Each holder
// is stored as another listener's registration (placeElsewhere), so the leader read that confirms
// a holder the scope shows (edgeScope.holderConfirmed) finds a writer's record. Control: with edge
// mode off, the scope shows every holder and the row fails.
func TestAnEdgeRoleSendThatEndedInProcessNamesNoHiddenHolder(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const hidden, shown = "ses_edge_in_process_pub", "ses_edge_in_process_priv"
	for _, holder := range []struct {
		id, project, role string
	}{{hidden, "PUB", "legion-priv-priv-5-tester"}, {shown, "PRIV", "legion-priv-priv-6-tester"}} {
		body := sendEndingInProcess(t, rig, holder.role, holder.id, holder.project, "")
		if names := strings.Contains(body, holder.id); names != (holder.id == shown) {
			t.Errorf("the send to %s that ended in process answers %s, which names the holder: %v; want it named only for the holder of PRIV", holder.id, body, names)
		}
	}
}

// sendEndingInProcess claims role for holder, a session of project another listener registered,
// and sends a message to role through sendToRole under the edge's scope (none under the control),
// with every forward ending while an earlier forward is in process, its error naming the subject
// it went to. With movedTo set, movedTo is a session of PRIV past the live window whose record the
// scope shows (confirmed before the send), and the role moves to it under the first forward. It
// fails t unless the answer is the 503 maybe_stored with
// Retry-After of an earlier attempt in process, and returns the answer's body.
func sendEndingInProcess(t *testing.T, rig *edgeRig, role, holder, project, movedTo string) string {
	t.Helper()
	var scope *edgeScope
	if rig.edgeOn() {
		scope = &edgeScope{projects: rig.projects, logger: rig.harness.config.logger}
	}
	rig.placeElsewhere(t, holder, project)
	if _, err := rig.deps.registry.SetRole(holder, "shared-listener", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	if movedTo != "" {
		var raw natsgo.KeyValue
		swapSessionBucket(t, rig.harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue { raw = kv; return kv })
		live, _ := rig.deps.sessions.Windows()
		// The record names its own key, so it is a registration the edge shows (edgeScope.shows):
		// a record naming no key is no session's, and the row could not tell the holder the forward
		// went to from the holder the send resolved last.
		lapsed, err := json.Marshal(session.SessionEntry{SessionID: movedTo, MachineID: "shared-listener", SelfSubscribed: true, Title: movedTo, Projects: []string{"PRIV"},
			UpdatedAt: time.Now().Add(-live - time.Minute).UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Put(movedTo, lapsed); err != nil {
			t.Fatalf("write %s's lapsed entry: %v", movedTo, err)
		}
		if !testwait.Holds(listenerWaitBound(t), testwait.Poll, func() bool { entry, _ := rig.deps.sessions.Lookup(movedTo); return entry.Title == movedTo }) {
			t.Fatalf("%s's entry never reached this listener's copy", movedTo)
		}
		// The send judges the holder it resolves on the record its resolution reads
		// (roleHolderSessions.Holder, which reads the bucket for a holder past the live window) through
		// edgeScope.holderConfirmed, so the guard reads the record and judges it the same way.
		read, err := roleHolderSessions{rig.deps.sessions}.Holder(context.Background(), movedTo)
		if err != nil {
			t.Fatalf("UNKNOWN: the resolution's read of %s: %v", movedTo, err)
		}
		judged, err := scope.holderConfirmed(context.Background(), rig.deps.sessions, roles.Resolution{State: roles.Lapsed, Holder: movedTo, Entry: read})
		if err != nil || judged.Holder != movedTo {
			t.Fatalf("UNKNOWN: the scope names %+v for %s, the holder the send resolves last (%v); want %s, so the row tells the holder the forward went to from the one the send resolved last", judged, movedTo, err, movedTo)
		}
	}
	var forwards atomic.Int32
	rig.deps.delivery.forwardRole = inlineForward(func(subject string, _ contracts.Envelope, _ string, _ time.Duration) error {
		if forwards.Add(1) == 1 && movedTo != "" {
			if _, err := rig.deps.registry.SetRole(movedTo, "shared-listener", role, false); err != nil {
				t.Errorf("move %s to %s: %v", role, movedTo, err)
			}
		}
		unconfirmed := fmt.Errorf("bus: %w: flush publish to %q: nats: timeout", bus.ErrPublishUnconfirmed, subject)
		return fmt.Errorf("%w: %w: %w", errEarlierAttemptInProcess, bus.ErrForwardNotStored, unconfirmed)
	})
	recorder := httptest.NewRecorder()
	record := &requestRecord{logger: rig.harness.config.logger, line: "listener publish", laneKey: "topic"}
	arrived := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), arrived.Add(publishDeliveryWindow))
	defer cancel()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-in-process")
	sendToRole(ctx, recorder, scope, record, rig.deps, item, senderOf(httptest.NewRequest(http.MethodPost, "/v1/messages/publish", nil), item), nil, role, false, arrived, nil)
	body := strings.TrimSpace(recorder.Body.String())
	t.Logf("the send to %s after %d forwards answered %d, Retry-After %q: %s", role, forwards.Load(), recorder.Code, recorder.Header().Get("Retry-After"), body)
	var answer listenerwire.APIError
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" ||
		answer.Code != listenerwire.CodeMaybeStored || !strings.HasPrefix(answer.Error, earlierAttemptInProcessPrefix) {
		t.Errorf("the send to %s: status %d, Retry-After %q, %s; want the 503 maybe_stored of an earlier attempt in process", role, recorder.Code, recorder.Header().Get("Retry-After"), body)
	}
	return body
}

// The same answer after the role has moved: the send's forward to a holder the edge does not show
// (PUB) ends while an earlier forward is in process, the role then moves to a holder the edge
// shows (PRIV) whose session is past the live window, so the send forwards nothing more, and its
// window ends. The 503 maybe_stored carries the first forward's error, which names the PUB
// holder's agent subject, so it is judged by the holder that forward went to, not by the holder
// the send resolved last: it names neither the hidden holder nor its subject. The later holder's
// record names its own key, and the scope is shown to name it before the send
// (edgeScope.namesHolderConfirmed), so a judgment by the later holder would let the error through.
// Controls: with edge mode off, the scope shows every holder and the row fails; with the error
// judged when the window ends, by the holder the send resolved last (the 503's builder calling
// shownForwardError, as the edge-mode change had it before its fix), the row fails.
func TestAnEdgeRoleSendJudgesAnInProcessErrorByTheHolderItsForwardWentTo(t *testing.T) {
	t.Parallel()
	const hidden, shown = "ses_edge_moved_in_process_pub", "ses_edge_moved_in_process_priv"
	body := sendEndingInProcess(t, newEdgeRig(t), "legion-priv-priv-7-tester", hidden, "PUB", shown)
	requireNamesNone(t, "the send's answer after the role moved", body, hidden)
}

// A sender's repeat after it took the role, an echo, is answered from the forward the route stored
// for the role's earlier holder (roleSend.deliveredHeld). When that forward cannot be read back, a
// verdict naming no sequence here, the 503's read error names the subject it read, the earlier
// holder's agent subject. At the edge, for an earlier holder the scope does not show (PUB), the
// answer is in the listener's own words, under the same status, Retry-After and prefix, and names
// neither that holder nor its subject, although the holder this send resolved, the sender itself
// (PRIV), is one the scope shows: each session's record is stored as another listener's
// registration (placeElsewhere), so the leader read that confirms a holder the scope shows
// (edgeScope.holderConfirmed, edgeScope.namesHolderConfirmed) finds a writer's record. Control:
// with edge mode off, the scope shows every holder and the row fails.
func TestAnEdgeEchoJudgesAnUnreadStoredForwardByTheEarlierHolderItWentTo(t *testing.T) {
	t.Parallel()
	const hidden, sender, role = "ses_edge_echo_unread_pub", "ses_edge_echo_unread_priv", "legion-priv-priv-8-tester"
	rig := newEdgeRig(t)
	var scope *edgeScope
	if rig.edgeOn() {
		scope = &edgeScope{projects: rig.projects, logger: rig.harness.config.logger}
	}
	rig.placeElsewhere(t, hidden, "PUB")
	rig.placeElsewhere(t, sender, "PRIV")
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, role+"-echo-unread")
	item.SourceSession = sender
	recorder := httptest.NewRecorder()
	send := &roleSend{w: recorder, record: &requestRecord{logger: rig.harness.config.logger, line: "listener publish", laneKey: "topic"}, d: rig.deps, ctx: t.Context(),
		scope: scope, item: item, sender: senderOf(httptest.NewRequest(http.MethodPost, "/v1/messages/publish", nil), item), role: role, arrived: time.Now()}
	send.hold(roles.Resolution{State: roles.Live, Holder: sender, HolderState: session.Live,
		Entry: session.SessionEntry{SessionID: sender, MachineID: "shared-listener", SelfSubscribed: true, Title: sender, Projects: []string{"PRIV"}}})
	if send.shown.Holder != sender {
		t.Fatalf("UNKNOWN: the send names holder %+v, not %s, so the row cannot tell the earlier holder's judgement from the resolved holder's", send.shown, sender)
	}
	send.deliveredHeld(t.Context(), hidden, 0, sendSkipped)
	body := strings.TrimSpace(recorder.Body.String())
	t.Logf("the echo whose earlier holder's forward was not read answered %d, Retry-After %q: %s", recorder.Code, recorder.Header().Get("Retry-After"), body)
	var answer listenerwire.APIError
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" || !strings.HasPrefix(answer.Error, storedCopyUnreadablePrefix) {
		t.Errorf("the echo: status %d, Retry-After %q, %s; want the 503 with Retry-After: 1 of a stored copy not read", recorder.Code, recorder.Header().Get("Retry-After"), body)
	}
	requireNamesNone(t, "the echo whose earlier holder's forward was not read", body, hidden)
}

// A session that holds a role of another project when it registers at the edge, which another
// listener claimed for it, loses that role at the registration: the claim is released rather than
// renewed, and the answer's warnings name it, as a claim of that role at the edge is refused. A
// message on that role's lane after the registration then reaches no holder, and the session's
// role of the token's project stays and is renewed. A role message the lane forwarded to the
// session's agent subject before the registration is withheld from it
// (TestAnEdgeWithholdsARoleMessageForwardedBeforeItsRegistration), and so is one forwarded under a
// later claim another writer makes: the relay's check of each frame's topic (edgeFrameVerdict)
// holds those frames. Controls: with edge mode off, and with the edge's release
// (edgeTopicHold.releaseRefusedRoles) releasing nothing, the INFRA claim still names the session
// after the registration, and the row fails.
func TestAnEdgeReleasesTheRoleClaimsOfOtherProjects(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const moved, privIssues = "ses_edge_role_moved", "notifications.dispatch.issue.PRIV.>"
	const infraRole, privRole = "legion-infra-infra-9-reviewer", "legion-priv-priv-9-tester"
	rig.placeElsewhere(t, moved)
	for _, role := range []string{infraRole, privRole} {
		if _, err := rig.deps.registry.SetRole(moved, "shared-listener", role, false); err != nil {
			t.Fatalf("claim %s for %s on the other listener: %v", role, moved, err)
		}
	}
	testwait.Eventually(t, "the role claim copy to name both claims", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && len(held) == 2
	})
	ctx := t.Context()
	before, err := rig.deps.registry.RoleClaim(ctx, privRole)
	if err != nil || before.HolderSessionID != moved {
		t.Fatalf("UNKNOWN: %s's claim before the registration reads %+v (%v), not %s's", privRole, before, err, moved)
	}

	standIn := startSessionStandIn(t, rig.url, moved, answerDone)
	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved, privIssues)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge")
	warnings := registrationWarnings(t, answer)
	requireWarning(t, warnings, "role "+infraRole+" was released")
	if slices.ContainsFunc(warnings, func(warning string) bool { return strings.Contains(warning, privRole) }) {
		t.Errorf("the registration's warnings %q name %s, a role of PRIV", warnings, privRole)
	}
	if infra, err := rig.deps.registry.RoleClaim(ctx, infraRole); err != nil || (infra.Revision != 0 && infra.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, infra, err, moved)
	}
	if priv, err := rig.deps.registry.RoleClaim(ctx, privRole); err != nil || priv.HolderSessionID != moved || priv.Revision <= before.Revision {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v), want %s's, renewed past revision %d", privRole, priv, err, moved, before.Revision)
	}

	// A message on the INFRA role's lane reaches no holder now: the lane records a forward before
	// it hands it on, in the call that took the message, and the PRIV event stored after it is
	// the session's first frame, since the relay sends in stream order.
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+infraRole, "edge-infra-role-"+id.New())
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	if ledgerAttempted(t, cfg, item.DedupeKey, moved) || ledgerDelivered(t, cfg, item.DedupeKey, moved) {
		t.Fatalf("the role lane forwarded INFRA's %s message to %s, a session whose token lists only PRIV", infraRole, moved)
	}
	sentinel, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != sentinel.EventID {
		t.Fatalf("the moved session's first frame is %s on %s, want PRIV's event: a message of INFRA's role reached it", frames[0].item.EventID, frames[0].item.Topic)
	}
}

// A role message the role lane forwarded to a session's own agent subject before its edge
// registration is withheld after that registration: its topic is of INFRA, a project the session's
// PRIV token does not list, so the edge's relay check of each frame's topic withholds it
// (edgeFrameVerdict), a frame already forwarded to the session's own subject held by it like any
// other. An edge listener runs with its grant, which turns the frame takeover off (takeOverFrame),
// so nothing copies the forward to the role's kept lane; the relay holds it in place. Here the
// session holds INFRA's role through another listener, the shared listener's role lane forwards a
// message of that role to it, and the session then registers at the edge under a PRIV token, which
// releases the INFRA claim. Its first frame is the PRIV sentinel, not the INFRA forward. Live
// control: the frame rule off at the relay (edgeWithholds back to the base's GitHub-only verdict)
// lets the INFRA forward through, and the row fails naming it.
func TestAnEdgeWithholdsARoleMessageForwardedBeforeItsRegistration(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID, infraRole = "ses_edge_forwarded_before", "legion-infra-infra-5-tester"
	rig.placeElsewhere(t, sessionID, "INFRA")
	if _, err := rig.deps.registry.SetRole(sessionID, "shared-listener", infraRole, false); err != nil {
		t.Fatalf("claim %s for %s on the other listener: %v", infraRole, sessionID, err)
	}
	testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(sessionID)
		return err == nil && slices.Contains(held, infraRole)
	})
	// The lane records the forward and stores it on the holder's own subject before the call that
	// took the message returns.
	cfg := wireTestRoleForwarder(t, rig.harness.config)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+infraRole, "edge-forwarded-before-"+id.New())
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Subject: item.Topic, Data: marshalListenerEnvelope(t, item)})
	if !ledgerAttempted(t, cfg, item.DedupeKey, sessionID) && !ledgerDelivered(t, cfg, item.DedupeKey, sessionID) {
		t.Fatalf("UNKNOWN: the role lane forwarded INFRA's %s message to no session, so the row has no frame to see", infraRole)
	}

	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, rig.personToken(sessionID, "PRIV"), sessionID, "notifications.dispatch.issue.PRIV.>"), http.StatusOK,
		"the session's registration at the edge")
	if claim, err := rig.deps.registry.RoleClaim(t.Context(), infraRole); err != nil || (claim.Revision != 0 && claim.HolderSessionID == sessionID) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): the role was not released, so a frame of it is not one the release left", infraRole, claim, err)
	}
	sentinel, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	// The relay sends in stream order, so the forward, stored first, would be the first frame were
	// it not withheld. Its role topic is of INFRA, a project the session's PRIV token does not list,
	// so the edge withholds it (edgeFrameVerdict): the session's first frame is the PRIV sentinel.
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), sentinel)
}

// A registration at the edge releases a claim of another project's role also while the role claim
// copy is refilling and names no role (store.Registry.RolesHeldBy refuses): the role's lane topic
// on the session's stored row, as this listener's copy of the row holds it before the
// registration plans anything (store.Registry.Cached), names the role then, and the read of the
// role bucket the release makes while the copy refuses names it too. Control: with the release
// weighing no stored row (store.Registry.ReleaseRefusedRoleClaims reading no topics in place of
// the cached row's) and reading no bucket, the claim stands, and the row fails; with either one
// off alone, the other still frees the claim.
func TestAnEdgeReleasesAnotherProjectsClaimWhileTheClaimViewRefills(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_role_refill", "legion-infra-infra-7-reviewer"
	rig.placeElsewhere(t, moved)
	if _, err := rig.deps.registry.SetRole(moved, "shared-listener", infraRole, false); err != nil {
		t.Fatalf("claim %s for %s on the other listener: %v", infraRole, moved, err)
	}
	testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && len(held) == 1
	})
	refilled := refuseRoleClaimView(t, rig.deps.registry)
	defer refilled()
	if _, err := rig.deps.registry.RolesHeldBy(moved); err == nil {
		t.Fatal("UNKNOWN: the role claim copy still answers, so the registration would read the claim from it")
	}

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge while the claim copy refills")
	requireWarning(t, registrationWarnings(t, answer), "role "+infraRole+" was released")
	if claim, err := rig.deps.registry.RoleClaim(t.Context(), infraRole); err != nil || (claim.Revision != 0 && claim.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, claim, err, moved)
	}
}

// A registration at the edge releases a claim of another project's role that the session's
// stored row does not name, also while the role claim copy is refilling and names no role
// (store.Registry.RolesHeldBy refuses): neither source names the claim then, so the release reads
// the role bucket itself, as a deregistration does when the copy refuses it. Controls: with edge
// mode off, and with the release taking the copy's refusal as no claims (no read of the bucket),
// the claim still names the session after the registration, and the row fails.
func TestAnEdgeReleasesAClaimNoRowNamesWhileTheClaimViewRefills(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_role_unnamed", "legion-infra-infra-8-reviewer"
	rig.placeElsewhere(t, moved)
	// A claim whose lane topic is on no row of the session: written to the role bucket alone, as
	// a grant through another listener whose row write was lost leaves it.
	record, err := json.Marshal(store.RoleClaim{HolderSessionID: moved, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode %s's claim: %v", infraRole, err)
	}
	if _, err := registryRoleKV(t, rig.deps.registry).Put(infraRole, record); err != nil {
		t.Fatalf("store %s's claim for %s: %v", infraRole, moved, err)
	}
	testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && slices.Contains(held, infraRole)
	})
	if row, ok := rig.deps.registry.Cached(moved); ok && slices.Contains(row.Topics, contracts.RoleTopicPrefix+infraRole) {
		t.Fatalf("UNKNOWN: %s's stored row names %s's lane (%v), so the release would read the claim from it", moved, infraRole, row.Topics)
	}
	refilled := refuseRoleClaimView(t, rig.deps.registry)
	defer refilled()
	if _, err := rig.deps.registry.RolesHeldBy(moved); err == nil {
		t.Fatal("UNKNOWN: the role claim copy still answers, so the registration would read the claim from it")
	}

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge while the claim copy refills")
	requireWarning(t, registrationWarnings(t, answer), "role "+infraRole+" was released")
	if claim, err := rig.deps.registry.RoleClaim(t.Context(), infraRole); err != nil || (claim.Revision != 0 && claim.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, claim, err, moved)
	}
}

// A claim of another project's role that the edge cannot release refuses the registration with
// the retryable 503, and nothing of the registration is stored: the claim stands, and nothing else
// frees it while the session lives. Here the claim's record cannot be read, so the release cannot
// tell that it is not the session's, and the 503 names that record rather than a bus that did not
// answer (edgeReleaseFailedPrefix). Controls: with edge mode off the registration is taken; with
// the release's failure answered as a bus that did not answer (writeBusUnavailable), the text
// check fails.
func TestAnEdgeRegistrationWhoseRoleReleaseFailsIsRefused(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_release_fails", "legion-infra-infra-6-reviewer"
	rig.placeElsewhereFollowing(t, moved, []string{}, contracts.RoleTopicPrefix+infraRole)
	// JSON that is not a claim is a record no read can decode (store.ErrMalformedRoleClaim); a
	// string that is not JSON would read as an earlier build's bare holder id.
	if _, err := registryRoleKV(t, rig.deps.registry).Put(infraRole, []byte(`{"holder_session_id":7}`)); err != nil {
		t.Fatalf("store an unreadable claim of %s: %v", infraRole, err)
	}
	before := storedEdgeSessionEntry(t, rig, moved)

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	if answer.Code != http.StatusServiceUnavailable || answer.Header().Get("Retry-After") == "" {
		t.Fatalf("the registration whose release of %s fails: status %d, Retry-After %q (%s), want the retryable 503",
			infraRole, answer.Code, answer.Header().Get("Retry-After"), strings.TrimSpace(answer.Body.String()))
	}
	// The bus answered, with a record no read can decode, so the answer names that record and
	// says the claim stands; the bus-unavailable text would send an operator after an outage that
	// never happened.
	var refusal listenerwire.APIError
	if err := json.Unmarshal(answer.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", answer.Body.String(), err)
	}
	if strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.HasPrefix(refusal.Error, edgeReleaseFailedPrefix) ||
		!strings.Contains(refusal.Error, "read role "+infraRole+"'s claim") || !strings.Contains(refusal.Error, store.ErrMalformedRoleClaim.Error()) {
		t.Fatalf("the refusal reads %q, want it to begin %q and name %s's unreadable claim record (%v), not a bus that did not answer",
			refusal.Error, edgeReleaseFailedPrefix, infraRole, store.ErrMalformedRoleClaim)
	}
	if after := storedEdgeSessionEntry(t, rig, moved); after.MachineID != before.MachineID || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("the refused registration wrote %s's entry: %+v, want it left at %+v", moved, after, before)
	}
}
