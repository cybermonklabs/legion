package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A session that registers an issue's topic in the old form, the key alone, is answered with the
// topic stored as it wrote it and a warning naming the new form, its project first, and its
// consumer delivers that issue's events whichever form they are published in: the rename's
// readers take both forms before any producer moves. Its own agent subject, which the plugin adds
// to every registration itself, is named in no warning, and is delivered in either form too.
func TestARegistrationInTheOldFormIsToldTheNewFormAndReceivesEither(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_old_form"
	rig := newSessionConsumerRig(t)
	legacy := "notifications.dispatch.issue.INFRA-12.>"
	current := contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">")
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"topics":          []string{legacy, contracts.LegacyAgentSubject(sessionID)},
		"delivery":        map[string]any{"consumer": true, "epoch": "e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var answer interestSubscribeAnswer
	if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", string(body))), &answer); err != nil {
		t.Fatalf("decode the registration's answer: %v", err)
	}
	if want := []string{legacy + " is the old form of " + current + "; register " + current}; !slices.Equal(answer.Warnings, want) {
		t.Fatalf("warnings = %q, want %q", answer.Warnings, want)
	}
	if !slices.Contains(answer.Topics, legacy) {
		t.Fatalf("answered topics = %q, want %s stored as the caller wrote it", answer.Topics, legacy)
	}
	want := []string{
		"notifications.agent.*." + sessionID,
		contracts.LegacyAgentSubject(sessionID),
		legacy,
		current,
	}
	slices.Sort(want)
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters = %q, want %q", got, want)
	}

	rig.store(t, "notifications.dispatch.issue.INFRA-12.comment.created")
	rig.store(t, contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created"))
	rig.store(t, contracts.AgentSubject("INFRA", sessionID))
	rig.store(t, contracts.DispatchIssueSubject("INFRA", "INFRA-13", "comment.created"))
	rig.awaitPending(t, sessionID, 3)
}

// A registration naming only the new form, or topics the rename leaves alone, is named in no
// warning: the warning is for a caller still on the old form.
func TestARegistrationInTheNewFormIsNamedInNoWarning(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_new_form"
	rig := newSessionConsumerRig(t)
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"topics":          []string{contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">"), "notifications.team.deploy-squad"},
		"delivery":        map[string]any{"consumer": true, "epoch": "e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var answer interestSubscribeAnswer
	if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", string(body))), &answer); err != nil {
		t.Fatalf("decode the registration's answer: %v", err)
	}
	if len(answer.Warnings) != 0 {
		t.Fatalf("warnings = %q, want none", answer.Warnings)
	}
}

// A role claimed in either form claims the one role its name is: the old form, the name alone, is
// answered with a warning naming the new form, project first, and the new form claims the same
// role with no warning. The claim record is keyed by the name either way, so GET /v1/roles/<name>
// reads the holder the new form's claim gave it.
func TestARoleClaimedInEitherFormClaimsItsName(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_role_forms"
	const role = "legion-infra-infra-12-architect"
	rig := newSessionConsumerRig(t)
	rig.register(t, sessionID, nil)

	var old struct {
		Topics   []string `json:"topics"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(rig.post(t, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"`+role+`"}`)), &old); err != nil {
		t.Fatalf("decode the old form's claim: %v", err)
	}
	if want := []string{role + " is the old form of INFRA." + role + "; claim INFRA." + role}; !slices.Equal(old.Warnings, want) {
		t.Fatalf("warnings = %q, want %q", old.Warnings, want)
	}
	if !slices.Contains(old.Topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("the claimant's topics = %q, want its role's lane as before", old.Topics)
	}

	const other = "ses_role_new_form"
	rig.register(t, other, nil)
	var current struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(rig.post(t, "/v1/roles/set", `{"session_id":"`+other+`","role":"INFRA.`+role+`"}`)), &current); err != nil {
		t.Fatalf("decode the new form's claim: %v", err)
	}
	if len(current.Warnings) != 0 {
		t.Fatalf("the new form's claim was warned %q", current.Warnings)
	}
	holder, err := rig.harness.registry.RoleHolder(role)
	if err != nil || holder != other {
		t.Fatalf("holder of %s = %q, %v; want %s, whose claim named the new form", role, holder, err, other)
	}
}

// A role named in the new form under a project it cannot work for is refused with 400, naming
// both projects (contracts.ClaimProject): a Legion role under any project but the one its own name
// gives, _ among them, since its claim is that project's and a caller under another would believe
// it holds a claim it does not. A free-form role (lead-reviewer) under a project is no such claim:
// it is that project's own, keyed <project>.<role> (store.RoleClaimKeyOf), beside the claim of the
// same name under no project, which keeps its bare key and its holder. The Legion role under its
// own project, and each bare name, are claimed as before.
func TestARoleClaimUnderAProjectItDoesNotWorkForIsRefused(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_role_project"
	rig := newSessionConsumerRig(t)
	rig.register(t, sessionID, nil)
	claim := func(role string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
			strings.NewReader(`{"session_id":"`+sessionID+`","role":"`+role+`"}`)))
		return recorder
	}
	for _, refused := range []struct{ role, name, own, asked string }{
		{"PRIV.legion-zz-controller", "legion-zz-controller", "ZZ", "PRIV"},
		{"_.legion-zz-controller", "legion-zz-controller", "ZZ", "_"},
	} {
		answer := claim(refused.role)
		body := answer.Body.String()
		if answer.Code != http.StatusBadRequest || !strings.Contains(body, "project "+refused.own) || !strings.Contains(body, "project "+refused.asked) {
			t.Fatalf("claim %s answered %d %s, want 400 naming its own project %s and the asked-for %s", refused.role, answer.Code, body, refused.own, refused.asked)
		}
		if holder, err := rig.deps.registry.RoleHolder(refused.name); err == nil && holder != "" {
			t.Fatalf("after the refused claim %s, %s is held by %q, want nobody", refused.role, refused.name, holder)
		}
	}
	for role, key := range map[string]string{
		"ZZ.legion-zz-controller": "legion-zz-controller",
		"legion-zz-controller":    "legion-zz-controller",
		"_.lead-reviewer":         "lead-reviewer",
		"lead-reviewer":           "lead-reviewer",
		"PRIV.lead-reviewer":      "PRIV.lead-reviewer",
		"INFRA.reviewer":          "INFRA.reviewer",
	} {
		if answer := claim(role); answer.Code != http.StatusOK {
			t.Fatalf("claim %s answered %d %s, want 200", role, answer.Code, answer.Body.String())
		}
		if holder, err := rig.deps.registry.RoleHolder(key); err != nil || holder != sessionID {
			t.Fatalf("after the claim %s, the claim keyed %s is held by %q (%v), want %s", role, key, holder, err, sessionID)
		}
	}
	if holder, err := rig.deps.registry.RoleHolder("reviewer"); err != nil || holder != "" {
		t.Fatalf("after the claim INFRA.reviewer, the claim of reviewer under no project is held by %q (%v), want nobody", holder, err)
	}
}

// An unsubscribe of either form of a topic removes every form the record holds (store.Remove), so
// its answer names each topic it took out, the other form the caller did not name among them: a
// plugin that followed both forms and dropped one reads from `removed` that the other is gone too,
// rather than finding it missing at its next registration and adding it back.
func TestAnUnsubscribeNamesEveryFormItRemoved(t *testing.T) {
	t.Parallel()
	rig := newInterestRig(t)
	const old, current = "notifications.dispatch.issue.KEY-1.>", "notifications.dispatch.issue.KEY.KEY-1.>"
	subscribeInterest(t, rig.mux, []string{old})
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/unsubscribe",
		strings.NewReader(`{"session_id":"`+interestSession+`","topics":["`+current+`"]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unsubscribe status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the unsubscribe answer %s: %v", recorder.Body.String(), err)
	}
	if want := []string{current, old}; !slices.Equal(answer.Removed, want) {
		t.Fatalf("removed = %q, want %q: the form named and the form the record held", answer.Removed, want)
	}
	stored, err := rig.deps.registry.Get(context.Background(), interestSession)
	if err == nil && slices.Contains(stored.Topics, old) {
		t.Fatalf("stored topics = %q, want %s gone", stored.Topics, old)
	}
}

// A role's claim is keyed by its bare name whichever form named it, for a role of no project and
// for a Legion role (store.RoleClaimKeyOf), so every reader of a role name must resolve it to that
// key before it looks the claim up: a send to the role's lane in the new form, and GET
// /v1/roles/<role>. A reader that takes the name as it stands looks up "_.lead-reviewer", which
// no claim is keyed by, so the publish is answered 202 kept with no holder and the message
// reaches nobody.
func TestARoleIsFoundByEitherFormOfItsName(t *testing.T) {
	t.Parallel()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	state := withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
	handler := publishHandler(state, logging.New("test"))
	uri := testnats.URL(t)

	for _, tc := range []struct{ role, project, holder string }{
		{"lead-reviewer", contracts.NoProject, "ses_lead_holder"},
		{"legion-infra-infra-12-architect", "INFRA", "ses_architect_holder"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			if err := sessions.Put(tc.holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
				t.Fatalf("register the holder: %v", err)
			}
			if _, err := registry.SetRole(tc.holder, "test-machine", tc.role, false); err != nil {
				t.Fatalf("claim %s: %v", tc.role, err)
			}
			startStandIn(t, uri, tc.holder)

			for _, topic := range []string{
				contracts.RoleTopicPrefix + tc.role,
				contracts.RoleSubject(tc.project, tc.role),
			} {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish",
					strings.NewReader(`{"topic":"`+topic+`","message":"please review","source":"agent"}`)))
				var answer struct {
					Holder     string `json:"holder"`
					Kept       bool   `json:"kept"`
					KeptReason string `json:"kept_reason"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
					t.Fatalf("decode the publish to %s: %v", topic, err)
				}
				if recorder.Code != http.StatusOK || answer.Holder != tc.holder || answer.Kept {
					t.Fatalf("a publish to %s answered %d %+v, want 200 naming %s and not kept", topic, recorder.Code, answer, tc.holder)
				}
			}

			for _, name := range []string{tc.role, tc.project + "." + tc.role} {
				recorder := httptest.NewRecorder()
				roleGetHandler(state, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+name, nil))
				var answer struct {
					Holder string `json:"holder"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
					t.Fatalf("decode GET /v1/roles/%s (%d): %v", name, recorder.Code, err)
				}
				if recorder.Code != http.StatusOK || answer.Holder != tc.holder {
					t.Fatalf("GET /v1/roles/%s answered %d naming %q, want 200 naming %s", name, recorder.Code, answer.Holder, tc.holder)
				}
			}
		})
	}
}

// A role named in its old form, the name alone, wherever the listener reads it to send to its
// holder or to look the holder up - a publish to the role's lane, a message the role lane takes on
// NATS, and GET /v1/roles/<role> - is counted (kind="role_read"), as a claim in the old form is
// counted (kind="role"): a publisher that only sends to a role is still on the old form, and its
// sends would stop resolving once the old form is refused, so the count is what
// legacy-subjects-gate.sh reads to see it. The publish is answered with the warning the claim and
// registration routes give an old form, naming the new one. The same role in its new form is
// counted and warned nowhere.
func TestARoleNamedInTheOldFormIsCountedWhereverItIsRead(t *testing.T) {
	t.Parallel()
	const role = "lead-reviewer"
	const holder = "ses_role_read_holder"
	harness := newListenerDeliveryHarness(t)
	counter := &legacyKinds{}
	counting, err := store.Open(t.Context(), harness.client.Conn, store.WithReplicas(1), store.WithLegacySubjectCounter(counter))
	if err != nil {
		t.Fatalf("open a counting registry: %v", err)
	}
	waitForRegistry(t, counting)
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the holder: %v", err)
	}
	if _, err := counting.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s: %v", role, err)
	}
	startStandIn(t, testnats.URL(t), holder)

	state := withPublishRoute(t, &listenerDeps{client: harness.client, registry: counting, sessions: harness.sessions})
	publish := publishHandler(state, logging.New("test"))
	laneConfig := harness.config
	laneConfig.registry = counting
	laneConfig = wireTestRoleForwarder(t, laneConfig)
	lane := coreNATSDeliveryHandler(laneConfig)

	// readEach reads the role under topic through each of the three readers, and answers the
	// publish's warnings.
	readEach := func(topic, path string) []string {
		t.Helper()
		recorder := httptest.NewRecorder()
		publish.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish",
			strings.NewReader(`{"topic":"`+topic+`","message":"please review","source":"agent"}`)))
		var answer struct {
			Holder   string    `json:"holder"`
			Warnings *[]string `json:"warnings"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the publish to %s: %v", topic, err)
		}
		if recorder.Code != http.StatusOK || answer.Holder != holder {
			t.Fatalf("a publish to %s answered %d %s, want 200 naming %s", topic, recorder.Code, recorder.Body.String(), holder)
		}

		recorder = httptest.NewRecorder()
		roleGetHandler(state, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/roles/%s answered %d %s, want 200", path, recorder.Code, recorder.Body.String())
		}

		lane(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(topic, "role-read-"+path))})
		laneConfig.roleForwards.settled(t)
		if answer.Warnings == nil {
			return nil
		}
		return *answer.Warnings
	}

	if warnings := readEach(contracts.RoleSubject(contracts.NoProject, role), contracts.NoProject+"."+role); warnings != nil {
		t.Fatalf("the new form's publish carries warnings %q, want the key left out", warnings)
	}
	if got := counter.counted(); len(got) != 0 {
		t.Fatalf("the new form was counted %q, want nothing", got)
	}

	current := contracts.NoProject + "." + role
	if warnings, want := readEach(contracts.RoleTopicPrefix+role, role), []string{role + " is the old form of " + current + "; name " + current}; !slices.Equal(warnings, want) {
		t.Fatalf("the old form's publish warns %q, want %q", warnings, want)
	}
	if got, want := counter.counted(), []string{"role_read", "role_read", "role_read"}; !slices.Equal(got, want) {
		t.Fatalf("the old form, read by the publish route, GET /v1/roles/<role> and the role lane, was counted %q, want %q", got, want)
	}
}

// A name that is no role (roles.Valid: lower-case letters, digits, `_` and `-`), read where the
// listener reads a role - a publish to its lane, the role lane and GET /v1/roles/<name> - is not
// counted as an old form, and the publish names no new form of it: a mistyped name is no reader
// the old form's refusal would stop, and the claim route refuses such a name outright.
func TestANameThatIsNoRoleIsNotCountedAsAnOldForm(t *testing.T) {
	t.Parallel()
	const name = "Lead-Reviewer"
	harness := newListenerDeliveryHarness(t)
	counter := &legacyKinds{}
	counting, err := store.Open(t.Context(), harness.client.Conn, store.WithReplicas(1), store.WithLegacySubjectCounter(counter))
	if err != nil {
		t.Fatalf("open a counting registry: %v", err)
	}
	waitForRegistry(t, counting)
	state := withPublishRoute(t, &listenerDeps{client: harness.client, registry: counting, sessions: harness.sessions})

	recorder := httptest.NewRecorder()
	publishHandler(state, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish",
		strings.NewReader(`{"topic":"`+contracts.RoleTopicPrefix+name+`","message":"please review","source":"agent","keep":false}`)))
	var answer struct {
		Warnings *[]string `json:"warnings"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the publish to %s: %v (%d %s)", name, err, recorder.Code, recorder.Body.String())
	}
	if answer.Warnings != nil {
		t.Fatalf("the publish to %s warns %q, want no old-form warning for a name that is no role", name, *answer.Warnings)
	}
	roleGetHandler(state, logging.New("test")).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/roles/"+name, nil))
	laneConfig := harness.config
	laneConfig.registry = counting
	laneConfig = wireTestRoleForwarder(t, laneConfig)
	coreNATSDeliveryHandler(laneConfig)(&natsgo.Msg{Data: marshalListenerEnvelope(t, listenerTestEnvelope(contracts.RoleTopicPrefix+name, "role-read-no-role"))})
	laneConfig.roleForwards.settled(t)
	if got := counter.counted(); len(got) != 0 {
		t.Fatalf("%s, which is no role, read by the publish route, GET /v1/roles/<name> and the role lane, was counted %q, want nothing", name, got)
	}
}

// legacyKinds is a store.LegacySubjectCounter that records the kind label of each count.
type legacyKinds struct {
	mu    sync.Mutex
	kinds []string
}

func (c *legacyKinds) Inc(labels ...[2]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, label := range labels {
		if label[0] == "kind" {
			c.kinds = append(c.kinds, label[1])
		}
	}
}

func (c *legacyKinds) counted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.kinds)
}

// A session's stored row read into its consumer's filter counts each topic the filter carries in
// the old form (kind="interest"), on every such read: a write of the stored row's filter and a
// repair of the session's consumer each count the issue topic the session registered in the old
// form. The session's own agent subject, which the subscribe route writes into the row itself, and
// its role's lane, which the stream does not carry and so no filter holds, are not counted.
func TestAStoredOldFormTopicIsCountedEachTimeTheRowIsReadIntoTheFilter(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_stored_old_form"
	rig := newSessionConsumerRig(t)
	rig.register(t, sessionID, map[string]any{"topics": []string{"notifications.dispatch.issue.INFRA-12.>"}})
	rig.post(t, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"lead-reviewer"}`)
	stored, err := rig.deps.registry.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	for _, topic := range []string{contracts.LegacyAgentSubject(sessionID), contracts.RoleTopicPrefix + "lead-reviewer"} {
		if !slices.Contains(stored.Topics, topic) {
			t.Fatalf("stored topics = %q, want %s among them, which this test shows is not counted", stored.Topics, topic)
		}
	}

	counter := &legacyKinds{}
	counting, err := store.Open(t.Context(), rig.harness.client.Conn, store.WithReplicas(1), store.WithLegacySubjectCounter(counter))
	if err != nil {
		t.Fatalf("open a counting registry: %v", err)
	}
	rig.deps.consumers.storedFilterWrite(context.Background(), counting, sessionID, "the test's write")
	if got := counter.counted(); !slices.Equal(got, []string{"interest"}) {
		t.Fatalf("a write of the stored row's filter counted %q, want one interest", got)
	}
	deps := *rig.deps
	deps.registry = counting
	if _, finding := rig.deps.consumers.registeredTarget(context.Background(), &deps, sessionID, 0); finding != registeredTargetBuilt {
		t.Fatalf("the repair's target = %v, want it built", finding)
	}
	if got := counter.counted(); !slices.Equal(got, []string{"interest", "interest"}) {
		t.Fatalf("after a repair read the row too, counted %q, want two interests", got)
	}
}
