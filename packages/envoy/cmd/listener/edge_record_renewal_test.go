//go:build natslive

package main

// A role claim renews its claimant's session record on whichever listener takes it: it reads the
// record and writes it back (roleSetHandler), and the write names the key it is stored under
// (session.SessionRegistry.PutWithin). The edge reads a record that names no key, which an earlier
// build wrote, as no project's, since such a record cannot be told from another session's record
// the server copied under this key. These rows hold that a renewal does not undo that: it carries
// the projects over only from a record that names its own key, and otherwise writes the projects
// of the token the claim was made with.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sharedRoleClaims is a shared listener's POST /v1/roles/set over the edge rig's stores: no edge
// mode, a machine of its own, and broker tokens checked as every listener that verifies them
// checks them (byToken). The bare handler is a request under the shared token, which proves no
// broker caller.
func (r *edgeRig) sharedRoleClaims(t *testing.T) (bare, byToken http.Handler) {
	t.Helper()
	logger := r.harness.config.logger
	deps := withRoleClaimDrain(t, &listenerDeps{client: r.harness.client, registry: r.deps.registry, sessions: r.deps.sessions})
	bare = roleSetHandler(deps, "shared-listener", logger)
	return bare, apiAuth(listenerAPIAuth{brokers: listenerBrokerTokens(t, r.broker), logger: logger}, bare)
}

// claimThrough sends sessionID's claim of role to handler, under token when it is not empty, and
// waits until the session registry reads sessionID's record as renewed by it: naming its key, at a
// stream sequence past the one it was stored at before. The claim's own answer is logged and not
// required: what the rows read is the record the claim wrote.
func (r *edgeRig) claimThrough(t *testing.T, handler http.Handler, token, sessionID, role string) session.SessionEntry {
	t.Helper()
	conn := r.harness.client.Conn
	before := testnats.LastSequence(t, conn, bus.SessionBucket, sessionID)
	body := fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role)
	request := httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body))
	if token != "" {
		request = brokerTokenRequest(t, r.caller, http.MethodPost, "/v1/roles/set", token, body)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	t.Logf("%s's claim of %s through the shared listener: status %d %s", sessionID, role, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	var renewed session.SessionEntry
	testwait.Eventually(t, sessionID+"'s record to be renewed by its claim of "+role, func() bool {
		if testnats.LastSequence(t, conn, bus.SessionBucket, sessionID) <= before {
			return false
		}
		entry, state := r.deps.sessions.Lookup(sessionID)
		renewed = entry
		return state == session.Live && entry.SessionID == sessionID
	})
	return renewed
}

// requireShown fails the test unless the session under token lists and reads other, and its send
// to other is taken.
func (r *edgeRig) requireShown(t *testing.T, token, sender, when, other string) {
	t.Helper()
	if listed := r.listedSessions(t, token); !slices.Contains(listed, other) {
		t.Errorf("%s, GET /v1/sessions lists %v, want %s", when, listed, other)
	}
	if recorder := r.call(t, http.MethodGet, token, "/v1/registry/"+other, ""); recorder.Code != http.StatusOK {
		t.Errorf("%s, GET /v1/registry/%s: status %d (%s), want 200", when, other, recorder.Code, recorder.Body.String())
	}
	sent := r.call(t, http.MethodPost, token, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, other, sender))
	if sent.Code != http.StatusOK {
		t.Errorf("%s, a send to %s: status %d (%s), want it taken", when, other, sent.Code, sent.Body.String())
	}
}

// Trace A of a copied record. S_source is an in-house session of PRIV whose record an earlier
// build wrote, naming no key; the server copies it onto the key of S_pub, an in-house session of
// PUB, with a direct get. The edge hides S_pub from S_priv, an outside session of PRIV, before
// the copy and after it, since the copy names no key. S_pub then claims a role through a shared
// listener under the shared token, which renews its record: carried over, the copy's projects
// named S_pub's key from then on, so the edge listed S_pub to S_priv and took its send. The
// renewal writes the projects of the claim's token instead, none, and S_pub stays hidden.
//
// The positive controls: S_peer, a session of PRIV whose record names its key, is shown to S_priv
// before and after its own renewal, which keeps its projects; and S_heir, a session of PRIV whose
// record an earlier build wrote, is hidden until it claims a role under its own PRIV token, and
// then shown, since the renewal writes that token's projects.
func TestARenewalBindsNoCopiedRecord(t *testing.T) {
	rig := newEdgeRig(t)
	const priv, pub, source, peer, heir = "ses_renew_priv", "ses_renew_pub", "ses_renew_source", "ses_renew_peer", "ses_renew_heir"
	token := rig.personToken(priv, "PRIV")
	requireStatus(t, rig.subscribe(t, token, priv), http.StatusOK, "S_priv's registration")
	rig.placeElsewhere(t, pub, "PUB")
	rig.placeElsewhere(t, source, "PRIV")
	rig.placeElsewhere(t, peer, "PRIV")
	rig.placeElsewhere(t, heir, "PRIV")
	conn := rig.harness.client.Conn
	bare, byToken := rig.sharedRoleClaims(t)

	rig.requireHidden(t, token, priv, "before the copy", pub)
	rig.requireShown(t, token, priv, "before S_peer's renewal", peer)

	rewriteAsEarlierBuild(t, conn, source)
	awaitEarlierBuildRecord(t, rig.deps.sessions, source)
	testnats.ServerCopiesKey(t, conn, bus.SessionBucket, pub, bus.SessionBucket, source)
	testwait.Eventually(t, "the session registry to read the copy of S_source's record at S_pub's key", func() bool {
		entry, state := rig.deps.sessions.Lookup(pub)
		return state == session.Live && entry.SessionID == "" && slices.Contains(entry.Projects, "PRIV")
	})
	rig.requireHidden(t, token, priv, "after the copy of S_source's earlier-build record onto S_pub's key", pub)

	renewed := rig.claimThrough(t, bare, "", pub, "renewal-pub-reviewer")
	if slices.Contains(renewed.Projects, "PRIV") {
		t.Errorf("S_pub's renewed record names projects %v: the copy's PRIV is bound to S_pub's key", renewed.Projects)
	}
	rig.requireHidden(t, token, priv, "after S_pub's role claim renewed the copy", pub)

	if kept := rig.claimThrough(t, bare, "", peer, "renewal-peer-reviewer"); !slices.Equal(kept.Projects, []string{"PRIV"}) {
		t.Errorf("S_peer's renewed record names projects %v, want [PRIV]: its record named its key", kept.Projects)
	}
	rig.requireShown(t, token, priv, "after S_peer's renewal", peer)

	rewriteAsEarlierBuild(t, conn, heir)
	awaitEarlierBuildRecord(t, rig.deps.sessions, heir)
	rig.requireHidden(t, token, priv, "before S_heir's renewal", heir)
	if bound := rig.claimThrough(t, byToken, rig.personToken(heir, "PRIV"), heir, "renewal-heir-reviewer"); !slices.Equal(bound.Projects, []string{"PRIV"}) {
		t.Errorf("S_heir's renewed record names projects %v, want [PRIV], its own token's", bound.Projects)
	}
	rig.requireShown(t, token, priv, "after S_heir's renewal under its own PRIV token", heir)
}

// D1-T: the role-claim refresh keeps the projects of the one record the stream's leader returns
// (session.SessionRegistry.StoredByWriter), never the direct get's. A direct get and the leader
// read are two reads of one key, and a writer's write landing between them makes them diverge. The
// registry's leader-read seam forces that divergence from a handler row: the direct get
// (bucketSessionEntry) reads the claimant's record naming its key and listing [PRIV PUB], while the
// leader read answers a writer's record of that key listing [PRIV]. After the claim the stored
// record lists [PRIV], the leader's. Control: with `entry.Projects = confirmed.Projects` removed
// (api.go), the refresh keeps the direct get's [PRIV PUB].
func TestARoleClaimRefreshKeepsTheLeaderRecordsProjects(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const claimant = "ses_d1t_claimant"
	rig.placeElsewhere(t, claimant, "PRIV", "PUB")
	bare, _ := rig.sharedRoleClaims(t)
	restore := rig.deps.sessions.SetStoredByWriterSeamForTest(claimant,
		session.SessionEntry{SessionID: claimant, MachineID: "shared-listener", Projects: []string{"PRIV"}}, true, nil)
	defer restore()
	renewed := rig.claimThrough(t, bare, "", claimant, "renewal-d1t-reviewer")
	if !slices.Equal(renewed.Projects, []string{"PRIV"}) {
		t.Errorf("the claim refresh stored projects %v, want the leader's [PRIV], not the direct get's [PRIV PUB]", renewed.Projects)
	}
}

// D1-r: "names its key" is judged on the record the leader returns, not on the direct get. The
// direct get reads the claimant's record naming its key and listing [PRIV PUB]; the leader read
// answers a writer's record that names NO key (session.DecodeEntry reads an empty SessionID)
// listing [PUB]. A record naming no key is no registration of this key, so its projects are not
// kept: the refresh writes the claim token's projects (registeringProjects, [PRIV] here). Control:
// `keepProjects = ok` without the `confirmed.SessionID == body.SessionID` test (api.go) keeps the
// no-key record's [PUB].
func TestARoleClaimRefreshDropsALeaderRecordNamingNoKey(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const claimant = "ses_d1r_claimant"
	rig.placeElsewhere(t, claimant, "PRIV", "PUB")
	_, byToken := rig.sharedRoleClaims(t)
	restore := rig.deps.sessions.SetStoredByWriterSeamForTest(claimant,
		session.SessionEntry{SessionID: "", MachineID: "shared-listener", Projects: []string{"PUB"}}, true, nil)
	defer restore()
	renewed := rig.claimThrough(t, byToken, rig.personToken(claimant, "PRIV"), claimant, "renewal-d1r-reviewer")
	if !slices.Equal(renewed.Projects, []string{"PRIV"}) {
		t.Errorf("the claim refresh stored projects %v, want the claim token's [PRIV]: a leader record naming no key must not have its projects kept", renewed.Projects)
	}
}

// H1/T1/N5: a role-claim refresh over a record the stream leader holds no writer's record for
// carries back none of that record's fields - only the machine, the self-subscription and the claim
// token's projects. A RePublish can store, at its own session's key, a record naming the key with a
// chosen title and directory, a non-consumer delivery that would pause its relay, a gap mark, a
// client instance id, capabilities, and display fields (Host, TmuxPlace, ConsumerStart, Plugin,
// PluginVersion); the refresh must write none of them back as the session's own registration, as the
// gap and renewal writes carry back nothing of a server-written record (the F3 rule). The
// leader-read seam answers no writer's record, as the leader does over a RePublish, while the direct
// get (bucketSessionEntry) still reads the forged fields. The whole written entry is compared, so a
// field left uncleared, or a new field added to SessionEntry later, fails the row. Control: carrying
// any one field (the api.go clearForged block keeping it) fails the compare.
func TestARoleClaimRefreshClearsAForgedRecordsFields(t *testing.T) {
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const claimant = "ses_h1_claimant"
	rig.placeElsewhere(t, claimant, "PRIV")
	if err := rig.deps.sessions.Put(claimant, session.SessionEntry{
		MachineID: "shared-listener", SelfSubscribed: true,
		Title: "forged-title", Dir: "/forged", Delivery: "broken", Epoch: "forged-epoch",
		ClientInstanceID: "forged-instance", Capabilities: []string{"forged"},
		Host: "forged-host", TmuxPlace: &session.TmuxPlace{}, ConsumerStart: 99,
		Plugin: "forged-plugin", PluginVersion: "9.9.9",
		Projects: []string{"PRIV"},
	}); err != nil {
		t.Fatalf("store the forged record for %s: %v", claimant, err)
	}
	if err := rig.deps.sessions.SetRepositoryGap(context.Background(), claimant, "forged-mark"); err != nil {
		t.Fatalf("store the forged gap mark for %s: %v", claimant, err)
	}
	testwait.Eventually(t, "the cache to hold the forged gap mark", func() bool {
		entry, _ := rig.deps.sessions.Lookup(claimant)
		return entry.RepositoryGap == "forged-mark"
	})
	_, byToken := rig.sharedRoleClaims(t)
	restore := rig.deps.sessions.SetStoredByWriterSeamForTest(claimant, session.SessionEntry{}, false, nil)
	defer restore()
	renewed := rig.claimThrough(t, byToken, rig.personToken(claimant, "PRIV"), claimant, "renewal-h1-reviewer")
	if renewed.UpdatedAt <= 0 {
		t.Fatalf("the refresh stamped no registration time on %s's record: %+v", claimant, renewed)
	}
	got := renewed
	got.UpdatedAt = 0
	want := session.SessionEntry{
		SessionID:      claimant,
		MachineID:      "shared-listener",
		SelfSubscribed: true,
		Projects:       []string{"PRIV"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the refresh over a forged record wrote %+v, want %+v: every field but the machine, self-subscription and the claim token's projects - the gap mark and the display fields Host, TmuxPlace, ConsumerStart, Plugin and PluginVersion among them - must be cleared",
			got, want)
	}
}
