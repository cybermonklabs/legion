package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// wireMachineTokens is GET /v1/admin/machine-tokens's answer as the wire carries it.
type wireMachineTokens struct {
	Project       string     `json:"project"`
	LiveTokens    int        `json:"live_tokens"`
	LastExpiresAt *time.Time `json:"last_expires_at"`
	Record        *int64     `json:"record"`
}

// knownPrivate is the store of private projects as rows: key → its channels joined by commas, for
// each row the store holds (KnownPrivateProjects).
func knownPrivate(t *testing.T, ts *testServer) map[string]string {
	t.Helper()
	held, err := ts.Store.KnownPrivateProjects(context.Background())
	if err != nil {
		t.Fatalf("read the store of private projects: %v", err)
	}
	rows := map[string]string{}
	for _, project := range held {
		rows[project.Key] = strings.Join(project.SlackChannels, ",")
	}
	return rows
}

// TestTheSwitchsCountRecordsThePrivateProjectFirst: a count with slack_channels and
// deadline_seconds, the switch's own call, records the project and its channels in the store of
// private projects and answers the record's number beside the count, a later one a greater number
// and the union of the channels; a count with no slack_channels, as devbox-tokens-at-switch.sh asks
// it, records nothing and answers no record. A channel that is no conversation id, 17 channels, and
// a deadline missing, no whole number or over 90 s are 400 RECORD_INPUT, and record nothing.
func TestTheSwitchsCountRecordsThePrivateProjectFirst(t *testing.T) {
	ts, _, listenerToken := newGrantLogServer(t)
	count := func(t *testing.T, bearer, query string) (int, []byte) {
		t.Helper()
		return ts.req(t, http.MethodGet, "/v1/admin/machine-tokens"+query, map[string]string{"Authorization": "Bearer " + bearer}, nil)
	}
	status, body := count(t, ts.uiToken, "?project=PRIV")
	if answer := decode[wireMachineTokens](t, body); status != http.StatusOK || answer.Record != nil || len(knownPrivate(t, ts)) != 0 {
		t.Fatalf("a count with no slack_channels = %d %s, store %v; want 200, no record and nothing recorded", status, body, knownPrivate(t, ts))
	}
	status, body = count(t, ts.uiToken, "?project=PRIV&slack_channels=C0PRIVAAAA1&deadline_seconds=90")
	first := decode[wireMachineTokens](t, body)
	if status != http.StatusOK || first.Record == nil || *first.Record <= 0 || knownPrivate(t, ts)["PRIV"] != "C0PRIVAAAA1" {
		t.Fatalf("the switch's count of PRIV = %d %s, store %v; want 200, a record and PRIV with C0PRIVAAAA1", status, body, knownPrivate(t, ts))
	}
	status, body = count(t, ts.uiToken, "?project=PRIV&slack_channels=D0PRIVBBBB2&deadline_seconds=1")
	second := decode[wireMachineTokens](t, body)
	if status != http.StatusOK || second.Record == nil || *second.Record <= *first.Record || knownPrivate(t, ts)["PRIV"] != "C0PRIVAAAA1,D0PRIVBBBB2" {
		t.Fatalf("a second count of PRIV = %d %s, store %v; want a later record and both channels", status, body, knownPrivate(t, ts))
	}
	status, body = count(t, ts.uiToken, "?project=NONE&slack_channels=&deadline_seconds=90")
	if answer := decode[wireMachineTokens](t, body); status != http.StatusOK || answer.Record == nil {
		t.Fatalf("a count of NONE naming no channel = %d %s, want 200 and a record", status, body)
	}
	if channels, held := knownPrivate(t, ts)["NONE"]; !held || channels != "" {
		t.Fatalf("the store holds NONE %t with channels %q, want held with none", held, channels)
	}
	seventeen := strings.TrimSuffix(strings.Repeat("C0SEVENTEEN,", 17), ",")
	for _, query := range []string{
		"?project=BAD&slack_channels=general&deadline_seconds=90",
		"?project=BAD&slack_channels=" + seventeen + "&deadline_seconds=90",
		"?project=BAD&slack_channels=C0BADAAAAA1",
		"?project=BAD&slack_channels=C0BADAAAAA1&deadline_seconds=91",
		"?project=BAD&slack_channels=C0BADAAAAA1&deadline_seconds=ninety",
		"?project=BAD&slack_channels=C0BADAAAAA1&deadline_seconds=0",
	} {
		status, body := count(t, ts.uiToken, query)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "RECORD_INPUT" {
			t.Errorf("GET /v1/admin/machine-tokens%s = %d %s, want 400 RECORD_INPUT", query, status, body)
		}
	}
	if _, held := knownPrivate(t, ts)["BAD"]; held {
		t.Fatal("a refused record wrote BAD into the store")
	}
	if status, body := count(t, listenerToken, "?project=PRIV&slack_channels=&deadline_seconds=90"); status != http.StatusForbidden {
		t.Fatalf("the switch's count as the listener = %d %s, want 403", status, body)
	}
}

// TestTheForgetRouteForgetsARecordItsSwitchLeft: POST /v1/admin/private-projects/forget answers
// Dispatch's caller alone. A forget naming the project's current record forgets it: 200
// forgotten, and the store no longer holds the project. A forget naming a record a later switch
// replaced is 200 superseded, and the row stays. A forget naming no record while the record is
// younger than the hold is 409 RECORD_IN_FLIGHT with retry_after_seconds within the hold. A body
// naming no project key, or a record that is not positive, is 400 PROJECT_INPUT, and the listener
// is 403.
func TestTheForgetRouteForgetsARecordItsSwitchLeft(t *testing.T) {
	ts, _, listenerToken := newGrantLogServer(t)
	as := func(bearer string) map[string]string { return map[string]string{"Authorization": "Bearer " + bearer} }
	record := func(t *testing.T, project string) int64 {
		t.Helper()
		status, body := ts.req(t, http.MethodGet, "/v1/admin/machine-tokens?project="+project+"&slack_channels=&deadline_seconds=90", as(ts.uiToken), nil)
		answer := decode[wireMachineTokens](t, body)
		if status != http.StatusOK || answer.Record == nil {
			t.Fatalf("record %s: %d %s", project, status, body)
		}
		return *answer.Record
	}
	forget := func(t *testing.T, bearer string, body any) (int, []byte) {
		t.Helper()
		return ts.req(t, http.MethodPost, "/v1/admin/private-projects/forget", as(bearer), body)
	}

	first := record(t, "PRIV")
	status, body := forget(t, ts.uiToken, map[string]any{"project": "PRIV", "record": first})
	if status != http.StatusOK || !strings.Contains(string(body), `"outcome":"forgotten"`) {
		t.Fatalf("forget PRIV's record %d = %d %s, want 200 forgotten", first, status, body)
	}
	if _, held := knownPrivate(t, ts)["PRIV"]; held {
		t.Fatal("the store still holds PRIV after its record was forgotten")
	}

	stale, current := record(t, "OTHER"), int64(0)
	current = record(t, "OTHER")
	status, body = forget(t, ts.uiToken, map[string]any{"project": "OTHER", "record": stale})
	if status != http.StatusOK || !strings.Contains(string(body), `"outcome":"superseded"`) {
		t.Fatalf("forget OTHER's replaced record %d = %d %s, want 200 superseded", stale, status, body)
	}
	if _, held := knownPrivate(t, ts)["OTHER"]; !held {
		t.Fatalf("the store dropped OTHER for a forget of its replaced record %d, while its record %d stands", stale, current)
	}
	status, body = forget(t, ts.uiToken, map[string]any{"project": "OTHER", "record": nil})
	refusal := decode[struct {
		Code              string `json:"code"`
		RetryAfterSeconds int64  `json:"retry_after_seconds"`
	}](t, body)
	if status != http.StatusConflict || refusal.Code != "RECORD_IN_FLIGHT" || refusal.RetryAfterSeconds < 1 || refusal.RetryAfterSeconds > 180 {
		t.Fatalf("forget OTHER with no record while its record is young = %d %s, want 409 RECORD_IN_FLIGHT within 180 s", status, body)
	}

	for _, bad := range []any{map[string]any{"project": "two keys", "record": nil}, map[string]any{"project": "OTHER", "record": 0}, "not an object"} {
		if status, body := forget(t, ts.uiToken, bad); status != http.StatusBadRequest || decode[wireError](t, body).Code != "PROJECT_INPUT" {
			t.Errorf("forget %v = %d %s, want 400 PROJECT_INPUT", bad, status, body)
		}
	}
	if status, body := forget(t, listenerToken, map[string]any{"project": "OTHER", "record": current}); status != http.StatusForbidden {
		t.Fatalf("forget as the listener = %d %s, want 403", status, body)
	}
}

// TestOnlyDispatchCountsDeveloperBoxTokens: GET /v1/admin/machine-tokens answers Dispatch's caller
// how many live tokens act as their person and list the project, a developer box's, and when the
// last expires. A token that lists the project among others counts; a named token, a revoked one
// and an expired one do not, nor one of another project. A key no token lists answers 0 and no
// expiry, and a query that is no one project key is 400 PROJECT_INPUT. The access review and the
// listener, each listed for powers of their own, are 403 ACCOUNT_LACKS_POWER.
func TestOnlyDispatchCountsDeveloperBoxTokens(t *testing.T) {
	ts, reviewToken, listenerToken := newGrantLogServer(t)
	ctx := context.Background()
	soon, later := time.Now().Add(10*time.Minute).UTC().Truncate(time.Second), time.Now().Add(14*time.Minute).UTC().Truncate(time.Second)
	for _, row := range []struct {
		signature, scope, projects string
		active                     bool
		expires                    time.Time
	}{
		{"box-priv", "person", "PRIV", true, soon},
		{"box-acme-priv", "person", "ACME PRIV", true, later},
		{"named-priv", "named", "PRIV", true, later},
		{"revoked-priv", "person", "PRIV", false, later},
		{"expired-priv", "person", "PRIV", true, time.Now().Add(-time.Minute)},
		{"box-acme", "person", "ACME", true, later},
	} {
		if _, err := ts.Store.Pool.Exec(ctx, `insert into oauth_access_tokens (signature, request_id, request, active, expires_at)
			values ($1, $1, jsonb_build_object('session', jsonb_build_object('extra', jsonb_build_object('project_scope', $2::text, 'projects', $3::text))), $4, $5)`,
			row.signature, row.scope, row.projects, row.active, row.expires); err != nil {
			t.Fatalf("store the token %s: %v", row.signature, err)
		}
	}
	count := func(t *testing.T, bearer, query string) (int, []byte) {
		t.Helper()
		return ts.req(t, http.MethodGet, "/v1/admin/machine-tokens"+query, map[string]string{"Authorization": "Bearer " + bearer}, nil)
	}

	status, body := count(t, ts.uiToken, "?project=PRIV")
	answer := decode[wireMachineTokens](t, body)
	if status != http.StatusOK || answer.Project != "PRIV" || answer.LiveTokens != 2 || answer.LastExpiresAt == nil || !answer.LastExpiresAt.Equal(later) {
		t.Fatalf("GET /v1/admin/machine-tokens?project=PRIV as dispatch = %d %s, want 200 counting box-priv and box-acme-priv, the last expiring at %s", status, body, later)
	}
	status, body = count(t, ts.uiToken, "?project=NONE")
	if answer := decode[wireMachineTokens](t, body); status != http.StatusOK || answer.LiveTokens != 0 || answer.LastExpiresAt != nil {
		t.Fatalf("GET /v1/admin/machine-tokens?project=NONE as dispatch = %d %s, want 200, 0 and no expiry", status, body)
	}
	for _, query := range []string{"", "?project=priv", "?project=PRIV+ACME"} {
		status, body := count(t, ts.uiToken, query)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "PROJECT_INPUT" {
			t.Errorf("GET /v1/admin/machine-tokens%s as dispatch = %d %s, want 400 PROJECT_INPUT", query, status, body)
		}
	}
	for name, bearer := range map[string]string{"access-review": reviewToken, "listener": listenerToken} {
		status, body := count(t, bearer, "?project=PRIV")
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_LACKS_POWER" {
			t.Errorf("GET /v1/admin/machine-tokens as %s = %d %s, want 403 ACCOUNT_LACKS_POWER: the count is Dispatch's question alone", name, status, body)
		}
	}
}
