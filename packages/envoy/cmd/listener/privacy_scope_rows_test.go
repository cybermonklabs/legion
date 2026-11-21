package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestATokenListingPUBSeesNothingOfPRIV: in the required mode with PRIV private, a session whose
// token lists only PUB is refused 403 project_not_listed an interest in PRIV's topic, a send of
// project PRIV to a PRIV session, and a read of PRIV's role. Controls: the same three requests by
// the PRIV session itself are taken, and in the all-public mode the PUB session's are taken too,
// so each refusal fails for the project it names.
func TestATokenListingPUBSeesNothingOfPRIV(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(fmt.Sprintf("required %v", required), func(t *testing.T) {
			rig := newPrivacyRig(t, required)
			rig.source.answer(privacyListingPRIV)
			pubToken := rig.personToken("ses_lists_pub", "PUB")
			requireStatus(t, rig.subscribe(t, pubToken, "ses_lists_pub"), http.StatusOK, "the PUB session's registration")
			privToken := rig.personToken("ses_lists_priv", "PRIV")
			privStandIn := startSessionStandIn(t, rig.url, "ses_lists_priv", answerDone)
			requireStatus(t, rig.subscribe(t, privToken, "ses_lists_priv"), http.StatusOK, "the PRIV session's registration")
			requireStatus(t, rig.call(t, http.MethodPost, privToken, "/v1/roles/set", `{"session_id":"ses_lists_priv","role":"PRIV.reviewer","soft":true}`), http.StatusOK, "the PRIV session's claim of PRIV.reviewer")

			requests := []struct {
				name, method, path string
				body               func(session string) string
			}{
				{"an interest in PRIV's topic", http.MethodPost, "/v1/interests/subscribe", func(session string) string {
					return consumerRegistration(session, "epoch-1", "notifications.dispatch.issue.PRIV.>")
				}},
				{"a send of project PRIV", http.MethodPost, "/v1/messages/send", func(string) string {
					return `{"target_session":"ses_lists_priv","message":"hello","project":"PRIV"}`
				}},
				{"a read of PRIV's role", http.MethodGet, "/v1/roles/PRIV.reviewer", func(string) string { return "" }},
			}
			for _, request := range requests {
				answer := rig.call(t, request.method, pubToken, request.path, request.body("ses_lists_pub"))
				refused := answer.Code == http.StatusForbidden && strings.Contains(answer.Body.String(), projectNotListed)
				if required && !refused {
					t.Errorf("required: the PUB session's %s answered %d %s, want 403 %s", request.name, answer.Code, answer.Body.String(), projectNotListed)
				}
				if !required && refused {
					t.Errorf("control, all-public: the PUB session's %s was refused %d %s", request.name, answer.Code, answer.Body.String())
				}
				if !required {
					continue
				}
				own := rig.call(t, request.method, privToken, request.path, request.body("ses_lists_priv"))
				if own.Code >= 300 {
					t.Errorf("control: the PRIV session's own %s answered %d %s, want it taken", request.name, own.Code, own.Body.String())
				}
			}
			// One send reaches the PRIV session in each mode - its own in the required mode, the
			// PUB session's in the all-public mode - and the row waits for it, so no reply is in
			// flight when the stand-in closes.
			if frames := privStandIn.collect(t, 1, 30*time.Second); len(frames) != 1 {
				t.Fatalf("the PRIV session received %d sends, want 1", len(frames))
			}
		})
	}
}

// rowsPrivacy is Dispatch's committed projects rows as the test switches them: the privacy reader
// roles.Buckets is built with in Dispatch, read afresh at each call.
type rowsPrivacy struct {
	mu      sync.Mutex
	private map[string]bool
}

func (r *rowsPrivacy) read(_ context.Context, project string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.private[project], nil
}

func (r *rowsPrivacy) set(project string, private bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.private[project] = private
}

// cachedOnce is the control's decider: its privacy is the first answer it read, kept.
type cachedOnce struct {
	roles.ClaimStore
	mu      sync.Mutex
	answers map[string]bool
}

func (c *cachedOnce) Private(ctx context.Context, project string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if private, read := c.answers[project]; read {
		return private, nil
	}
	private, err := c.ClaimStore.Private(ctx, project)
	if err == nil {
		c.answers[project] = private
	}
	return private, err
}

// decider is one reader of a role's holder: a claim store and the session lookup it resolves with.
type decider struct {
	name     string
	claims   roles.ClaimStore
	sessions roles.SessionLookup
}

// disagreement is what the deciders answer differently for project's privacy and for the holder
// of (project, role), or "" when every decider answers alike; want is the privacy all must read.
func disagreement(t *testing.T, deciders []decider, project, role string, want bool) string {
	t.Helper()
	var answers []string
	for _, d := range deciders {
		private, err := d.claims.Private(context.Background(), project)
		resolved, resolveErr := roles.ResolveHolder(context.Background(), d.claims, d.sessions, project, role)
		answers = append(answers, fmt.Sprintf("%s: private %v (%v), holder %q state %v (%v)", d.name, private, err, resolved.Holder, resolved.State, resolveErr))
		if err != nil || resolveErr != nil || private != want {
			return strings.Join(answers, "; ")
		}
	}
	for _, answer := range answers[1:] {
		if strings.SplitN(answer, ": ", 2)[1] != strings.SplitN(answers[0], ": ", 2)[1] {
			return strings.Join(answers, "; ")
		}
	}
	return ""
}

// TestBothDecidersAgreeOnPrivacy: the listener's claim store (store.Registry, privacy from the
// privacy source), its role lane's (roleLaneClaims) and Dispatch's roles.Buckets (privacy from
// Dispatch's committed rows) answer alike as Dispatch switches PRIV public, private and public
// again: each reads PRIV's privacy as the rows hold it, and each resolves reviewer's message of
// PRIV to S, the full member's session holding reviewer under no project, while PRIV is public,
// and to no holder while it is private. Control: a decider that keeps its first privacy answer
// disagrees after the first switch, and the comparison names it.
func TestBothDecidersAgreeOnPrivacy(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(privacyNone)
	const holder = "ses_both_deciders"
	token := rig.memberToken(holder, false)
	requireStatus(t, rig.subscribe(t, token, holder), http.StatusOK, "S's registration")
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", `{"session_id":"`+holder+`","role":"reviewer","soft":true}`), http.StatusOK, "S's claim of reviewer")

	rows := &rowsPrivacy{private: map[string]bool{"PRIV": false}}
	buckets := roles.NewBuckets(rig.harness.client, roles.Windows{}, rows.read, roles.Required)
	listener := rig.deps.claims()
	sessions := roleHolderSessions{rig.deps.sessions}
	deciders := []decider{
		{"the listener's claim store", listener, sessions},
		{"the role lane's", roleLaneClaims{claims: listener}, sessions},
		{"Dispatch's roles.Buckets", buckets, buckets},
	}
	stale := append(append([]decider{}, deciders...), decider{"a decider keeping its first answer", &cachedOnce{ClaimStore: buckets, answers: map[string]bool{}}, buckets})

	switchTo := func(private bool) {
		rows.set("PRIV", private)
		if private {
			rig.source.answer(privacyListingPRIV)
		} else {
			rig.source.answer(privacyNone)
		}
	}
	for step, private := range []bool{false, true, false} {
		switchTo(private)
		if differ := disagreement(t, deciders, "PRIV", "reviewer", private); differ != "" {
			t.Fatalf("step %d, PRIV private %v: the deciders disagree: %s", step, private, differ)
		}
		resolved, err := roles.ResolveHolder(context.Background(), buckets, buckets, "PRIV", "reviewer")
		if err != nil || (private && resolved.State != roles.Unclaimed) || (!private && resolved.Holder != holder) {
			t.Fatalf("step %d, PRIV private %v: reviewer's message of PRIV resolved to %+v, %v", step, private, resolved, err)
		}
		// The stale decider read PRIV public at step 0 and keeps it, so at the first switch, to
		// private, it must disagree.
		if differ := disagreement(t, stale, "PRIV", "reviewer", private); step == 1 && differ == "" {
			t.Fatalf("control, step %d: a decider keeping its first answer agreed with the others; the comparison cannot see a stale read", step)
		}
	}
}

// putAsServerReply has the server store at sessionID's key of envoy_sessions the answer it sends
// to a request on subject with body: a JetStream API answer, the no-responders status of a subject
// nobody serves (an empty record), or another key's record read by DIRECT.GET.
func putAsServerReply(t *testing.T, url, sessionID, subject string, body []byte) {
	t.Helper()
	conn := testnats.Connect(t, url)
	if err := conn.PublishRequest(subject, "$KV."+bus.SessionBucket+"."+sessionID, body); err != nil {
		t.Fatalf("ask %s with the reply at %s's key: %v", subject, sessionID, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// TestARequiredRelayReadsNoScopeFromAForgedRecord: E is a restricted member's session whose token
// lists PRIV, F a full member's whose token lists PRIV2, and PRIV and PRIV2 are private. Four
// records are stored at E's key in turn: three by the server, as the answer to a request whose
// reply names E's key (a JetStream API answer, the empty no-responders status, and a copy of F's
// record read by DIRECT.GET), and one in an earlier build's shape (E's own record with no
// session_id, no projects and no restricted). In none of the four does a PUB frame or a PRIV2
// frame reach E, and after each, E's next heartbeat restores its PRIV frames. Controls (mutants,
// each run apart): the plain decode, under which the copy gives E F's scope, PUB's and PRIV2's
// frames; and the plain decode with an absent restricted read as false, under which the earlier
// build's record gives E every public project. A JetStream answer and an empty record stop E's
// relay under any decode (the relay's own ownership check, sessionRelays.wanted, refuses a record
// with no consumer delivery or another machine), so the earlier build's record is the form that
// holds the fail-closed restricted to account; it is stored before the copy, so each control fails
// on its own form.
func TestARequiredRelayReadsNoScopeFromAForgedRecord(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(`{"projects":[{"project":"PRIV","slack_channels":[],"slack_channels_changed_at":null},{"project":"PRIV2","slack_channels":[],"slack_channels_changed_at":null}]}`)
	const e, f = "ses_forged_e", "ses_forged_f"
	eToken := rig.memberToken(e, true, "PRIV")
	eStandIn := rig.session(t, e, true, []string{"PRIV"}, everyIssueTopic)
	rig.session(t, f, false, []string{"PRIV2"}, everyIssueTopic)
	priv2Subject := "notifications.dispatch.issue.PRIV2.PRIV2-1.comment.created"

	first, _ := rig.storeEvent(t, privIssueSubject)
	expectOnly(t, eStandIn, "E before any forged record", first)

	forms := []struct {
		name  string
		forge func(t *testing.T)
	}{
		{"a JetStream answer", func(t *testing.T) {
			putAsServerReply(t, rig.url, e, "$JS.API.STREAM.INFO.KV_"+bus.SessionBucket, nil)
		}},
		{"an empty record", func(t *testing.T) {
			putAsServerReply(t, rig.url, e, "no.service.answers.here", nil)
		}},
		{"an earlier build's record", func(t *testing.T) {
			kv, err := rig.harness.client.JS().KeyValue(bus.SessionBucket)
			if err != nil {
				t.Fatalf("open the session bucket: %v", err)
			}
			old := fmt.Sprintf(`{"port":0,"machine_id":"test-machine","dir":"","title":"","capabilities":null,"updated_at":%d,"delivery":"consumer","epoch":"epoch-1"}`, time.Now().UnixMilli())
			if _, err := kv.Put(e, []byte(old)); err != nil {
				t.Fatalf("store an earlier build's record at E's key: %v", err)
			}
		}},
		{"a copy of F's record", func(t *testing.T) {
			putAsServerReply(t, rig.url, e, "$JS.API.DIRECT.GET.KV_"+bus.SessionBucket, []byte(`{"last_by_subj":"$KV.`+bus.SessionBucket+`.`+f+`"}`))
		}},
	}
	for _, form := range forms {
		form.forge(t)
		time.Sleep(500 * time.Millisecond)
		rig.storeEvent(t, pubIssueSubject)
		rig.storeEvent(t, priv2Subject)
		expectOnly(t, eStandIn, "E under "+form.name)
		requireStatus(t, rig.subscribe(t, eToken, e, everyIssueTopic), http.StatusOK, "E's heartbeat after "+form.name)
		if entry, err := rig.deps.sessions.Get(e); err != nil || !entry.BoundTo(e) {
			t.Fatalf("after %s E's heartbeat left %+v, %v; want E's own record", form.name, entry, err)
		}
		next, _ := rig.storeEvent(t, privIssueSubject)
		expectOnly(t, eStandIn, "E after its heartbeat, past "+form.name, next)
	}
}

// unbindRecord rewrites sessionID's record without its session_id, as a build that wrote the
// projects before the record named its key did (session.SessionEntry.BoundTo), and waits until
// the listener's registry reads it so.
func unbindRecord(t *testing.T, rig *privacyRig, sessionID string) {
	t.Helper()
	entry, err := rig.deps.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's record: %v", sessionID, err)
	}
	entry.SessionID = ""
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode %s's record without its key: %v", sessionID, err)
	}
	kv, err := rig.harness.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := kv.Put(sessionID, raw); err != nil {
		t.Fatalf("store %s's record without its key: %v", sessionID, err)
	}
	testwait.Eventually(t, sessionID+"'s record read without its key", func() bool {
		read, _ := rig.deps.sessions.Lookup(sessionID)
		return read.SessionID == "" && read.MachineID != ""
	})
}

// TestASendToAnUnboundTargetIsNoProjects: in the required mode the send route reads the target's
// projects only from a record that names its key, for the project KC1 chooses as for whether the
// target sees it. A full member's session that lists no project sends with no project to T, whose
// record lists the public PUB but does not name its key: the message is of no project, accepted and
// stored on T's subject of no project, and T receives it. Beside it, the same send to T2, whose own
// record lists PUB, is of PUB, stored on T2's PUB subject. Control (a mutant, run apart): the
// project chosen from the record's raw list makes the send to T one of PUB, which T's unbound
// record (no project's, restricted) does not see, and the route answers 403 project_not_listed
// naming T.
func TestASendToAnUnboundTargetIsNoProjects(t *testing.T) {
	rig := newPrivacyRig(t, true)
	rig.source.answer(privacyListingPRIV)
	const sender, unbound, bound = "ses_send_sender", "ses_send_unbound", "ses_send_bound"
	senderToken := rig.memberToken(sender, false)
	requireStatus(t, rig.subscribe(t, senderToken, sender), http.StatusOK, "the sender's registration")
	unboundStandIn := rig.session(t, unbound, false, []string{"PUB"})
	boundStandIn := rig.session(t, bound, false, []string{"PUB"})
	unbindRecord(t, rig, unbound)

	for _, target := range []struct {
		id      string
		standIn *sessionStandIn
		subject string
	}{
		{unbound, unboundStandIn, contracts.LegacyAgentSubject(unbound)},
		{bound, boundStandIn, contracts.AgentSubject("PUB", bound)},
	} {
		answer := rig.call(t, http.MethodPost, senderToken, "/v1/messages/send", `{"target_session":"`+target.id+`","message":"hello"}`)
		if answer.Code >= 300 {
			t.Fatalf("the send to %s answered %d %s, want it accepted", target.id, answer.Code, answer.Body.String())
		}
		frames := target.standIn.collect(t, 1, 30*time.Second)
		if len(frames) != 1 || frames[0].item.Topic != target.subject {
			t.Fatalf("the send to %s reached it on %v, want %s", target.id, frames, target.subject)
		}
	}
}
