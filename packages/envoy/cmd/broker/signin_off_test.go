package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The secrets the brokers below serve: two of one person's, the human-tier one any session needs
// that person to approve and the agent-tier one a pod (a session of no person) needs that person
// to approve; the launcher service's own, which its pods get at once; and a shared agent-tier
// one, which every session gets at once. The owner is an example address that names no one.
const (
	signInOffOwner         = "owner@example.com"
	signInOffHumanSecret   = "OWNER_DEPLOY_KEY"
	signInOffAgentSecret   = "OWNER_READ_TOKEN"
	signInOffServiceSecret = "LAUNCHER_TOKEN"
	signInOffServiceValue  = "launcher-token-value"
	signInOffSharedSecret  = "SHARED_READ_TOKEN"
	signInOffSharedValue   = "shared-read-token-value"
)

// signInOffSecrets writes the fake secrets file both brokers below serve and answers its path.
func signInOffSecrets(t *testing.T) string {
	t.Helper()
	secret := func(slug, owner, tier, value string) string {
		return `{"name": "` + testSecretsPrefix + slug + `", "kms_key_id": "` + testSecretsKMSKeyARN + `", "tags": {"owner": "` + owner + `", "tier": "` + tier + `"}, "value": "` + value + `"}`
	}
	content := `{"secrets": [` + strings.Join([]string{
		secret("owner-deploy-key", signInOffOwner, "human", "owner-deploy-key-value"),
		secret("owner-read-token", signInOffOwner, "agent", "owner-read-token-value"),
		secret("launcher-token", approvalLauncherService, "agent", signInOffServiceValue),
		secret("shared-read-token", "shared", "agent", signInOffSharedValue),
	}, ", ") + `]}`
	path := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signInOffEnv turns the broker's person sign-in off: BROKER_SIGNIN=off, and the four settings
// productionEnv gives every broker these tests start cleared, since off refuses beside them. Its
// sweep runs every second, so a row waiting on what the sweep does waits a second, not five.
func signInOffEnv() []string {
	return []string{
		"BROKER_SIGNIN=off",
		"BROKER_SIGNIN_ISSUER=", "BROKER_SIGNIN_CLIENT_ID=",
		"BROKER_SIGNIN_CLIENT_SECRET_FILE=", "BROKER_SIGNIN_DOMAIN=",
		"BROKER_SWEEP_SECONDS=1",
	}
}

// signInOnJudgeDetail is the model judge's boot line on a broker that signs people in and has no
// judge, as main logs it: the sign-in-off change leaves that broker's line as it was.
const signInOnJudgeDetail = "none of BROKER_JUDGE_MODEL_URL, BROKER_JUDGE_MODEL, BROKER_JUDGE_MODEL_AUDIENCE, BROKER_JUDGE_DISPATCH_URL, BROKER_JUDGE_DISPATCH_TOKEN_FILE is set, so every human-tier request waits for the person who decides it today"

// TestTheBinaryWithSignInOffServesNoApprovalPageAndRefusesWhatAPersonWouldApprove drives the
// REAL compiled broker on a real Postgres. First with the switch unset and the four sign-in
// settings given, the control: a pod's request for the owner's human-tier secret waits on the
// owner's record, whose page sends the browser to sign in, and the sign-in's status is served.
// Then the same database, behind the same public URL, is served by a broker with
// BROKER_SIGNIN=off and no sign-in setting at all, the broker a deployment runs while how its
// people sign in is not settled. It starts, saying so at boot; every route of the approval page
// and of its sign-in is a 404; the request the first run left pending is denied by its sweep,
// naming the setting, and leaves Dispatch's list; a new request for a secret a person would
// approve (the human-tier one, and the agent-tier one, which a pod of no person would need the
// owner to approve) is denied at once, with no record, no page to open and nothing waiting; and
// what the policy grants at once (the launcher service's own secret, a shared agent-tier secret)
// is granted and released as on any broker.
func TestTheBinaryWithSignInOffServesNoApprovalPageAndRefusesWhatAPersonWouldApprove(t *testing.T) {
	f := newApprovalFixture(t)
	f.secrets = signInOffSecrets(t)
	databaseURL := storetest.URL(t)
	public := newFront(t)
	base := public.URL
	dispatchPending := func(t *testing.T) (int, string) {
		t.Helper()
		status, answer := accountCall(t, base, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(signInOffOwner),
			map[string]string{"Authorization": "Bearer " + f.dispatchToken(t)}, nil)
		return status, string(answer)
	}

	on := f.start(t, databaseURL, base, f.signInEnv()...)
	public.route(t, on)
	pods := f.pods(t, base)
	pod := pods.Enroll(pods.LogIn(approvalLauncherService, approvalLauncherAccount), "pod-a", approvalPodAccount)
	var left brokertest.RequestReply
	t.Run("control: with the switch unset a request waits on the owner's page", func(t *testing.T) {
		if len(recordsWith(on.Boot, "approval sign-in on", nil)) != 1 || len(recordsWith(on.Boot, "approval sign-in off", nil)) != 0 {
			t.Fatalf("the control's boot log does not say the approval sign-in is on:\n%s", on.Boot)
		}
		if detail, _ := on.AwaitRecord(t, "model judge", map[string]any{"state": "off"})["detail"].(string); detail != signInOnJudgeDetail {
			t.Fatalf("the control's model judge line reads %q, want main's %q", detail, signInOnJudgeDetail)
		}
		left = pods.Ask(pod, signInOffHumanSecret)
		if left.State != "pending" || left.RecordID == nil || left.ApproveURL != base+"/approve/"+*left.RecordID {
			t.Fatalf("the control's request for %s = %+v, want pending on the owner's record with its page", signInOffHumanSecret, left)
		}
		b := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
		requireSignInRedirect(t, f.issuer, b.Get(t, left.ApproveURL), "the control's GET "+left.ApproveURL)
		if res := b.Get(t, "/signin/status"); res.Status != http.StatusOK {
			t.Fatalf("the control's GET /signin/status = %d %s, want 200", res.Status, res.Body)
		}
		if status, answer := dispatchPending(t); status != http.StatusOK || !strings.Contains(answer, *left.RecordID) {
			t.Fatalf("the control's GET /v1/pending = %d %s, want the owner's record", status, answer)
		}
	})
	if left.RecordID == nil {
		t.FailNow()
	}
	on.Terminate(t)
	if err := on.AwaitExit(t); err != nil {
		t.Fatalf("the sign-in-on broker exited with %v, want 0:\n%s", err, on.Log())
	}

	off := f.start(t, databaseURL, base, signInOffEnv()...)
	public.route(t, off)
	if len(recordsWith(off.Boot, "approval sign-in off", nil)) != 1 || strings.Contains(off.Boot, "approval sign-in on") {
		t.Fatalf("the boot log does not say the approval sign-in is off, and that alone:\n%s", off.Boot)
	}
	// The judge's boot line agrees with the sign-in's: with sign-in off no request waits for a
	// person, so the line no longer says one does.
	if detail, _ := off.AwaitRecord(t, "model judge", map[string]any{"state": "off"})["detail"].(string); !strings.Contains(detail, "BROKER_SIGNIN=off") || strings.Contains(detail, "waits for the person") {
		t.Fatalf("the sign-in-off broker's model judge line reads %q, want it to say no request waits for a person under BROKER_SIGNIN=off", detail)
	}

	t.Run("no route of the approval page or its sign-in is served", func(t *testing.T) {
		b := brokertest.NewBrowser(t, base, http.DefaultTransport, f.issuer)
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, "/approve/" + *left.RecordID},
			{http.MethodPost, "/approve"},
			{http.MethodGet, "/approve/assets/approve.css"},
			{http.MethodGet, "/signin/callback?code=example-code&state=example-state"},
			{http.MethodPost, "/signin/logout"},
			{http.MethodGet, "/signin/status"},
		} {
			var res brokertest.Response
			if call.method == http.MethodPost {
				res = b.FromPage(t, call.path, nil)
			} else {
				res = b.Get(t, call.path)
			}
			if res.Status != http.StatusNotFound {
				t.Errorf("%s %s = %d %s, want 404: a broker with sign-in off serves no approval page", call.method, call.path, res.Status, res.Body)
			}
		}
	})

	t.Run("the request the sign-in-on run left pending is denied by the sweep", func(t *testing.T) {
		reply := testwait.Until(t, "the sweep of the sign-in-off broker to end the request the first run left pending", func() (brokertest.RequestReply, bool) {
			reply := pods.State(pod, left.RequestID)
			return reply, reply.State != "pending"
		})
		if reply.State != "denied" || reply.GrantID != nil || reply.ApproveURL != "" || reply.Detail != policy.ReasonSignInOff {
			t.Fatalf("the request the first run left = %+v, want denied with no grant and no page, reading %q", reply, policy.ReasonSignInOff)
		}
		if status, answer := dispatchPending(t); status != http.StatusOK || !strings.Contains(answer, `"pending":[]`) {
			t.Fatalf("Dispatch's GET /v1/pending for the owner = %d %s, want 200 with nothing pending", status, answer)
		}
	})

	t.Run("a request a person would approve is denied at once, naming the setting", func(t *testing.T) {
		for _, ask := range []struct{ secret, reason string }{
			{signInOffHumanSecret, "pod-a needs " + signInOffHumanSecret},
			{signInOffAgentSecret, "pod-a needs " + signInOffAgentSecret},
			// With no reason too: asking again with one would be refused the same way, so the
			// denial names the switch and not the missing reason.
			{signInOffHumanSecret, ""},
		} {
			reply := pods.AskWithReason(pod, ask.secret, ask.reason)
			t.Logf("pod-a's request for %s (reason %q) = %+v", ask.secret, ask.reason, reply)
			if reply.State != "denied" || reply.RecordID != nil || reply.GrantID != nil || reply.ApproveURL != "" {
				t.Fatalf("pod-a's request for %s (reason %q) = %+v, want denied with no record, no grant and no page", ask.secret, ask.reason, reply)
			}
			if want := ask.secret + ": " + policy.ReasonSignInOff; reply.Detail != want {
				t.Fatalf("pod-a's request for %s (reason %q) was denied %q, want %q", ask.secret, ask.reason, reply.Detail, want)
			}
		}
		if status, answer := dispatchPending(t); status != http.StatusOK || !strings.Contains(answer, `"pending":[]`) {
			t.Fatalf("Dispatch's GET /v1/pending for the owner = %d %s, want 200 with nothing pending", status, answer)
		}
	})

	t.Run("what the policy grants at once is granted and released as on any broker", func(t *testing.T) {
		for secret, value := range map[string]string{signInOffServiceSecret: signInOffServiceValue, signInOffSharedSecret: signInOffSharedValue} {
			reply := pods.Ask(pod, secret)
			if reply.State != "granted" || reply.GrantID == nil || reply.Detail != "" {
				t.Fatalf("pod-a's request for %s = %+v, want granted at once", secret, reply)
			}
			if values := pods.Values(pod, *reply.GrantID); values[secret] != value {
				t.Fatalf("pod-a's grant of %s released %v, want %s=%s", secret, values, secret, value)
			}
		}
	})
}
