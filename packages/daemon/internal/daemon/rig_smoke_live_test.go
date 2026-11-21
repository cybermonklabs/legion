//go:build e2e

// The rig's own proof (RIG.md): that what it says it runs is what runs, and that each seam the
// checks after w3 build on does what it says, before a check reads anything through it.
// scripts/e2e/live-rig-smoke.sh builds this driver and runs it on lib/live-rig.sh's run.
//
// One claim, whose pane's turns go to the scripted stand-in, and the scratch Dispatch: the two
// options w3 does not take, so between the two proofs every part of the rig has run.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TestTheRigServesWhatItsChecksNameLive runs the rig's parts one step each.
func TestTheRigServesWhatItsChecksNameLive(t *testing.T) {
	r := startRig(t, rigOptions{script: "scripts/e2e/live-rig-smoke.sh", standin: true, dispatch: true})
	key := r.cfg.Project + "-1"
	token := r.token(key, claim.RoleImplementer)
	r.seed(rigSeed{issue: key, phase: phase.Implementing, role: claim.RoleImplementer, task: "Say the word SMOKE and nothing else."})
	r.boot()
	r.awaitReady(token)
	session := r.claim(token).Session
	r.step("the-listener-is-the-pinned-one-and-each-user-holds-its-grant", func() { r.smokeGrants(token, session) })
	r.step("the-front-stops-and-lets-through-a-sessions-registrations", func() { r.smokeFront(session) })
	r.step("nats-stops-and-starts-and-the-session-connects-again", func() { r.smokeNATSRestart(token, session) })
	r.step("the-daemon-restarts-on-its-own-port-and-the-pane-binds-again", func() { r.smokeDaemonRestart(token) })
	r.step("the-scratch-dispatch-publishes-on-the-rigs-nats", r.smokeDispatch)
}

// smokeGrants reads what the rig started: the pinned listener with production's session age, each
// process connected to NATS as its own user, the pane's turn answered by the stand-in, and a
// client that presents no credential refused a publish its grant does not hold.
func (r *rig) smokeGrants(token claim.Token, session string) {
	health := r.listenerHealth()
	if health["status"] != "healthy" || health["session_max_age"] != "5m0s" {
		r.fails("listener-health", "the listener's /healthz reads %v, want healthy with session_max_age 5m0s", health)
	}
	users := map[string]int{}
	paneAsMembers := false
	for _, c := range r.nats.connections() {
		user := r.nats.userOf(c)
		users[user]++
		if c.Name == "omp-"+session && user == "members" {
			paneAsMembers = true
		}
	}
	r.note("NATS connections by user: %v", users)
	if users["listener"] == 0 || users["daemon"] == 0 || users["dispatch"] == 0 || !paneAsMembers {
		r.fails("users", "/connz shows %v and the pane's session as members %v; want the listener, the daemon and Dispatch each as its own user and the session as members", users, paneAsMembers)
	}
	r.await("the pane's task to be answered by the stand-in", idleWait, func() bool {
		return r.claim(token).State == string(supervise.StateIdle) && r.standin.turnsHolding("SMOKE") > 0
	})
	refused := make(chan error, 1)
	probe, err := nats.Connect(r.nats.url, nats.Name("legion-rig grant probe"), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		select {
		case refused <- err:
		default:
		}
	}))
	if err != nil {
		r.fails("members-grant", "connect with no credential: %v", err)
	}
	defer probe.Close()
	if err := probe.Publish("notifications.role.legion-rig-probe", []byte("{}")); err != nil {
		r.fails("members-grant", "publish: %v", err)
	}
	_ = probe.Flush()
	select {
	case err := <-refused:
		if !errors.Is(err, nats.ErrPermissionViolation) && !strings.Contains(err.Error(), "Permissions Violation") {
			r.fails("members-grant", "the publish was refused with %v, want a permissions violation", err)
		}
		r.note("a client with no credential is members: its publish to notifications.role.legion-rig-probe was refused: %v", err)
	case <-time.After(10 * time.Second):
		r.fails("members-grant", "a client with no credential published to notifications.role.legion-rig-probe unrefused; the server admits it as more than members")
	}
}

// smokeFront stops the session's registrations at the front, reads that the front refused one,
// and lets them through again, after which the listener reads the session registering again.
func (r *rig) smokeFront(session string) {
	r.front.blockRegistrations(session)
	blocked := time.Now()
	r.await("the front to refuse a registration of "+session, 3*time.Minute, func() bool { return len(r.front.refusals(session)) > 0 })
	refusal := r.front.refusals(session)[0]
	r.front.unblock(session)
	unblocked := time.Now()
	r.note("the front refused %s's %s at %s, %s after the block", session, refusal.path, clockAt(refusal.at), refusal.at.Sub(blocked).Round(time.Second))
	var seen int64
	r.await("the listener to read "+session+" registering again", 3*time.Minute, func() bool {
		seen = r.lastSeen(session)
		return seen >= unblocked.UnixMilli()
	})
	r.note("the listener read %s's registration at %s, after the front let it through at %s", session, clockAt(time.UnixMilli(seen)), clockAt(unblocked))
}

// lastSeen is the session's last registration as the listener's own list reads it, 0 when the
// list has no row for it.
func (r *rig) lastSeen(session string) int64 {
	r.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, r.listener.BaseURL()+"/v1/sessions?include=unresponsive", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.listener.Token())
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("read the listener's session list: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var rows []struct {
		SessionID string `json:"session_id"`
		LastSeen  int64  `json:"last_seen"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		r.t.Fatalf("decode the session list %s: %v", body, err)
	}
	for _, row := range rows {
		if row.SessionID == session {
			return row.LastSeen
		}
	}
	return 0
}

// smokeNATSRestart stops the nats-server and starts it again on the same ports and store: every
// client reconnects, the pane's session as members again and the daemon as its own user.
func (r *rig) smokeNATSRestart(token claim.Token, session string) {
	var before uint64
	for _, c := range r.nats.connections() {
		if c.Name == "omp-"+session {
			before = c.CID
		}
	}
	r.nats.stop()
	stopped := time.Now()
	r.nats.start()
	r.note("the nats-server stopped at %s and served again at %s on %s", clockAt(stopped), clockAt(time.Now()), r.nats.url)
	var after uint64
	r.await("the session and the daemon to connect again", 3*time.Minute, func() bool {
		daemon := false
		after = 0
		for _, c := range r.nats.connections() {
			if c.Name == "omp-"+session && r.nats.userOf(c) == "members" {
				after = c.CID
			}
			if r.nats.userOf(c) == "daemon" {
				daemon = true
			}
		}
		return after != 0 && daemon
	})
	r.note("%s's session connected again as members (connection %d, was %d), and the daemon as its own user", token, after, before)
}

// smokeDaemonRestart restarts the daemon on its own API port and socket: the pane's shim dials
// the new daemon through the relay, which binds it, and the claim is live again.
func (r *rig) smokeDaemonRestart(token claim.Token) {
	launch := r.runtime.current(token)
	stopped, serving := r.restartDaemon()
	r.await(string(token)+"'s shim to bind the new daemon", 2*time.Minute, func() bool {
		for _, c := range r.relay.connsOf(launch.spec.BootToken) {
			if !c.ackAt.IsZero() && c.opened.After(stopped) {
				return true
			}
		}
		return false
	})
	r.await(string(token)+" live after the restart", 2*time.Minute, func() bool {
		state := r.claim(token).State
		return state == string(supervise.StateReady) || state == string(supervise.StateIdle)
	})
	r.note("the daemon stopped at %s and served again at %s on port %d; %s's shim (generation %d, pid %d) bound it again and the claim is %s",
		clockAt(stopped), clockAt(serving), r.cfg.Port, token, launch.spec.Generation, launch.pane.pid, r.claim(token).State)
}

// smokeDispatch creates an issue on the scratch Dispatch and reads its event where Dispatch
// published it: the rig's stream, as Dispatch's own NATS user.
func (r *rig) smokeDispatch() {
	key := createScratchDispatchIssue(r.t, r.scratch.base)
	subject := "notifications.dispatch.issue." + key + ".>"
	var stored []storedMessage
	r.await("Dispatch's event for "+key+" on the rig's stream", 2*time.Minute, func() bool {
		stored = r.nats.storedOn(subject)
		return len(stored) > 0
	})
	r.note("the scratch Dispatch created %s and the stream holds %d of its events under %s, the first stored at %s", key, len(stored), subject, clockAt(stored[0].at))
}
