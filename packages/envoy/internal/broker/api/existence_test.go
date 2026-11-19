// existence_test.go: a caller the broker has not authenticated learns nothing from the two decide
// routes or from the reread route about which credential-request record ids and which secret
// names exist. Each such row sends the calls of one caller naming things that exist and things
// that do not, and reads every answer the same way (readAnswer, requireSameAnswers); the rows
// beside them pin that a caller the broker does authenticate still gets the answer it got before,
// and that GET /v1/settings stays open to a caller with no credential, which two of its callers
// are when they read it.
package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/testwait"
)

// answerRead is one answer as requireSameAnswers compares it: its status and its body, the body
// as JSON with answer_id taken out, since that member names the one answer it is in and is new on
// every answer, so it says nothing of what the call named.
type answerRead struct {
	status int
	body   string
}

// readAnswer sends request and reads its answer, failing t unless the answer carries the
// broker's answer id in its header: an answer every line of the broker's own code writes, so a
// read here is never of a body something in front of the broker wrote.
func readAnswer(t *testing.T, request *http.Request) answerRead {
	t.Helper()
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Header.Get(answer.Header)) != 32 {
		t.Fatalf("%s %s = %d %s carries no answer id; want an answer the broker wrote", request.Method, request.URL.Path, resp.StatusCode, raw)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return answerRead{status: resp.StatusCode, body: string(raw)}
	}
	delete(fields, "answer_id")
	normal, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return answerRead{status: resp.StatusCode, body: string(normal)}
}

// requireSameAnswers fails t unless every answer in answers, keyed by what its call named, is the
// same answer: one status, one code and one body.
func requireSameAnswers(t *testing.T, what string, answers map[string]answerRead) {
	t.Helper()
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range answers {
			if !yield(name) {
				return
			}
		}
	})
	first := answers[names[0]]
	for _, name := range names[1:] {
		if got := answers[name]; got != first {
			t.Fatalf("%s: naming %s answered %d %s, and naming %s answered %d %s; want one answer whatever the call names",
				what, names[0], first.status, first.body, name, got.status, got.body)
		}
	}
}

// post builds a POST of path at ts with headers and a JSON body, nil for none.
func post(t *testing.T, ts *testServer, path string, headers map[string]string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	request, err := http.NewRequest(http.MethodPost, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}

// TestTheDecideRoutesAnswerACallerTheyDoNotAuthenticateTheSameForEveryRecordID: approve and deny,
// called with no credential, with a bearer token that does not verify, with the token of an
// account BROKER_API_ACCOUNTS does not list, and with a session's proof, which these routes do not
// take, answer each caller one answer for a pending secret request's id, a pending machine login's
// id and an id no record has, and write nothing. Before the decide routes checked the caller
// first, the secret request's id answered 403 DECIDE_ON_BROKER_PAGE naming its page where the
// other two answered 401, so any caller could tell which ids named a secret request.
func TestTheDecideRoutesAnswerACallerTheyDoNotAuthenticateTheSameForEveryRecordID(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "checking what a stranger learns of this request", "DEEL_API_KEY"), "session_id": nil})
	secretRequest := *decode[wireCreateRequestResponse](t, body).RecordID
	code := ts.openMachineLogin(t, newSigningKey(t), testApprover, "example-host-devbox")
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	machineLogin := decode[wireRecord](t, body).RecordID
	if machineLogin == "" {
		t.Fatalf("lookup of the machine login answered %s; want its record", body)
	}
	ids := map[string]string{
		"a pending secret request": secretRequest,
		"a pending machine login":  machineLogin,
		"an id no record has":      strings.Repeat("0", 64),
	}
	written := func() []string {
		t.Helper()
		return ts.column(t, `select concat_ws(' ',
			(select count(*) from credential_request_events), (select count(*) from grants),
			(select count(*) from audit), (select count(*) from launcher_credentials))`)
	}
	before := written()

	for caller, headers := range map[string]func(path string) map[string]string{
		"no credential": func(string) map[string]string { return nil },
		"a token that does not verify": func(string) map[string]string {
			return map[string]string{"Authorization": "Bearer " + strings.Join([]string{"not", "a", "token"}, ".")}
		},
		"an unlisted account's token": func(string) map[string]string {
			return map[string]string{"Authorization": "Bearer " + ts.accountToken(t, unlistedAccount, "", time.Hour)}
		},
		"a session's proof": func(path string) map[string]string {
			signed, err := proof.Sign(sessionKey, enrollmentID, http.MethodPost, ts.URL+path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			return map[string]string{"Proof": signed}
		},
	} {
		for _, route := range []string{"approve", "deny"} {
			answers := map[string]answerRead{}
			for what, id := range ids {
				path := "/v1/credential-requests/" + id + "/" + route
				answers[what] = readAnswer(t, post(t, ts, path, headers(path), map[string]any{"approver": testApprover, "code": code}))
			}
			requireSameAnswers(t, route+" with "+caller, answers)
			got := answers["an id no record has"]
			if got.status != http.StatusUnauthorized && got.status != http.StatusForbidden {
				t.Fatalf("%s with %s = %d %s; want the refusal of a caller the broker does not authenticate", route, caller, got.status, got.body)
			}
		}
	}
	if after := written(); !slices.Equal(after, before) {
		t.Fatalf("events, grants, audit rows and launcher credentials = %v after the refused calls, want %v as before", after, before)
	}
	if got := ts.record(t, secretRequest); got.State != "pending" || got.Decided != nil {
		t.Fatalf("the secret request after the refused calls = %+v, want still pending", got)
	}
}

// TestTheDecideRoutesStillAnswerAListedCallerAsBefore: the caller BROKER_API_ACCOUNTS lists as
// Dispatch's server still gets every answer it got before the decide routes checked the caller
// first. A secret request is 403 DECIDE_ON_BROKER_PAGE naming its page, an id no record has is 404
// NOT_FOUND, a machine login without its code is 400 CODE_REQUIRED, and with its code it is
// approved.
func TestTheDecideRoutesStillAnswerAListedCallerAsBefore(t *testing.T) {
	ts := newTestServer(t)
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "checking what Dispatch is told of this request", "DEEL_API_KEY"), "session_id": nil})
	secretRequest := *decode[wireCreateRequestResponse](t, body).RecordID
	page := ts.URL + "/approve/" + secretRequest
	for _, route := range []string{"approve", "deny"} {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+secretRequest+"/"+route, map[string]any{"approver": testApprover})
		if refusal := decode[wirePageRefusal](t, body); status != http.StatusForbidden || refusal.Code != "DECIDE_ON_BROKER_PAGE" || refusal.ApproveURL != page {
			t.Fatalf("%s of a secret request as Dispatch = %d %s; want 403 DECIDE_ON_BROKER_PAGE naming %s", route, status, body, page)
		}
		status, body = ts.ui(t, http.MethodPost, "/v1/credential-requests/"+strings.Repeat("0", 64)+"/"+route, map[string]any{"approver": testApprover})
		if status != http.StatusNotFound || decode[wireError](t, body).Code != "NOT_FOUND" {
			t.Fatalf("%s of an id no record has as Dispatch = %d %s; want 404 NOT_FOUND", route, status, body)
		}
	}
	code := ts.openMachineLogin(t, newSigningKey(t), testApprover, "example-host-devbox")
	_, body = ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	login := decode[wireRecord](t, body).RecordID
	if status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+login+"/approve", map[string]any{"approver": testApprover}); status != http.StatusBadRequest || decode[wireError](t, body).Code != "CODE_REQUIRED" {
		t.Fatalf("approve of a machine login without its code as Dispatch = %d %s; want 400 CODE_REQUIRED", status, body)
	}
	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+login+"/approve", map[string]any{"approver": testApprover, "code": code})
	var decided struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}
	if err := json.Unmarshal(body, &decided); err != nil || status != http.StatusOK || decided.State != "approved" || decided.CredentialID == nil {
		t.Fatalf("approve of a machine login with its code as Dispatch = %d %s; want 200 approved with its credential", status, body)
	}
}

// refusedSecret is a secret the broker holds and refuses to serve, for its missing owner tag: a
// third answer a reread could give, beside served and absent.
func refusedSecret(name string) secrets.LocalSecret {
	secret := policytest.Secret(name, testApprover, policy.TierAgent, "v1")
	delete(secret.Tags, policy.TagOwner)
	return secret
}

// TestARereadAnswersACallerWithNoSessionTheSameForEveryName: POST /v1/secrets/{name}/reread,
// called with no proof, answers one answer for a secret the broker serves, one it serves as a
// type, one it refuses, and a name no secret carries: 202 and {"accepted":true}. So does a call
// whose proof the broker does not accept, a proof that is no proof and a session's proof naming an
// enrollment the broker does not hold, each its own one answer. Before, the reread answered every
// caller whether the broker served the name, why not and as which type.
func TestARereadAnswersACallerWithNoSessionTheSameForEveryName(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadOverallLimit = &rereadsFreely })
	ts.Secrets.Put(policytest.TypeSecret("APP_KEY", testApprover, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	ts.Secrets.Put(refusedSecret("UNOWNED_KEY"))
	names := map[string]string{
		"a secret the broker serves":   "WORKER_TOKEN",
		"a secret it serves as a type": "APP_KEY",
		"a secret it refuses":          "UNOWNED_KEY",
		"a name no secret carries":     "MISSING_KEY",
	}
	stranger := newSigningKey(t)
	for caller, headers := range map[string]func(path string) map[string]string{
		"no proof": func(string) map[string]string { return nil },
		"a proof that is no proof": func(string) map[string]string {
			return map[string]string{"Proof": strings.Join([]string{"not", "a", "proof"}, ".")}
		},
		"a proof naming an enrollment the broker does not hold": func(path string) map[string]string {
			signed, err := proof.Sign(stranger, uuid.NewString(), http.MethodPost, ts.URL+path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			return map[string]string{"Proof": signed}
		},
	} {
		answers := map[string]answerRead{}
		for what, name := range names {
			path := "/v1/secrets/" + name + "/reread"
			answers[what] = readAnswer(t, post(t, ts, path, headers(path), nil))
		}
		requireSameAnswers(t, "a reread with "+caller, answers)
		if got := answers["a name no secret carries"]; caller == "no proof" && (got.status != http.StatusAccepted || got.body != `{"accepted":true}`) {
			t.Fatalf("a reread with no proof = %d %s; want 202 {\"accepted\":true}", got.status, got.body)
		}
	}
}

// TestASessionsRereadStillGetsTheAnswerItGotBefore: a session that signs its reread is answered
// once the reread is done, as every caller was before: whether the broker serves the name, as
// which type, and why not, for each of the four names the row above asks about.
func TestASessionsRereadStillGetsTheAnswerItGotBefore(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadOverallLimit = &rereadsFreely })
	ts.Secrets.Put(policytest.TypeSecret("APP_KEY", testApprover, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	ts.Secrets.Put(refusedSecret("UNOWNED_KEY"))
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	for name, want := range map[string]string{
		"WORKER_TOKEN": `{"name":"WORKER_TOKEN","served":true}`,
		"APP_KEY":      `{"name":"APP_KEY","served":true,"type":"github-app"}`,
		"UNOWNED_KEY":  `{"name":"UNOWNED_KEY","served":false,"reason":"owner-tag-missing"}`,
		"MISSING_KEY":  `{"name":"MISSING_KEY","served":false,"reason":"absent"}`,
	} {
		status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/secrets/"+name+"/reread", nil)
		if status != http.StatusOK || strings.TrimSpace(string(body)) != want {
			t.Fatalf("a session's reread of %s = %d %s; want 200 %s", name, status, body, want)
		}
	}
}

// heldDescriber holds every DescribeSecret a reread makes until release is closed, after it has
// said which secret it was asked about on entered, and then answers it as store does.
type heldDescriber struct {
	secrets.SecretsManagerAPI
	entered chan<- string
	release <-chan struct{}
}

func (d heldDescriber) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	d.entered <- aws.ToString(in.SecretId)
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.SecretsManagerAPI.DescribeSecret(ctx, in, opts...)
}

// heldFor bounds how long a row waits for something the broker should do at once while the store
// is held: an answer that waits on the store never comes, so it is what the row fails after.
const heldFor = 5 * time.Second

// TestARereadAnswersACallerWithNoSessionAtOnceAndRereadsAfter: with the store holding every
// describe it is asked, a reread with no proof is answered 202 at once, for a name the broker
// serves, a name it has not read yet and a name no secret carries alike, so how long the answer
// takes says nothing of what the store holds. The reread still runs after the answer: once the
// store answers, the broker serves the secret it had not read and tells the other pods of each
// name. Before, the answer waited on the store, which takes one call to say it holds no secret by
// a name and more to describe one it holds.
func TestARereadAnswersACallerWithNoSessionAtOnceAndRereadsAfter(t *testing.T) {
	local := secrets.NewLocal(policytest.Secret("WORKER_TOKEN", policy.OwnerShared, policy.TierAgent, "worker-v1"))
	entered := make(chan string, 8)
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	loader := policytest.Loader(local)
	loader.Store = policytest.StoreThrough(local, heldDescriber{SecretsManagerAPI: local, entered: entered, release: release})
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan string, 8)
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		RereadOverallLimit: &rereadsFreely,
		NotifyReread:       func(_ context.Context, name string) { notified <- name }})
	// NEW_KEY is in the store only from now on, so only a reread can make the broker serve it.
	local.Put(policytest.Secret("NEW_KEY", testApprover, policy.TierAgent, "v1"))

	names := []string{"WORKER_TOKEN", "NEW_KEY", "MISSING_KEY"}
	for _, name := range names {
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/secrets/"+name+"/reread", nil))
		}()
		select {
		case <-done:
		case <-time.After(heldFor):
			t.Fatalf("a reread of %s with no proof answered nothing within %s while the store held its describe; want 202 at once", name, heldFor)
		}
		if recorder.Code != http.StatusAccepted || strings.TrimSpace(recorder.Body.String()) != `{"accepted":true}` {
			t.Fatalf("a reread of %s with no proof while the store held its describe = %d %s; want 202 {\"accepted\":true}", name, recorder.Code, recorder.Body.String())
		}
	}
	select {
	case id := <-entered:
		if !slices.ContainsFunc(names, func(name string) bool { return policytest.ID(name) == id }) {
			t.Fatalf("the store was asked to describe %s; want one of the names reread, %v", id, names)
		}
	case <-time.After(heldFor):
		t.Fatalf("no reread asked the store anything within %s of its answer; want the reread run after the answer", heldFor)
	}
	if _, served := cur.Get().Secrets["NEW_KEY"]; served {
		t.Fatal("the broker serves NEW_KEY while the store still holds its describe; want it served only once the store answers")
	}

	released = true
	close(release)
	var told []string
	for range names {
		select {
		case name := <-notified:
			told = append(told, name)
		case <-time.After(heldFor):
			t.Fatalf("the other pods were told of %v within %s of the store answering; want each of %v", told, heldFor, names)
		}
	}
	if slices.Sort(told); !slices.Equal(told, slices.Sorted(slices.Values(names))) {
		t.Fatalf("the other pods were told of %v; want each of %v once", told, names)
	}
	if _, served := cur.Get().Secrets["NEW_KEY"]; !served {
		t.Fatal("the broker does not serve NEW_KEY once the reread a caller with no proof asked for has run; want it served")
	}
}

// failingDescriber fails every DescribeSecret as a throttled Secrets Manager does.
type failingDescriber struct{ secrets.SecretsManagerAPI }

func (failingDescriber) DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	return nil, &throttled{}
}

// throttled is the error Secrets Manager answers a caller past its rate with.
type throttled struct{}

func (*throttled) Error() string {
	return "operation error Secrets Manager: DescribeSecret, ThrottlingException"
}

// TestARereadNoCallerWaitsOnLogsTheStoresFailure: a reread run after its 202, whose store fails,
// has no caller to be told of it, so it is the broker's failure and it logs the ERROR line
// "broker: reread secret failed" that an operator's alert counts, naming the secret.
func TestARereadNoCallerWaitsOnLogsTheStoresFailure(t *testing.T) {
	local := secrets.NewLocal()
	loader := policytest.Loader(local)
	loader.Store = policytest.StoreThrough(local, failingDescriber{SecretsManagerAPI: local})
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Register(mux, api.Deps{Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN})
	logged := policytest.CaptureLog(t)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("a reread with no proof = %d %s; want 202", recorder.Code, recorder.Body.String())
	}
	if !testwait.Holds(heldFor, 10*time.Millisecond, func() bool {
		text := logged.String()
		return strings.Contains(text, "ERROR broker: reread secret failed") && strings.Contains(text, "NEW_KEY")
	}) {
		t.Fatalf("logged %q within %s of the answer; want the ERROR line naming NEW_KEY", logged.String(), heldFor)
	}
}

// TestSettingsStayOpenToACallerWithNoCredential is the census's answer for GET /v1/settings as a
// test: a machine's `agent-secrets` reads its Google exchange audience here before it holds any
// credential of the broker's, to mint the identity token it trades for its first one, and the
// secret forms read the store, the prefix and the key under a cloud credential alone. So a caller
// with no credential is answered 200 with all of it, the very answer a session and Dispatch get.
func TestSettingsStayOpenToACallerWithNoCredential(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.GoogleExchangeAudience = "agent-secrets-vm" })
	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, open := ts.req(t, http.MethodGet, "/v1/settings", nil, nil)
	settings := decode[map[string]any](t, open)
	if status != http.StatusOK || settings["secrets_prefix"] != policytest.Prefix || settings["kms_key_arn"] != policytest.KeyARN ||
		settings["aws_account_id"] == nil || settings["google_exchange_audience"] != "agent-secrets-vm" {
		t.Fatalf("GET /v1/settings with no credential = %d %s; want 200 naming the prefix, the key, its account and the exchange audience", status, open)
	}
	for caller, answer := range map[string]func() (int, []byte){
		"a session": func() (int, []byte) { return ts.session(t, key, enrollmentID, http.MethodGet, "/v1/settings", nil) },
		"Dispatch":  func() (int, []byte) { return ts.ui(t, http.MethodGet, "/v1/settings", nil) },
	} {
		if status, body := answer(); status != http.StatusOK || string(body) != string(open) {
			t.Fatalf("GET /v1/settings as %s = %d %s; want the answer a caller with no credential got, %s", caller, status, body, open)
		}
	}
}
