//go:build e2e

// The live proof's driver for dispatch://INFRA-3144's check c-live-proof: the daemon releases the
// Dispatch claim of a session it has left behind, a move out of the workflow releases the claim of
// its tree's session, and a release that leaves a claim on purpose says why in the issue's record.
// scripts/e2e/left-session-claims.sh builds this driver with `-tags e2e` and owns the nats-server it
// runs against, which it names in LEGION_E2E_NATS_URL; the PostgreSQL is LEGION_TEST_PG_DSN's.
//
// Nothing of the release path is stood in for. The rig is the person-handover proofs'
// (person_handover_scratch_dispatch_test.go), as register_gate_basis_live_test.go runs it: the real
// Dispatch server this checkout builds, on a database of its own, publishing its events on that
// broker, and the whole daemon (run, through startDaemonLogging) with dispatch_url at it, reading
// those events. Dispatch judges a session's claim by that session's record in the Envoy registry,
// which it reads from the listener by id, and it refuses to judge one whose listener does not
// answer (packages/envoy/internal/dispatch/api/issue_claim.go, fetchHolderLiveness and
// claimBlockedBy). So the run also starts the Envoy listener this checkout builds, on the same
// broker (startSessionsListener), and starts Dispatch with ENVOY_URL at it.
//
// A session comes up as a pane's plugins bring it up: registered with the daemon (POST
// /legion/v1/claims/register) and then with the listener (POST /v1/interests/subscribe, asking for
// its own consumer, as pi-envoy registers). Its process ends with that registration: DELETE
// /v1/sessions/<id>, which a plugin sends at its stop and the daemon's clean stop sends for a role
// whose process it found gone, after which the listener answers the session 404, and Dispatch reads
// it gone. Each claim a session makes is the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts
// under bun) run as that session; a person's claim and each move are Dispatch's own routes under the
// trusted identity header; the controller's fresh_session is the daemon's own route under a
// controller grant. Every assertion is a read of Dispatch's events and issue, of what the CLI
// printed, of the listener's registry, or of the daemon's state, record route and operator claims.
//
// What is stood in for lies outside the release: GitHub (blockersGitHub), the Apps' tokens, and the
// runtime: the package's fake runtime launches every claim, and no agent runs. The daemon's own
// Envoy address is the rig's default, not this run's listener, so its notices and its clean stops
// are no part of what Dispatch reads here.
//
// Each step runs as a subtest of its own (step), so a step that fails ends there and the next step
// still runs, and a failure names the assertion it failed at (fails): the script's negative
// control runs this driver over the commit the change starts from and reads each step's own
// failure. So the driver names no Go symbol that commit lacks: it reads `claimLeft` from the record
// route as plain JSON, as a role's read_record hands it on, and the contract it states below the
// claim-left entry is the number the build it runs in gives that entry, read at run time
// (api.NumberedEntries). Over that commit the driver builds and runs, and its steps fail where the
// change is missing.
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// leftClaimsWait bounds each read-back's wait for what a claim release does. The release runs on
// the outbox's claim lane once the transaction that queued it commits; a release Dispatch refused
// while it read the holder live waits out a backoff that caps at 64 seconds, so two minutes holds
// one such retry, and a release that never comes fails well inside the driver's deadline.
const leftClaimsWait = 2 * time.Minute

// leftClaimsReader is the session every `dispatch read` of the run is made as: no session that
// claims anything, so a read changes nothing a step holds.
const leftClaimsReader = "ses-left-claims-reader"

// leftClaimsContractHeader is the request header a caller states its plugin's contract in, which
// the daemon places it by (internal/api's pluginContractHeader, shaping.go).
const leftClaimsContractHeader = "Legion-Plugin-Contract"

// leftClaimsEntrySlug is the slug of the contract entry `claimLeft` arrived at
// (internal/api/contract_20261127202231_claim-left.go), as `legion version --contracts` prints it.
const leftClaimsEntrySlug = "claim-left"

// leftClaimsPerson is the person every person's act of the run is made as: the trusted identity
// header the rig's Dispatch takes (signedInAsThePerson), whose holder the daemon writes
// `user:smoke`.
var leftClaimsPerson = scratchActor{Kind: "user", ID: "smoke"}

// leftSessionClaimsLive is one run of the proof: the rig, the checkout whose dispatch CLI runs and
// the home it runs under, the Envoy listener Dispatch reads every session holder from, and the
// assertion the running step failed at (fails), which step prints.
type leftSessionClaimsLive struct {
	*personHandoverRig
	checkout, cliHome string
	listener          string
	client            *http.Client
	failedAt          string
}

// TestLeftSessionClaimsLive drives the proof's steps against one booted daemon, one Dispatch and
// one listener.
func TestLeftSessionClaimsLive(t *testing.T) {
	// Not parallel: a live proof whose daemon, scratch Dispatch and listener run on the one NATS
	// server LEGION_E2E_NATS_URL names, and which writes the file LEGION_E2E_DAEMON_LOG names and the
	// dispatch.log and listener.log beside it, which a proof beside it would share.
	r := newLeftSessionClaimsLive(t)
	r.step("a-fresh-session-releases-the-left-session", r.freshSessionReleasesTheLeftSession)
	r.step("a-move-releases-its-trees-claim", r.moveReleasesTheTreesClaim)
	r.step("a-persons-claim-stays-with-its-reason", r.personsClaimStays)
}

// newLeftSessionClaimsLive starts the listener on the script's broker, then Dispatch on that broker
// with ENVOY_URL at the listener, and boots the daemon on the rig with a cap of three, one slot for
// each step's root. The design gate is off, as the blockers proof runs it, so a root's architect's
// own registration of its design opens its tree and starts its planning.
func newLeftSessionClaimsLive(t *testing.T) *leftSessionClaimsLive {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker for Dispatch's events, the listener and the daemon's intake; scripts/e2e/left-session-claims.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	checkout := liveCLICheckout(t)
	logs := t.TempDir()
	daemonLog := os.Getenv(liveDaemonLogEnv)
	if daemonLog != "" {
		logs = filepath.Dir(daemonLog)
	}
	// The listener first, as approved-version-news.sh's part two starts the two on one broker:
	// Dispatch reconciles the ENVOY_NOTIFICATIONS stream there once it connects.
	listener := startSessionsListener(t, logs, natsURL)
	board := scratchDispatchOptions{natsURL: natsURL, root: checkout, env: []string{"ENVOY_URL=" + listener}}
	if daemonLog != "" {
		board.log = evidenceFile(t, filepath.Join(logs, "dispatch.log"))
	}
	rig := newPersonHandoverRigOn(t, board, 3)
	rig.cfg.Gates.Design = config.DesignGateOff.Steps()
	if daemonLog != "" {
		rig.daemonLog = evidenceFile(t, daemonLog)
	}
	r := &leftSessionClaimsLive{personHandoverRig: rig, checkout: checkout, cliHome: t.TempDir(), listener: listener,
		client: &http.Client{Timeout: 10 * time.Second}}
	r.boot()
	r.note("Dispatch %s (database of its own, events on %s, ENVOY_URL %s), project %s; the Envoy listener on the same broker; daemon %s, design gate off",
		r.baseURL, natsURL, listener, r.cfg.Project, r.d.base)
	return r
}

// ---- how a step runs and fails ------------------------------------------------------------------

// step runs one step as a subtest of its own, between its `== name` and `ok name (<s>s)` lines,
// with every read failing that subtest rather than the whole run: the rig's reads go through r.t
// and the daemon's through r.d.t, which are the step's own testing.T while the step runs. A step
// that fails prints `FAIL name at <assertion> (<s>s)`, naming the assertion it failed at (fails),
// and the next step runs.
func (r *leftSessionClaimsLive) step(name string, body func()) {
	parent := r.t
	parent.Run(name, func(t *testing.T) {
		r.t, r.d.t, r.failedAt = t, t, ""
		started := time.Now()
		defer func() {
			r.t, r.d.t = parent, parent
			if t.Failed() {
				at := r.failedAt
				if at == "" {
					at = "none of its named assertions (a wait or a read of its own)"
				}
				fmt.Printf("FAIL %s at %s (%ds)\n", name, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("== %s\n", name)
		body()
		fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
	})
}

// fails ends the running step at its named assertion, which step prints and the script's negative
// control reads back.
func (r *leftSessionClaimsLive) fails(assertion, format string, args ...any) {
	r.t.Helper()
	r.failedAt = assertion
	r.t.Fatalf(assertion+": "+format, args...)
}

// within waits for read to hold, for leftClaimsWait at most or what is left of the test binary's
// deadline less a second when that is sooner (testwait.Bounded), and fails the step at assertion
// ("" for a wait that is no named assertion), naming what it waited for, the bound it waited and
// what read last saw.
func (r *leftSessionClaimsLive) within(assertion, what string, read func() (string, bool)) {
	r.t.Helper()
	bound := testwait.Bounded(r.t, leftClaimsWait, time.Second)
	last := ""
	if testwait.Holds(bound, 250*time.Millisecond, func() bool {
		var held bool
		last, held = read()
		return held
	}) {
		return
	}
	if assertion == "" {
		r.t.Fatalf("waited %s for %s; the last read saw %s", bound, what, last)
	}
	r.fails(assertion, "waited %s for %s; the last read saw %s", bound, what, last)
}

// note prints what a step drove or read, under its heading.
func (r *leftSessionClaimsLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// ---- the steps ----------------------------------------------------------------------------------

// Step 1. A root in planning whose architect session S1 registered on the listener claims the root
// with the dispatch CLI. S1's process ends, and its registration with it, so Dispatch reads S1 gone.
// The controller's fresh_session on the architect leaves S1 behind and starts the architect again
// in a new session S2, which registers and claims nothing. Dispatch's events of the root then hold
// an issue.released whose actor is the daemon's own session and whose previous claim is S1's
// (released): the left session's own release, which the claim write that left S1 behind queued.
// `dispatch read` prints `Claimed by: nobody` (claimed-by-nobody).
func (r *leftSessionClaimsLive) freshSessionReleasesTheLeftSession() {
	root := r.create(asSession, "Invoice archive migration", map[string]any{"labels": []string{"legion"}})
	s1, s2 := leftClaimsSession(root.Key, "s1"), leftClaimsSession(root.Key, "s2")
	architect := r.rootInPlanning(root, s1)
	r.claimAs(root.Key, s1)
	r.endSession(s1)
	launched := r.launches(string(architect))
	r.controllerFreshSession(root.Key)
	testwait.Eventually(r.t, "the daemon to launch the architect of "+root.Key+" again after the fresh session", func() bool {
		return r.launches(string(architect)) > launched
	})
	r.bringUp(lastLaunch(r.t, r.rt, architect), s2)
	if got := r.d.claim(architect); got.Session != s2 {
		r.t.Fatalf("after the fresh session and the new launch's registration, %s records session %q, want %s", architect, got.Session, s2)
	}
	r.note("the controller's fresh_session on %s's architect answered 200; the daemon launched it again, and %s now records session %s, which claims nothing",
		root.Key, architect, s2)
	released := r.releasedByTheDaemon("released", root.Key, s1)
	r.note("read back: %s's %s", root.Key, describeClaimEvent(released))
	r.wantClaimedByNobody("claimed-by-nobody", root.Key)
}

// Step 2. A second root in planning whose architect session S3 claims it. S3's process ends, and
// its registration with it, so Dispatch reads S3 gone. The person moves the root to backlog and
// leaves it there, which takes it out of the workflow. Dispatch's events of the root then hold an
// issue.released by the daemon's own session of S3's claim (released), `dispatch read` prints
// `Claimed by: nobody` (claimed-by-nobody), the record names no claimLeft, since the release took
// the claim (no-claim-left), and Dispatch still shows the root in backlog (left-in-backlog).
func (r *leftSessionClaimsLive) moveReleasesTheTreesClaim() {
	root := r.create(asSession, "Webhook retry budget", map[string]any{"labels": []string{"legion"}})
	s3 := leftClaimsSession(root.Key, "s3")
	r.rootInPlanning(root, s3)
	r.claimAs(root.Key, s3)
	r.endSession(s3)
	r.movedToBacklog(root.Key)
	released := r.releasedByTheDaemon("released", root.Key, s3)
	r.note("read back: %s's %s", root.Key, describeClaimEvent(released))
	r.wantClaimedByNobody("claimed-by-nobody", root.Key)
	if left, read, answered := r.claimLeftServed(root.Key, 0); !answered || left != nil {
		r.fails("no-claim-left", "GET /legion/v1/issues/%s/record answered %s after the release took the claim, want a record naming no claimLeft", root.Key, read)
	}
	r.note("read back: GET /legion/v1/issues/%s/record names no claimLeft", root.Key)
	if shown := r.issue(root.Key).Status; shown != "backlog" {
		r.fails("left-in-backlog", "Dispatch shows %s %s after the release, want it left in backlog", root.Key, shown)
	}
	r.note("read back: Dispatch shows %s in backlog, where the person left it", root.Key)
}

// Step 3. The person claims a third root in planning, which then moves to backlog. The move's
// release leaves a person's claim, and GET /legion/v1/issues/<key>/record names claimLeft with
// holder user:<id>, reason person and the executor's time (claim-left). Dispatch keeps the claim
// once that release has run, with no issue.released, and `dispatch read` names the person
// (claim-stays). A caller stating the claim-left entry's own contract is served the entry
// (served-at-the-entry); one stating the contract just below it is served no claimLeft
// (none-below-the-entry).
func (r *leftSessionClaimsLive) personsClaimStays() {
	root := r.create(asSession, "Onboarding email copy", map[string]any{"labels": []string{"legion"}})
	r.rootInPlanning(root, leftClaimsSession(root.Key, "architect"))
	var claimed struct {
		Claim *struct {
			Actor scratchActor `json:"actor"`
		} `json:"claim"`
	}
	dispatchCall(r.t, r.baseURL, http.MethodPost, "/api/v1/issues/"+root.Key+"/claim", map[string]any{}, &claimed)
	if claimed.Claim == nil || claimed.Claim.Actor != leftClaimsPerson {
		r.t.Fatalf("the person's claim of %s answered the claim %+v, want %s:%s", root.Key, claimed.Claim, leftClaimsPerson.Kind, leftClaimsPerson.ID)
	}
	r.note("the person %s claimed %s through POST /api/v1/issues/%s/claim", leftClaimsPerson.ID, root.Key, root.Key)
	before := time.Now()
	r.movedToBacklog(root.Key)

	var left map[string]any
	var read string
	r.within("claim-left", "GET /legion/v1/issues/"+root.Key+"/record to name claimLeft", func() (string, bool) {
		left, read, _ = r.claimLeftServed(root.Key, 0)
		return read, left != nil
	})
	after := time.Now()
	holder := leftClaimsPerson.Kind + ":" + leftClaimsPerson.ID
	if left["holder"] != holder || left["reason"] != "person" {
		r.fails("claim-left", "GET /legion/v1/issues/%s/record answered %s, want claimLeft with holder %s and reason person", root.Key, read, holder)
	}
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(left["at"]))
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(after.Add(time.Second)) {
		r.fails("claim-left", "claimLeft of %s names the time %v (%v), want an RFC 3339 time on the executor's clock between the move (%s) and the read (%s)",
			root.Key, left["at"], err, before, after)
	}
	r.note("read back: GET /legion/v1/issues/%s/record answers %s", root.Key, read)

	// The release has run, since its verdict wrote that entry: Dispatch keeps the claim, and goes on
	// keeping it, with no release on the issue.
	deadline := time.Now().Add(liveSettle)
	for {
		if holds := r.dispatchHolder(root.Key); holds == nil || *holds != leftClaimsPerson {
			r.fails("claim-stays", "Dispatch shows %s's claim held by %+v once the release ran, want the person's %s to stay", root.Key, holds, holder)
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if releases := r.claimEvents(root.Key, "issue.released"); len(releases) != 0 {
		r.fails("claim-stays", "Dispatch's events of %s hold %v, want no issue.released of the person's claim", root.Key, releases)
	}
	line := r.claimedBy(root.Key)
	if !strings.HasPrefix(line, "Claimed by: "+leftClaimsPerson.ID+" since ") {
		r.fails("claim-stays", "dispatch read --issue %s prints %q, want the person's claim, %q", root.Key, line, "Claimed by: "+leftClaimsPerson.ID+" since <at>")
	}
	r.note("read back: for %s Dispatch still shows the claim of %s, its events hold no issue.released, and dispatch read --issue %s prints %q",
		liveSettle, holder, root.Key, line)

	entry, found := claimLeftEntry()
	if !found {
		r.fails("served-at-the-entry", "this build registers no contract entry %s; it registers %+v", leftClaimsEntrySlug, api.NumberedEntries())
	}
	if left, read, answered := r.claimLeftServed(root.Key, entry.Number); !answered || left == nil {
		r.fails("served-at-the-entry", "GET /legion/v1/issues/%s/record stating contract %d, the %s entry's own, answered %s, want claimLeft served",
			root.Key, entry.Number, entry.Slug, read)
	}
	below := entry.Number - 1
	if left, read, answered := r.claimLeftServed(root.Key, below); !answered || left != nil {
		r.fails("none-below-the-entry", "GET /legion/v1/issues/%s/record stating contract %d, just below the %s entry's %d, answered %s, want a record naming no claimLeft",
			root.Key, below, entry.Slug, entry.Number, read)
	}
	r.note("read back: stating %s %d, the number this build gives entry %d %s (api.NumberedEntries, as `legion version --contracts` prints it), the record names claimLeft; stating %d it names none",
		leftClaimsContractHeader, entry.Number, entry.ID, entry.Slug, below)
}

// ---- what the steps drive -----------------------------------------------------------------------

// leftClaimsSession is the session the run names name for root: no dot, colon or space, so the
// listener takes it as a key and the dispatch CLI as a session id.
func leftClaimsSession(root, name string) string {
	return "ses-" + strings.ToLower(root) + "-" + name
}

// rootInPlanning hands root, which a session created and labelled, to Legion as the person's todo,
// waits for it to take a slot and for its architect's launch, brings that architect up on session
// (bringUp), and has the architect register root's primary document as its design, as its tool
// does, which with the gate off opens the tree and starts its planning. It answers the architect's
// claim token.
func (r *leftSessionClaimsLive) rootInPlanning(root scratchIssue, session string) claim.Token {
	t := r.t
	t.Helper()
	setDispatchStatus(t, r.baseURL, root.Key, "todo")
	settled(t, r.d, "admit "+root.Key+" into a slot", func(s api.State) bool { return slices.Contains(s.Admission.Active, root.Key) })
	architect := r.architectToken(root.Key)
	testwait.Eventually(t, "the daemon to launch the architect of "+root.Key, func() bool { return r.launches(string(architect)) > 0 })
	secret := r.bringUp(lastLaunch(t, r.rt, architect), session)
	awaitDispatchStatus(t, r.baseURL, root.Key, "in_progress")
	grant := sessionGrants(t, r.d, session, secret, root.Key)
	if status, answer := r.d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: root.Key, ArtifactID: root.PrimaryArtifactID, Version: 1,
	}, false); status != http.StatusOK {
		t.Fatalf("the architect's design-gate registration on %s = %d; body %s", root.Key, status, answer)
	}
	settled(t, r.d, "start the planning of "+root.Key+" once its architect registered its design", func(s api.State) bool {
		return s.Issues[root.Key].Phase == phase.Planning
	})
	r.note("%s is in planning with a slot; its architect's claim %s is registered as session %s", root.Key, architect, session)
	return architect
}

// bringUp is the agent of launch coming up on session as a pane's plugins bring it up: the session
// registered with the daemon, as registerLaunch registers it but under a session of the step's own,
// and then with the listener (onTheListener). It answers the secret the daemon gave the session.
func (r *leftSessionClaimsLive) bringUp(launch runtime.SpawnSpec, session string) string {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s as session %s = %d; body %s", launch.Claim, session, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s from %s: %v", launch.Claim, body, err)
	}
	r.onTheListener(session, string(launch.Claim))
	return registered.Secret
}

// onTheListener registers session with the listener as pi-envoy does, asking for the session's own
// consumer, and waits for the registry to read it live: what Dispatch reads of that session from
// then on.
func (r *leftSessionClaimsLive) onTheListener(session, title string) {
	t := r.t
	t.Helper()
	status, answer := r.listenerCall(http.MethodPost, "/v1/interests/subscribe", map[string]any{
		"session_id": session, "title": title, "topics": []string{}, "capabilities": []string{}, "self_subscribed": true,
		"delivery": map[string]any{"consumer": true, "epoch": session + "-epoch"},
	})
	var registered struct {
		Delivery *struct {
			Mode string `json:"mode"`
		} `json:"delivery"`
	}
	if status != http.StatusOK || json.Unmarshal(answer, &registered) != nil || registered.Delivery == nil || registered.Delivery.Mode != "consumer" {
		t.Fatalf("the listener's registration of %s = %d %s, want 200 with its own consumer's delivery", session, status, answer)
	}
	r.within("", "the listener to read "+session+" live", func() (string, bool) {
		status, answer := r.registryRead(session)
		var info struct {
			State string `json:"state"`
		}
		return fmt.Sprintf("%d %s", status, answer), status == http.StatusOK && json.Unmarshal(answer, &info) == nil && info.State == "live"
	})
}

// endSession is session's process ending, and its registration with it (DELETE /v1/sessions/<id>,
// as a plugin's stop and the daemon's clean stop send it). It waits for the listener to answer the
// session 404 on the read Dispatch judges a claim's holder by, which Dispatch reads as gone.
func (r *leftSessionClaimsLive) endSession(session string) {
	t := r.t
	t.Helper()
	if status, answer := r.listenerCall(http.MethodDelete, "/v1/sessions/"+url.PathEscape(session), nil); status != http.StatusOK {
		t.Fatalf("the listener's DELETE of %s's registration = %d %s, want 200", session, status, answer)
	}
	var last string
	r.within("", "the listener to read "+session+" gone", func() (string, bool) {
		status, answer := r.registryRead(session)
		last = fmt.Sprintf("%d %s", status, answer)
		return last, status == http.StatusNotFound
	})
	r.note("%s's process ended: DELETE /v1/sessions/%s answered 200, and GET /v1/registry/%s?include=unresponsive answers %s, so Dispatch reads it gone",
		session, session, session, last)
}

// registryRead is GET /v1/registry/<session>?include=unresponsive, the read Dispatch judges a
// claim's holder by (envoy.Client.Session).
func (r *leftSessionClaimsLive) registryRead(session string) (int, []byte) {
	r.t.Helper()
	return r.listenerCall(http.MethodGet, "/v1/registry/"+url.PathEscape(session)+"?include=unresponsive", nil)
}

// listenerCall sends one request to the listener, with body as JSON when it is not nil, and answers
// its status and body whatever they are.
func (r *leftSessionClaimsLive) listenerCall(method, path string, body any) (int, []byte) {
	t := r.t
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, r.listener+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		t.Fatalf("the listener's %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the listener's answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// claimAs is session claiming key with `dispatch claim --issue <key>`, as its agent's shell runs it,
// and Dispatch showing session holding key's claim.
func (r *leftSessionClaimsLive) claimAs(key, session string) {
	t := r.t
	t.Helper()
	out, code := r.cli(session, "claim", "--issue", key)
	printed := string(out.stdout) + string(out.stderr)
	if code != 0 || !strings.Contains(printed, key+": claimed by you since ") {
		t.Fatalf("dispatch claim --issue %s as %s exited %d printing %q, want the claim", key, session, code, printed)
	}
	if holds := r.dispatchHolder(key); holds == nil || *holds != (scratchActor{Kind: "session", ID: session}) {
		t.Fatalf("Dispatch shows %s's claim held by %+v after dispatch claim as %s", key, holds, session)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(printed), "\n")
	r.note("dispatch claim --issue %s as %s: %s", key, session, line)
}

// controllerFreshSession is the project controller's fresh_session on key's architect (POST
// /legion/v1/claims/fresh-session), under a controller grant minted with the operator's bearer
// (POST /legion/v1/grants), as `legion` mints the grant its controller's calls carry.
func (r *leftSessionClaimsLive) controllerFreshSession(key string) {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants", map[string]any{}, true)
	if status != http.StatusOK {
		t.Fatalf("the controller grant = %d; body %s", status, body)
	}
	var grant api.GrantResponse
	if err := json.Unmarshal(body, &grant); err != nil {
		t.Fatalf("decode the controller grant %s: %v", body, err)
	}
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		api.ClaimRequest{GrantID: grant.GrantID, Issue: key, Role: string(claim.RoleArchitect)}, false); status != http.StatusOK {
		t.Fatalf("the controller's fresh_session on %s's architect = %d, want 200; body %s", key, status, body)
	}
}

// movedToBacklog is the person moving key to backlog at the dashboard, once the daemon's own
// Dispatch writes for key have run, and the daemon recording key out of the workflow off that
// move's event.
func (r *leftSessionClaimsLive) movedToBacklog(key string) {
	r.t.Helper()
	r.dispatchRowsDone(key)
	r.setStatus(key, "backlog")
	r.within("", "the daemon to record "+key+" out of the workflow", func() (string, bool) {
		recorded := r.record(key)
		return fmt.Sprintf("phase %s, status %s, Dispatch status %s", recorded.Phase, recorded.Status, recorded.DispatchStatus), record.Stopped(recorded)
	})
	r.note("the person moved %s to backlog, and the daemon records it out of the workflow", key)
}

// ---- what the steps read ------------------------------------------------------------------------

// leftClaimPayload is the part of an issue.claimed or issue.released payload a step reads: the
// claim the event replaced or cleared, and why.
type leftClaimPayload struct {
	Previous *struct {
		Actor scratchActor `json:"actor"`
	} `json:"previous_claim"`
	Reason string `json:"reason"`
}

// describeClaimEvent is a claim event of Dispatch's as a step prints it: `event <seq> <type> by
// <actor> of <previous holder>'s claim (<reason>)`.
func describeClaimEvent(event scratchEvent) string {
	var payload leftClaimPayload
	_ = json.Unmarshal(event.Payload, &payload)
	previous := "nobody"
	if payload.Previous != nil {
		previous = payload.Previous.Actor.Kind + ":" + payload.Previous.Actor.ID
	}
	return fmt.Sprintf("event %d %s by %s:%s of %s's claim (%s)", event.Seq, event.Type, event.Actor.Kind, event.Actor.ID, previous, payload.Reason)
}

// claimEvents is every event of key of the kinds named, oldest first, as describeClaimEvent writes
// each.
func (r *leftSessionClaimsLive) claimEvents(key string, kinds ...string) []string {
	r.t.Helper()
	var events []string
	for _, event := range r.issueEvents(key) {
		if slices.Contains(kinds, event.Type) {
			events = append(events, describeClaimEvent(event))
		}
	}
	return events
}

// releasedByTheDaemon waits, for leftClaimsWait at most, for Dispatch's events of key to hold an
// issue.released whose actor is the daemon's own session (dispatch.DaemonSession) and whose
// previous claim is session's, and answers it. Its failure, at assertion, names every claim event
// key holds.
func (r *leftSessionClaimsLive) releasedByTheDaemon(assertion, key, session string) scratchEvent {
	r.t.Helper()
	daemon := scratchActor{Kind: "session", ID: dispatch.DaemonSession(r.cfg.Project)}
	held := scratchActor{Kind: "session", ID: session}
	var released scratchEvent
	r.within(assertion, fmt.Sprintf("Dispatch's events of %s to hold an issue.released by %s:%s of session:%s's claim", key, daemon.Kind, daemon.ID, session), func() (string, bool) {
		var seen []string
		for _, event := range r.issueEvents(key) {
			if event.Type != "issue.claimed" && event.Type != "issue.released" {
				continue
			}
			seen = append(seen, describeClaimEvent(event))
			var payload leftClaimPayload
			if event.Type == "issue.released" && event.Actor == daemon && json.Unmarshal(event.Payload, &payload) == nil &&
				payload.Previous != nil && payload.Previous.Actor == held {
				released = event
				return strings.Join(seen, "; "), true
			}
		}
		if len(seen) == 0 {
			return "no claim event", false
		}
		return "the claim events " + strings.Join(seen, "; "), false
	})
	return released
}

// wantClaimedByNobody holds, at assertion, that `dispatch read --issue <key>` prints `Claimed by:
// nobody`.
func (r *leftSessionClaimsLive) wantClaimedByNobody(assertion, key string) {
	r.t.Helper()
	if line := r.claimedBy(key); line != "Claimed by: nobody" {
		r.fails(assertion, "dispatch read --issue %s prints %q after the release, want %q", key, line, "Claimed by: nobody")
	}
	r.note("read back: dispatch read --issue %s prints \"Claimed by: nobody\"", key)
}

// claimedBy is the `Claimed by:` line `dispatch read --issue <key>` prints, read as
// leftClaimsReader.
func (r *leftSessionClaimsLive) claimedBy(key string) string {
	t := r.t
	t.Helper()
	out, code := r.cli(leftClaimsReader, "read", "--issue", key)
	if code != 0 {
		t.Fatalf("dispatch read --issue %s exited %d: %s%s", key, code, out.stdout, out.stderr)
	}
	for line := range strings.Lines(string(out.stdout)) {
		if trimmed := strings.TrimRight(line, "\n"); strings.HasPrefix(trimmed, "Claimed by: ") {
			return trimmed
		}
	}
	t.Fatalf("dispatch read --issue %s printed no `Claimed by:` line:\n%s", key, out.stdout)
	return ""
}

// cli runs the dispatch CLI as session in the run's own shell (cliShell), at this run's Dispatch
// with its agent token.
func (r *leftSessionClaimsLive) cli(session string, args ...string) (cliOutput, int) {
	r.t.Helper()
	return cliShell{t: r.t, dir: r.cliHome, session: session, base: r.baseURL, token: scratchAgentToken}.bun(nil, dispatchCLI(r.checkout), args...)
}

// dispatchHolder is the actor Dispatch shows holding key's claim, nil for nobody.
func (r *leftSessionClaimsLive) dispatchHolder(key string) *scratchActor {
	r.t.Helper()
	var read struct {
		Claim *struct {
			Actor scratchActor `json:"actor"`
		} `json:"claim"`
	}
	dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
	if read.Claim == nil {
		return nil
	}
	return &read.Claim.Actor
}

// claimLeftServed is key's claimLeft as GET /legion/v1/issues/<key>/record serves it to a caller
// stating contract in leftClaimsContractHeader (0 states none, and is served the daemon's own
// contract), decoded as a role's read_record reads the body: plain JSON. It answers the entry, nil
// where the record names none, what the read saw (the status and the claimLeft text, or the whole
// answer when it is no record), and whether the route answered a record at all.
func (r *leftSessionClaimsLive) claimLeftServed(key string, contract int) (map[string]any, string, bool) {
	t := r.t
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, r.d.base+"/legion/v1/issues/"+url.PathEscape(key)+"/record", nil)
	if err != nil {
		t.Fatal(err)
	}
	if contract > 0 {
		request.Header.Set(leftClaimsContractHeader, strconv.Itoa(contract))
	}
	response, err := r.d.client.Do(request)
	if err != nil {
		t.Fatalf("GET the record of %s: %v", key, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	var body map[string]json.RawMessage
	if response.StatusCode != http.StatusOK || json.Unmarshal(answer, &body) != nil {
		return nil, fmt.Sprintf("%d %s", response.StatusCode, answer), false
	}
	served, named := body["claimLeft"]
	if !named || string(served) == "null" {
		return nil, "200, claimLeft absent", true
	}
	var left map[string]any
	if err := json.Unmarshal(served, &left); err != nil || left == nil {
		return nil, fmt.Sprintf("200, claimLeft %s, which is no object", served), true
	}
	return left, fmt.Sprintf("200, claimLeft %s", served), true
}

// claimLeftEntry is the claim-left contract entry with the number the build the driver runs in
// gives it, and whether that build registers one at all.
func claimLeftEntry() (api.NumberedEntry, bool) {
	for _, entry := range api.NumberedEntries() {
		if entry.Slug == leftClaimsEntrySlug {
			return entry, true
		}
	}
	return api.NumberedEntry{}, false
}
