package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The subjects the relay rows store: a Dispatch issue event of each project, which a session
// following notifications.dispatch.issue.> takes on its consumer, and which the relay decides by
// the project the subject names (contracts.SubjectProject).
const (
	privIssueSubject = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	pubIssueSubject  = "notifications.dispatch.issue.PUB.PUB-1.comment.created"
	everyIssueTopic  = "notifications.dispatch.issue.>"
)

// privacyRig is a listener's /v1 and session relays in a privacy mode, every piece real but the
// broker and the privacy source: the edge rig of a listener serving no edge projects (its broker
// stub and credential), the privacy source's stand-in (privacySource), and the listener's privacy
// mode wired as main wires it, its counters on the rig's registry.
type privacyRig struct {
	*edgeRig
	source  *privacySource
	privacy *listenerPrivacy
}

// newPrivacyRig is the rig of a listener in the required mode, or the all-public mode when
// required is false; its source answers that no project is private until a row says otherwise.
func newPrivacyRig(t *testing.T, required bool) *privacyRig {
	t.Helper()
	edge := newEdgeRigServing(t, nil)
	source := newPrivacySource(t, privacyNone)
	logger := edge.harness.config.logger
	privacy := &listenerPrivacy{required: required, counters: newPrivacyCounters(edge.met), logger: logger}
	if required {
		privacy.k7 = newK7Reader(source.server.URL, listenerBrokerAccount(edgeAccountToken), logger)
	}
	edge.deps.privacy = privacy
	edge.relays.setPrivacy(privacy)
	return &privacyRig{edgeRig: edge, source: source, privacy: privacy}
}

// memberToken is a token the broker answers as session's, acting for a person who is a restricted
// member or a full member, its project scope person and its list projects.
func (r *privacyRig) memberToken(session string, restricted bool, projects ...string) string {
	token := "member-token-of-" + session
	answer := sessionAnswer(session, "member@example.com", r.caller.JKT())
	answer.Projects = slices.Sorted(slices.Values(projects))
	answer.Restricted = new(restricted)
	r.broker.Says(token, answer)
	return token
}

// session registers sessionID for consumer delivery under a member token, following topics, and
// returns the stand-in its frames reach.
func (r *privacyRig) session(t *testing.T, sessionID string, restricted bool, projects []string, topics ...string) *sessionStandIn {
	t.Helper()
	standIn := startSessionStandIn(t, r.url, sessionID, answerDone)
	requireStatus(t, r.subscribe(t, r.memberToken(sessionID, restricted, projects...), sessionID, topics...), http.StatusOK, "the registration of "+sessionID)
	return standIn
}

// counter is the value the rig's /metrics reports for series, which must be present.
func (r *privacyRig) counter(t *testing.T, series string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return counterValue(t, recorder.Body.String(), series)
}

// requireHoldGap fails the test unless frame carries the privacy hold's gap mark.
func requireHoldGap(t *testing.T, frame relayedFrame, asked string) {
	t.Helper()
	if frame.msg.Header.Get(relayHeaderGap) != "true" || frame.msg.Header.Get(relayHeaderGapReason) != privacyHoldGap {
		t.Fatalf("%s: frame %d carries Envoy-Gap %q and Envoy-Gap-Reason %q, want true and %s", asked, frame.seq,
			frame.msg.Header.Get(relayHeaderGap), frame.msg.Header.Get(relayHeaderGapReason), privacyHoldGap)
	}
}

// requireNoGap fails the test when frame carries a gap mark.
func requireNoGap(t *testing.T, frame relayedFrame, asked string) {
	t.Helper()
	if gap := frame.msg.Header.Get(relayHeaderGap); gap != "" {
		t.Fatalf("%s: frame %d carries Envoy-Gap %q (reason %q), want none", asked, frame.seq, gap, frame.msg.Header.Get(relayHeaderGapReason))
	}
}

// TestTheHoldDoesNotStall: 1,100 PRIV frames are held for E while the privacy read is unknown,
// then E's own frame and a PUB frame, which E's token lists: E receives both at once, the first
// marked Envoy-Gap with reason privacy_hold once a full buffer was dropped, the second unmarked,
// and no PRIV frame; the hold dropped four full lanes (1,024 frames) and kept the last 76; when a
// read then answers PRIV public, E receives those 76 in stream order. Held frames are acknowledged
// upstream, so the consumer's MaxAckPending (1,024) never fills with them. Control
// (TestTheHoldDoesNotStallControl): with a lane wide enough to hold all 1,100, nothing is dropped,
// and E's own frame carries no gap mark, so the gap row fails for the drop it names.
func TestTheHoldDoesNotStall(t *testing.T) {
	rig, standIn, priv := holdPRIVFrames(t, 1100, holdLimits.laneFrames)
	own, _ := rig.storeEvent(t, contracts.LegacyAgentSubject("ses_hold_e"))
	pub, _ := rig.storeEvent(t, pubIssueSubject)
	frames := standIn.collect(t, 2, 60*time.Second)
	requireFrames(t, frames, own, pub)
	requireHoldGap(t, frames[0], "E's own frame after the drops")
	requireNoGap(t, frames[1], "the PUB frame after it")
	if dropped := rig.counter(t, `envoy_listener_held_dropped_total{reason="`+heldDroppedLane+`"}`); dropped != 1024 {
		t.Fatalf("the hold dropped %d frames as full lanes, want 1024 (four lanes of 256)", dropped)
	}
	rig.source.answer(privacyNone)
	requireFrames(t, standIn.collect(t, 76, 60*time.Second), priv[1024:]...)
	standIn.none(t, time.Second)
}

// TestTheHoldDoesNotStallControl is TestTheHoldDoesNotStall's control: a lane wide enough for every
// frame drops none, so E's own frame carries no gap mark, and all 1,100 PRIV frames reach E once a
// read answers.
func TestTheHoldDoesNotStallControl(t *testing.T) {
	rig, standIn, priv := holdPRIVFrames(t, 1100, 2048)
	own, _ := rig.storeEvent(t, contracts.LegacyAgentSubject("ses_hold_e"))
	frames := standIn.collect(t, 1, 60*time.Second)
	requireFrames(t, frames, own)
	requireNoGap(t, frames[0], "control: E's own frame with no frame dropped")
	rig.source.answer(privacyNone)
	requireFrames(t, standIn.collect(t, len(priv), 60*time.Second), priv...)
}

// holdPRIVFrames is the start both hold rows share: a required-mode rig whose lanes hold
// laneFrames, E registered as a full member's session listing PUB and following every issue event,
// the source failing, and n PRIV frames stored, which the relay holds for E.
func holdPRIVFrames(t *testing.T, n, laneFrames int) (*privacyRig, *sessionStandIn, []contracts.Envelope) {
	t.Helper()
	limits := holdLimits
	holdLimits.laneFrames = laneFrames
	holdLimits.redecide = 500 * time.Millisecond
	t.Cleanup(func() { holdLimits = limits })
	rig := newPrivacyRig(t, true)
	standIn := rig.session(t, "ses_hold_e", false, []string{"PUB"}, everyIssueTopic)
	rig.source.failWith(http.StatusBadGateway)
	priv := make([]contracts.Envelope, 0, n)
	for range n {
		item, _ := rig.storeEvent(t, privIssueSubject)
		priv = append(priv, item)
	}
	return rig, standIn, priv
}

// TestARestartWhileFramesAreHeldMarksTheGap: a listener that starts relaying a session whose record
// carries the held marker (its predecessor held frames for it, which are lost) marks the session's
// first frame Envoy-Gap with reason privacy_hold and clears the marker. Control: the same session
// with no marker gets its first frame unmarked, so the row fails for the marker it names.
func TestARestartWhileFramesAreHeldMarksTheGap(t *testing.T) {
	for _, marked := range []bool{true, false} {
		t.Run(fmt.Sprintf("held marker %v", marked), func(t *testing.T) {
			rig := newPrivacyRig(t, true)
			sessionID := fmt.Sprintf("ses_restart_held_%v", marked)
			// The record is written, then its marker by SetHeld, the marker's one writer: every
			// other write of the record (Put, PutWithin) carries the marker of the record it
			// replaces, so a Put naming Held sets nothing.
			if err := rig.deps.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", Projects: []string{}, Restricted: new(false)}); err != nil {
				t.Fatalf("write the predecessor's record: %v", err)
			}
			if err := rig.deps.sessions.SetHeld(t.Context(), sessionID, marked); err != nil {
				t.Fatalf("write the predecessor's held marker: %v", err)
			}
			standIn := rig.session(t, sessionID, false, nil)
			own, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
			frames := standIn.collect(t, 1, 30*time.Second)
			requireFrames(t, frames, own)
			if !marked {
				requireNoGap(t, frames[0], "control: the first frame of a session with no held marker")
				return
			}
			requireHoldGap(t, frames[0], "the first frame after a restart that lost held frames")
			testwait.Eventually(t, "the held marker to be cleared", func() bool {
				entry, err := rig.deps.sessions.Get(sessionID)
				return err == nil && !entry.Held
			})
		})
	}
}

// TestEveryTokenGetsTheGitHubEventsItsInterestNames: in the required mode a GitHub event is a
// message of no project that every token sees, so each session whose interest names its
// repository gets it, whatever its token lists or whether its person is restricted, and no read
// of the privacy source decides it. Controls, each breaking the interest check: a session that
// follows another repository, and one that follows no GitHub topic, get nothing.
func TestEveryTokenGetsTheGitHubEventsItsInterestNames(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(privacyListingPRIV)
	widgets := "notifications.github.acme.widgets.>"
	restricted := rig.session(t, "ses_github_restricted", true, []string{"PRIV"}, widgets)
	full := rig.session(t, "ses_github_full", false, nil, widgets)
	other := rig.session(t, "ses_github_other_repo", true, []string{"OPS"}, "notifications.github.acme.gadgets.>")
	none := rig.session(t, "ses_github_none", false, nil)
	before := rig.source.reads()
	event := rig.storeGithubEvent(t, "acme/widgets", "pull_request.4.opened")
	requireFrames(t, restricted.collect(t, 1, 30*time.Second), event)
	requireFrames(t, full.collect(t, 1, 30*time.Second), event)
	other.none(t, time.Second)
	none.none(t, time.Second)
	if reads := rig.source.reads(); reads != before {
		t.Fatalf("relaying a GitHub event read the privacy source %d times, want none", reads-before)
	}
}

// TestARouteAnswers503WhileK7CannotAnswer: with the privacy read unknown, POST /v1/roles/set of a
// PRIV role by a full member's session whose token lists no project answers 503 refilling and
// writes no claim. Control: the source answering that no project is private; the claim is
// written, so the row fails for the unknown read it names.
func TestARouteAnswers503WhileK7CannotAnswer(t *testing.T) {
	rig := newPrivacyRig(t, true)
	const holder = "ses_route_503"
	token := rig.memberToken(holder, false)
	requireStatus(t, rig.subscribe(t, token, holder), http.StatusOK, "the holder's registration")
	claim := fmt.Sprintf(`{"session_id":%q,"role":"PRIV.lead-reviewer","soft":true}`, holder)
	rig.source.failWith(http.StatusBadGateway)
	answer := rig.call(t, http.MethodPost, token, "/v1/roles/set", claim)
	requireStatus(t, answer, http.StatusServiceUnavailable, "a claim of PRIV.lead-reviewer while the privacy read fails")
	if got, err := rig.deps.registry.RoleClaim(t.Context(), contracts.RoleClaimKey("PRIV", "lead-reviewer")); err != nil || got.HolderSessionID != "" {
		t.Fatalf("the refused claim wrote holder %q, %v; want no claim", got.HolderSessionID, err)
	}
	rig.source.answer(privacyNone)
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", claim), http.StatusOK, "control: the same claim once the source answers")
	if got, err := rig.deps.registry.RoleClaim(t.Context(), contracts.RoleClaimKey("PRIV", "lead-reviewer")); err != nil || got.HolderSessionID != holder {
		t.Fatalf("control: the claim wrote holder %q, %v; want %s", got.HolderSessionID, err, holder)
	}
}

// privacyTeamTopic is a topic of no project every relay row's session follows.
const privacyTeamTopic = "notifications.team.privacy-rows"

// expectOnly fails the test unless standIn receives exactly want, in stream order, and nothing more
// within a short window.
func expectOnly(t *testing.T, standIn *sessionStandIn, asked string, want ...contracts.Envelope) {
	t.Helper()
	if len(want) > 0 {
		requireFrames(t, standIn.collect(t, len(want), 30*time.Second), want...)
	}
	select {
	case frame := <-standIn.frames:
		t.Fatalf("%s: the session received frame %d (%s) beyond %d expected", asked, frame.seq, frame.item.Topic, len(want))
	case <-time.After(700 * time.Millisecond):
	}
}

// TestTheRelayDecidesEachFrameFromAFreshRead: the relay over real session consumers decides each
// frame of a project from a fresh privacy read, by the member rule. E is a full member's session
// whose token lists no project, V a full member's listing PRIV, R a restricted member's listing
// PRIV. With one answer listing PRIV, a PRIV frame reaches V and R and is withheld from E, and a
// PUB frame reaches E and V and never R (a full member's list limits only private projects; a
// restricted member's limits every project). After PUB's switch to private, the next PUB frame is
// withheld from V and E, and R receives neither. After PRIV's switch to public, the next PRIV frame
// reaches E. With no privacy answer, a frame of no project reaches all three at once, a PRIV frame
// reaches V and R at once and is held for E, and a PUB frame is held for E and V and never reaches
// R. Each row's control is the row beside it: the same session before the switch, or the other
// member kind, gets the other answer, so no row passes for a reason it does not name.
func TestTheRelayDecidesEachFrameFromAFreshRead(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(privacyListingPRIV)
	decisions := func() int64 {
		return rig.counter(t, `envoy_listener_project_decisions_total{answer="public"}`) + rig.counter(t, `envoy_listener_project_decisions_total{answer="private"}`)
	}
	if decisions() != 0 {
		t.Fatal("the decision counters are not present at 0 before the first frame")
	}
	e := rig.session(t, "ses_rows_e", false, nil, everyIssueTopic, privacyTeamTopic)
	v := rig.session(t, "ses_rows_v", false, []string{"PRIV"}, everyIssueTopic, privacyTeamTopic)
	r := rig.session(t, "ses_rows_r", true, []string{"PRIV"}, everyIssueTopic, privacyTeamTopic)

	// One answer listing PRIV.
	priv, _ := rig.storeEvent(t, privIssueSubject)
	pub, _ := rig.storeEvent(t, pubIssueSubject)
	expectOnly(t, v, "V, one answer listing PRIV", priv, pub)
	expectOnly(t, r, "R, one answer listing PRIV", priv)
	expectOnly(t, e, "E, one answer listing PRIV", pub)
	if decisions() == 0 {
		t.Fatal("the frames decided from a read counted no decision")
	}

	// PUB switched private: the next PUB frame reaches none of the three.
	rig.source.answer(`{"projects":[{"project":"PRIV","slack_channels":[],"slack_channels_changed_at":null},{"project":"PUB","slack_channels":[],"slack_channels_changed_at":null}]}`)
	rig.storeEvent(t, pubIssueSubject)
	expectOnly(t, v, "V, after PUB's switch to private")
	expectOnly(t, e, "E, after PUB's switch to private")
	expectOnly(t, r, "R, after PUB's switch to private")

	// PRIV switched public: the next PRIV frame reaches E.
	rig.source.answer(privacyNone)
	privPublic, _ := rig.storeEvent(t, privIssueSubject)
	expectOnly(t, e, "E, after PRIV's switch to public", privPublic)
	expectOnly(t, v, "V, after PRIV's switch to public", privPublic)
	expectOnly(t, r, "R, after PRIV's switch to public", privPublic)

	// No privacy answer.
	rig.source.failWith(http.StatusBadGateway)
	team, _ := rig.storeEvent(t, privacyTeamTopic)
	for name, standIn := range map[string]*sessionStandIn{"E": e, "V": v, "R": r} {
		requireFrames(t, standIn.collect(t, 1, 10*time.Second), team)
		_ = name
	}
	privHeld, _ := rig.storeEvent(t, privIssueSubject)
	rig.storeEvent(t, pubIssueSubject)
	expectOnly(t, v, "V, no privacy answer", privHeld)
	expectOnly(t, r, "R, no privacy answer", privHeld)
	expectOnly(t, e, "E, no privacy answer")
}

// TestRolesSetRefusesAProjectTheTokenDoesNotSee: in the required mode with PUB public, a claim of a
// PUB role by a restricted member's session whose token lists INFRA answers 403
// project_not_listed and writes no claim, and so does one whose token lists INFRA and carries no
// restricted claim; one by a full member's session listing INFRA is taken, and so is one by a
// restricted member's session listing PUB. In the all-public mode all four are taken - the
// control under which the restricted INFRA claim is written - and the full member's row is the
// control for a check that restricts every project to the list.
func TestRolesSetRefusesAProjectTheTokenDoesNotSee(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %v", required), func(t *testing.T) {
			rig := newPrivacyRig(t, required)
			for _, row := range []struct {
				name, sessionID, role string
				restricted            *bool
				projects              []string
				refused               bool
			}{
				{"a restricted member listing INFRA", "ses_claim_restricted_infra", "PUB.reviewer-a", new(true), []string{"INFRA"}, true},
				{"a token listing INFRA with no restricted claim", "ses_claim_unmarked_infra", "PUB.reviewer-b", nil, []string{"INFRA"}, true},
				{"a full member listing INFRA", "ses_claim_full_infra", "PUB.reviewer-c", new(false), []string{"INFRA"}, false},
				{"a restricted member listing PUB", "ses_claim_restricted_pub", "PUB.reviewer-d", new(true), []string{"PUB"}, false},
			} {
				token := rig.personToken(row.sessionID, row.projects...)
				if row.restricted != nil {
					token = rig.memberToken(row.sessionID, *row.restricted, row.projects...)
				}
				requireStatus(t, rig.subscribe(t, token, row.sessionID), http.StatusOK, row.name+"'s registration")
				answer := rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, row.sessionID, row.role))
				claimed, err := rig.deps.registry.RoleClaim(t.Context(), store.RoleClaimKeyOf(row.role))
				if err != nil {
					t.Fatalf("%s: read the claim of %s: %v", row.name, row.role, err)
				}
				if required && row.refused {
					if answer.Code != http.StatusForbidden || !strings.Contains(answer.Body.String(), projectNotListed) || claimed.HolderSessionID != "" {
						t.Errorf("%s: answered %d %s with holder %q, want 403 %s and no claim", row.name, answer.Code, answer.Body.String(), claimed.HolderSessionID, projectNotListed)
					}
					continue
				}
				if answer.Code != http.StatusOK || claimed.HolderSessionID != row.sessionID {
					t.Errorf("%s (required %v): answered %d %s with holder %q, want the claim taken", row.name, required, answer.Code, answer.Body.String(), claimed.HolderSessionID)
				}
			}
		})
	}
}
