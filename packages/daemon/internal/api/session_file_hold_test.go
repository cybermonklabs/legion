package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// An Oh My Pi relaunch resumes the session FILE its claim recorded, whatever session id the claim
// recorded: the Agent Sandbox runtime starts it with `--resume=<recorded file>`
// (runtime/sandbox/manifest.go, agentArgv), and the launcher checks only that the deployment's one
// session table holds that file (launcher's resumable). That is the takeover this pull request
// closes for a Claude Code transcript (holdRecorded), and it is open for an Oh My Pi session file:
// a role that registers a fresh id of its own with another claim's Oh My Pi session file is
// admitted and records that file, and its next relaunch continues the other claim's session, its
// transcript read and written. Its registration then reports that claim's id and is refused, so no
// grant moves; the session itself is taken.
//
// Each case holds the outcome and not a mechanism: a registration naming another claim's session
// file is refused 409 before anything of the claim changes and with no broker call; a claim whose
// record names another claim's session file, as a daemon before the hold could write it, is never
// resumed into it. At fb53e0e1f2 both cases fail: the registration is answered 200 and records
// the file, and the relaunch resumes it.
func TestAnOhMyPiSessionFileIsHeldToItsClaim(t *testing.T) {
	const holderFile = "/sessions/ses_holder.jsonl"

	t.Run("a registration naming another claim's session file", func(t *testing.T) {
		h, broker := sessionHoldHarness(t)
		holder, holderBoot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
		h.readyAt(holder, "ses_holder", h.registered(holderBoot, "ses_holder"))
		if got := h.stored(holder).SessionFile; got != holderFile {
			t.Fatalf("the holder records %q, want %s", got, holderFile)
		}

		taker, takerBoot := h.podRole("LEGION-901", claim.RoleTester, 1)
		before, calls := h.stored(taker), broker.calls()
		recorder := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
			BootToken: takerBoot, SessionID: "ses_fresh", OmpSessionFile: holderFile,
			AgentID: "agent-ses_fresh", PluginContract: DaemonAPIVersion,
		}, nil)
		if recorder.Code != http.StatusConflict {
			t.Errorf("%s registering a fresh id with %s's session file %s = %d, want 409; body %s", taker, holder, holderFile, recorder.Code, recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), string(holder)) {
			t.Errorf("the refusal %s names %s, the claim that holds the session", recorder.Body, holder)
		}
		if got := h.stored(taker); got.SessionFile == holderFile || got.Session != before.Session {
			t.Errorf("the refused claim records session %q and file %q; want neither the fresh id nor %s's file", got.Session, got.SessionFile, holder)
		}
		if got := broker.calls(); got != calls {
			t.Errorf("the broker received %d calls from the refused registration, want none", got-calls)
		}
	})

	t.Run("a relaunch resuming another claim's session file", func(t *testing.T) {
		h, _ := sessionHoldHarness(t)
		holder, holderBoot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
		h.readyAt(holder, "ses_holder", h.registered(holderBoot, "ses_holder"))

		taker, takerBoot := h.podRole("LEGION-902", claim.RoleImplementer, 1)
		h.readyAt(taker, "ses_taker", h.registered(takerBoot, "ses_taker"))
		m, _ := h.supervisor.Machine(taker)
		if err := m.Handle(h.ctx, supervise.RequestSuspend{Claim: taker, By: supervise.SuspendedByOperator}); err != nil {
			t.Fatal(err)
		}
		// A record a daemon before the hold wrote: the taker's own session id, and the holder's
		// Oh My Pi session file, which a resume continues.
		stored := h.stored(taker)
		stored.SessionFile = holderFile
		if err := h.store.PutClaim(h.ctx, stored); err != nil {
			t.Fatal(err)
		}
		h.supervisor.restart(t, taker)
		h.runtime.ScriptResume(fake.SpawnResult{Locator: podLocator(taker, "pod-legion-902", 2)})
		resumes := len(h.runtime.CallsOf("Resume"))

		recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(taker)+"/resume", nil)
		for _, call := range h.runtime.CallsOf("Resume")[resumes:] {
			if call.Spec.ResumeSessionFile == holderFile {
				t.Errorf("the runtime was asked to resume %s's session file %s for %s (resume answered %d %s)",
					holder, holderFile, taker, recorder.Code, strings.TrimSpace(recorder.Body.String()))
			}
		}
	})
}
