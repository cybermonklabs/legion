package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/webhook"
)

// outageReading is what each of the five groups the privacy read decides answered: the relay
// (a PUB frame and the session's own frame), a route (a claim of a PUB role), the Slack intake,
// the role fallback, and the readiness route.
type outageReading struct {
	pubRelayed, ownRelayed bool
	claim, slack, ready    int
	fallbackHeld           bool
}

// readOutageGroups drives the five groups once. The session holder follows every issue event and
// holds reviewer under no project, so the fallback from (PUB, reviewer) turns on PUB's privacy;
// claimRole is the PUB role the route row claims, one per reading so no claim answers another.
func readOutageGroups(t *testing.T, rig *privacyRig, holder, token string, standIn *sessionStandIn, claimRole string) outageReading {
	t.Helper()
	var reading outageReading
	pub, _ := rig.storeEvent(t, pubIssueSubject)
	own, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(holder))
	deadline := time.After(10 * time.Second)
	for !reading.ownRelayed {
		select {
		case frame := <-standIn.frames:
			reading.pubRelayed = reading.pubRelayed || frame.item.EventID == pub.EventID
			reading.ownRelayed = reading.ownRelayed || frame.item.EventID == own.EventID
		case <-deadline:
			t.Fatalf("the session's own frame did not arrive within 10 s (PUB relayed %v)", reading.pubRelayed)
		}
	}
	// The PUB frame, stored before the own frame, is decided first: relayed by now, or held.
	reading.claim = rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, holder, claimRole)).Code
	reading.slack = slackEvent(t, webhook.SlackHandler(slackTestSecret, &slackPublished{}, webhook.WithSlackPrivacy(rig.privacy)), "C0PUB00001")
	_, err := roles.ResolveHolder(context.Background(), rig.deps.claims(), roleHolderSessions{rig.deps.sessions}, "PUB", "reviewer")
	readErr, isRead := errors.AsType[*roles.ReadError](err)
	reading.fallbackHeld = isRead && readErr.Step == roles.PrivacyRead
	request := httptest.NewRequest(http.MethodGet, "/v1/privacy-readiness", nil)
	request = request.WithContext(withVerifiedPrincipal(request.Context(), outageDispatchAccount))
	recorder := httptest.NewRecorder()
	privacyReadinessHandler(rig.privacy).ServeHTTP(recorder, request)
	reading.ready = recorder.Code
	return reading
}

const outageDispatchAccount = "system:serviceaccount:dispatch:dispatch"

// TestAPrivacyOutageFailsClosedEvenWhileEveryProjectIsPublic: the source answered that no project
// is private, then fails in each of three forms - 502 dispatch_unavailable, a read past its
// timeout, and a refused connection. In each, all five groups fail closed although every project
// was public at the last answer, since the reader keeps no last answer: the relay holds the PUB
// frame of a full member's session listing no project while its own frame (no project) arrives at
// once; the claim of a PUB role answers 503; the Slack intake answers 503; the fallback from
// (PUB, reviewer) to the claim under no project is held at the privacy read; and the readiness
// route answers 503. Controls: the same five groups with the source answering - before each
// outage, on a fresh rig - relay the PUB frame, write the claim, take the Slack event, resolve
// the fallback's holder and answer ready, so each group's row fails for the outage it names. Once
// the source answers again, the held PUB frame arrives.
func TestAPrivacyOutageFailsClosedEvenWhileEveryProjectIsPublic(t *testing.T) {
	limits := holdLimits
	holdLimits.redecide = 300 * time.Millisecond
	t.Cleanup(func() { holdLimits = limits })
	for _, outage := range []struct {
		name  string
		begin func(rig *privacyRig) (end func())
	}{
		{"502 dispatch_unavailable", func(rig *privacyRig) func() {
			rig.source.failWith(http.StatusBadGateway)
			return func() { rig.source.answer(privacyNone) }
		}},
		{"a read past its timeout", func(rig *privacyRig) func() { return rig.source.hold() }},
		{"a refused connection", func(rig *privacyRig) func() {
			closed := httptest.NewServer(http.NotFoundHandler())
			closed.Close()
			url := rig.privacy.k7.url
			rig.privacy.k7.url = closed.URL + k7Path
			return func() { rig.privacy.k7.url = url }
		}},
	} {
		t.Run(outage.name, func(t *testing.T) {
			rig := newPrivacyRig(t, true)
			rig.privacy.k7.timeout = 500 * time.Millisecond
			rig.privacy.allProjects = []string{outageDispatchAccount}
			holder := "ses_outage_holder"
			standIn := rig.session(t, holder, false, nil, everyIssueTopic)
			token := rig.memberToken(holder, false)
			requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":"reviewer","soft":true}`, holder)), http.StatusOK, "the bare reviewer claim")

			answering := readOutageGroups(t, rig, holder, token, standIn, "PUB.answering")
			if want := (outageReading{pubRelayed: true, ownRelayed: true, claim: http.StatusOK, slack: http.StatusOK, ready: http.StatusOK}); answering != want {
				t.Fatalf("control, the source answering: the groups read %+v, want %+v", answering, want)
			}

			end := outage.begin(rig)
			failing := readOutageGroups(t, rig, holder, token, standIn, "PUB.failing")
			if want := (outageReading{ownRelayed: true, claim: http.StatusServiceUnavailable, slack: http.StatusServiceUnavailable, ready: http.StatusServiceUnavailable, fallbackHeld: true}); failing != want {
				t.Fatalf("%s: the groups read %+v, want %+v", outage.name, failing, want)
			}
			end()
			select {
			case frame := <-standIn.frames:
				if project, _ := contracts.SubjectProject(frame.item.Topic); project != "PUB" {
					t.Fatalf("after the outage the first frame is of %q, want the held PUB frame", project)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the held PUB frame did not arrive within 15 s of the source answering again")
			}
		})
	}
}
