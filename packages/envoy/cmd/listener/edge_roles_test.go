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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// The role rule: an edge session claims and reads only a role of its token's projects, the
// project contracts.ClaimProject gives it, and its claim is keyed by contracts.RoleClaimKey.
func TestEdgeHoldsRolesToTheTokensProjects(t *testing.T) {
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
	rig := newEdgeRigServing(t, nil)
	requireFreeFormRoleUnderItsProject(t, rig, "ses_ff_bare", "ses_ff_priv", "ses_ff_sender")
}

// At the edge, an edge session claims, reads and is reached through a free-form role of
// its token's project, and a role of the same name under no project, claimed elsewhere, keeps its
// own holder. The edge refuses the bare name, which works for no project, to claim and to publish.
func TestAnEdgeClaimsPublishesAndReadsAFreeFormRoleOfItsProject(t *testing.T) {
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
	if cfg.attemptCache.Seen(other.DedupeKey, holder) || cfg.dedupeCache.Seen(other.DedupeKey, holder) {
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

// A role read at the edge names a holder whose registering token listed none of the reader's
// projects without its place or process: a PRIV role held by a session another listener
// registered with no broker token, under a claim naming its client instance, is answered with its
// holder and claim, and with no directory, machine, host, title or client instance and no
// capabilities. A holder of the reader's project is answered whole.
func TestAnEdgeRoleReadNamesNoPlaceOfAHolderOutsideItsProjects(t *testing.T) {
	rig := newEdgeRig(t)
	const reader, internal, peer = "ses_edge_role_reader", "ses_edge_role_internal", "ses_edge_role_peer"
	const internalRole, peerRole = "legion-priv-priv-4-tester", "legion-priv-priv-5-tester"
	token := rig.personToken(reader, "PRIV")
	requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
	requireStatus(t, rig.subscribe(t, rig.namedToken(peer, "PRIV"), peer), http.StatusOK, "the peer's registration")
	if err := rig.deps.sessions.Put(internal, session.SessionEntry{MachineID: "internal-machine", Host: "internal-host", SelfSubscribed: true,
		Title: "an internal agent", Dir: "/home/agent/internal-checkout", Capabilities: []string{"tmux"}, Projects: []string{}}); err != nil {
		t.Fatalf("register %s elsewhere: %v", internal, err)
	}
	if _, err := rig.deps.registry.SetRoleWithPrevious(t.Context(), store.RoleClaimRequest{
		SessionID: internal, MachineID: "test-machine", ClientInstanceID: "internal-process", Role: internalRole,
	}); err != nil {
		t.Fatalf("claim %s for %s: %v", internalRole, internal, err)
	}
	if _, err := rig.deps.registry.SetRole(peer, "test-machine", peerRole, false); err != nil {
		t.Fatalf("claim %s for %s: %v", peerRole, peer, err)
	}
	read := func(role string) listenerwire.RoleHolder {
		t.Helper()
		recorder := rig.call(t, http.MethodGet, token, "/v1/roles/"+role, "")
		requireStatus(t, recorder, http.StatusOK, "GET /v1/roles/"+role)
		var answer listenerwire.RoleHolder
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode %s: %v", recorder.Body.String(), err)
		}
		return answer
	}
	held := read(internalRole)
	if held.Holder != internal || held.Dir != "" || held.Host != "" || held.MachineID != "" || held.Title != "" || held.ClientInstanceID != "" || len(held.Capabilities) != 0 {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s with no directory, host, machine, title, client instance or capabilities", internalRole, held, internal)
	}
	if whole := read(peerRole); whole.Holder != peer || whole.MachineID == "" || whole.Host == "" {
		t.Errorf("GET /v1/roles/%s answers %+v, want holder %s with its machine and host: it is a session of PRIV", peerRole, whole, peer)
	}
}

// A session that holds a role of another project when it registers at the edge, which another
// listener claimed for it, loses that role at the registration: the claim is released rather than
// renewed, and the answer's warnings name it, as a claim of that role at the edge is refused. A
// message on that role's lane after the registration then reaches no holder, and the session's
// role of the token's project stays and is renewed. A role message the lane forwarded to the
// session's agent subject before the registration, or after a later claim another writer makes,
// is not this row's: the relay's check of each frame's topic holds those frames. Controls: with
// edge mode off, and with the edge's release (edgeTopicHold.releaseRefusedRoles) releasing
// nothing, the INFRA claim still names the session after the registration, and the row fails.
func TestAnEdgeReleasesTheRoleClaimsOfOtherProjects(t *testing.T) {
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
	if cfg.attemptCache.Seen(item.DedupeKey, moved) || cfg.dedupeCache.Seen(item.DedupeKey, moved) {
		t.Fatalf("the role lane forwarded INFRA's %s message to %s, a session whose token lists only PRIV", infraRole, moved)
	}
	sentinel, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != sentinel.EventID {
		t.Fatalf("the moved session's first frame is %s on %s, want PRIV's event: a message of INFRA's role reached it", frames[0].item.EventID, frames[0].item.Topic)
	}
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
