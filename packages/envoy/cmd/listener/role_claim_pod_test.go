// A session pod's role is claimed by the pod's role claimer, a program beside the agent that
// sends POST /v1/roles/set with the session id and the role and no client_instance_id, for the
// session the pod's own agent has registered. The claim it writes names no process, and the
// agent's next registration, which states the agent's process, adopts it. These cases drive that
// path through the listener's own routes over a real role bucket.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// claimRoleForPod is the claim a session pod's role claimer makes: POST /v1/roles/set with the
// session id and the role and nothing else. It fails t, the test running it, which may be a
// subtest of the rig's own.
func (r *roleClaimHandBackRig) claimRoleForPod(t *testing.T) store.RoleClaim {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"role":%q}`, r.session, r.role)
	if recorder := r.send(http.MethodPost, "/v1/roles/set", body); recorder.Code != http.StatusOK {
		t.Fatalf("POST /v1/roles/set %s: status %d %s, want 200", body, recorder.Code, recorder.Body.String())
	}
	return r.stored()
}

// handBackAs is POST /v1/interests/unsubscribe with body, which must answer 200; it answers the
// topics the route reports removed.
func (r *roleClaimHandBackRig) handBackAs(body string) []string {
	r.t.Helper()
	recorder := r.send(http.MethodPost, "/v1/interests/unsubscribe", body)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("POST /v1/interests/unsubscribe %s: status %d %s, want 200", body, recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		r.t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return answer.Removed
}

// podNoticeTopic is a subject a plugin opened for the role the pod holds, which pi-envoy hands
// back with no client_instance_id when the role ends (its closeSubjects); a body below names the
// role's lane beside it.
const podNoticeTopic = "notifications.team.pod-notice"

// The pod path: POST /v1/roles/set with no client_instance_id, the body the role claimer sends, is
// granted and stores a claim that names no process; the session's next registration that states
// a client instance writes that instance into the claim; a later registration of the same
// session that states another instance changes nothing, and a hand-back from that other instance
// frees nothing. The process that adopted the claim goes on renewing it.
//
// A hand-back with no client_instance_id naming the role's lane - which no current client sends
// for a role lane, but a client of an older build or any caller that leaves the instance out
// does - here naming the lane alone and beside another subject, marks a pod's claim that names no
// process and frees nothing; such a claim is then adopted by no registration.
// Control: the same bodies mark a claim that names a process, as they did before.
func TestAPodClaimWithNoProcessGoesToThePodsFirstRegistration(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_session_pod"
	)
	lane := contracts.RoleTopicPrefix + role

	t.Run("the role claimer's claim goes to the pod's first registration", func(t *testing.T) {
		rig := newRoleClaimHandBackRig(t, sessionID, role)
		// The pod's agent registers before the role claimer claims: the claimer claims only for
		// a session the listener lists, and the route refuses a session it does not hold.
		rig.register(holdingProcess, rig.ownTopics())

		var claimed store.RoleClaim
		if !t.Run("POST /v1/roles/set with no client_instance_id is granted and names no process", func(t *testing.T) {
			claimed = rig.claimRoleForPod(t)
			if claimed.HolderSessionID != sessionID || claimed.ClientInstanceID != "" {
				t.Fatalf("the claim is %+v, want %s's claim naming no process", claimed, sessionID)
			}
		}) {
			t.FailNow()
		}

		var adopted store.RoleClaim
		if !t.Run("the session's next registration stating a process adopts the claim", func(t *testing.T) {
			rig.register(holdingProcess, rig.ownTopics())
			adopted = rig.stored()
			if adopted.HolderSessionID != sessionID || adopted.ClientInstanceID != holdingProcess || adopted.ClaimedAt != claimed.ClaimedAt || adopted.HandedBack {
				t.Fatalf("the claim after the registration is %+v, want %s's claim of %d naming the process %q", adopted, sessionID, claimed.ClaimedAt, holdingProcess)
			}
		}) {
			t.FailNow()
		}

		t.Run("a later registration stating another process changes nothing", func(t *testing.T) {
			rig.register(secondProcess, rig.ownTopics())
			if stood := rig.stored(); !reflect.DeepEqual(stood, adopted) {
				t.Fatalf("the claim after the second process's registration is %+v (revision %d), want it exactly as the adoption left it, %+v (revision %d)", stood, stood.Revision, adopted, adopted.Revision)
			}
		})

		t.Run("a hand-back from that other process frees nothing", func(t *testing.T) {
			rig.handBackAs(`{"session_id":"` + sessionID + `","topics":["` + lane + `"],"client_instance_id":"` + secondProcess + `"}`)
			if stood := rig.stored(); !reflect.DeepEqual(stood, adopted) {
				t.Fatalf("the claim after the second process's hand-back is %+v (revision %d), want it exactly as the adoption left it, %+v (revision %d)", stood, stood.Revision, adopted, adopted.Revision)
			}
			if topics := rig.topics(); !slices.Contains(topics, lane) {
				t.Fatalf("the record after the second process's hand-back is %v, want the role's lane on it: the claim it belongs to stands", topics)
			}
		})

		t.Run("control: the process that adopted the claim renews it", func(t *testing.T) {
			rig.register(holdingProcess, rig.ownTopics())
			if renewed := rig.stored(); renewed.Revision == adopted.Revision || renewed.ClientInstanceID != holdingProcess {
				t.Fatalf("the claim after the adopting process's registration is %+v (revision %d), want it renewed past revision %d", renewed, renewed.Revision, adopted.Revision)
			}
		})
	})

	t.Run("a hand-back with no client_instance_id naming the role's lane", func(t *testing.T) {
		for name, topics := range map[string]string{
			"the role's lane alone":            fmtTopics(lane),
			"the lane beside a notice subject": fmtTopics(podNoticeTopic, lane),
		} {
			body := `{"session_id":"` + sessionID + `","topics":` + topics + `}`
			t.Run(name, func(t *testing.T) {
				t.Run("a pod's claim that names no process is marked and freed by nothing", func(t *testing.T) {
					rig := newRoleClaimHandBackRig(t, sessionID, role)
					rig.register(holdingProcess, fmtOwnTopics(handBackOwnTopic, podNoticeTopic))
					claimed := rig.claimRoleForPod(t)

					if removed := rig.handBackAs(body); slices.Contains(removed, lane) {
						t.Fatalf("the hand-back reports %v removed, want the role's lane kept: its claim stands", removed)
					}

					marked := rig.stored()
					if !marked.HandedBack || marked.HolderSessionID != sessionID || marked.ClientInstanceID != "" || marked.ClaimedAt != claimed.ClaimedAt {
						t.Fatalf("the claim after the hand-back is %+v (revision %d), want %s's claim of %d marked handed back and naming no process", marked, marked.Revision, sessionID, claimed.ClaimedAt)
					}
					rig.register(holdingProcess, rig.ownTopics())
					if stood := rig.stored(); !reflect.DeepEqual(stood, marked) {
						t.Fatalf("the claim after the agent's next registration is %+v, want it exactly as the hand-back left it, %+v: no registration adopts or renews a marked claim", stood, marked)
					}

					// The way back is the outright claim: the pod's role claimer's next claim,
					// a hard claim with no client_instance_id, reads no holder and lands at
					// once, writing a fresh record that carries no mark. The agent's own soft
					// claim is weighed by the marked claim's renewal and refused meanwhile.
					t.Run("the pod's outright claim takes the marked claim back at once", func(t *testing.T) {
						refused := rig.send(http.MethodPost, "/v1/roles/set",
							`{"session_id":"`+sessionID+`","role":"`+role+`","soft":true,"client_instance_id":"`+holdingProcess+`"}`)
						if refused.Code != http.StatusConflict {
							t.Fatalf("the agent's soft claim over the marked claim: status %d %s, want 409 while the marked claim's renewal is inside the live window", refused.Code, refused.Body.String())
						}
						again := rig.claimRoleForPod(t)
						if again.HandedBack || again.HolderSessionID != sessionID || again.ClientInstanceID != "" || again.Revision == marked.Revision {
							t.Fatalf("the claim after the outright claim is %+v (revision %d), want a fresh unmarked claim of %s past revision %d", again, again.Revision, sessionID, marked.Revision)
						}
					})
				})
				t.Run("control: a claim that names a process is marked as before", func(t *testing.T) {
					rig := newRoleClaimHandBackRig(t, sessionID, role)
					rig.register(holdingProcess, fmtOwnTopics(handBackOwnTopic, podNoticeTopic))
					rig.claim(holdingProcess)
					claimed := rig.stored()

					rig.handBackAs(body)

					marked := rig.stored()
					if !marked.HandedBack || marked.HolderSessionID != sessionID || marked.ClientInstanceID != holdingProcess || marked.ClaimedAt != claimed.ClaimedAt {
						t.Fatalf("the claim after the hand-back is %+v, want %s's claim of process %q marked handed back", marked, sessionID, holdingProcess)
					}
				})
			})
		}
	})
}

// fmtTopics is topics as a JSON array.
func fmtTopics(topics ...string) string {
	encoded, err := json.Marshal(topics)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// fmtOwnTopics is the "topics" member a registration carries, naming topics.
func fmtOwnTopics(topics ...string) string {
	return `"topics":` + fmtTopics(topics...) + `,`
}
