// packages/envoy/cmd/cipher/signin_renewer_linux_test.go
//
// A person's sign-in on a machine keeps its enrollment while the machine needs it: the built
// agent-secrets against the real broker (ciphertest.NewRig: real handlers, real Postgres) with a
// lease of a few seconds, and the broker's own sweep (enroll.Service.EndLapsed) run by the test.
// Before the sign-in renewer, nothing renewed a sign-in's enrollment, so its lease lapsed after
// BROKER_LEASE_SECONDS whatever it held: the sweep then revoked a grant a person had approved and
// cancelled a request still waiting for one. They skip without BROKER_TEST_DATABASE_URL, as every
// rig test does. Linux-only: each test finds the renewers it started through /proc, and stops them
// by their own pid when it ends.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

// renewalLease is the lease the rig's broker grants: a few seconds, so each test sees several
// leases pass, and long enough that a renewer on a loaded machine renews well inside it: the
// renewer has a third of it for each renewal call. At 3 seconds, one run in about ten of these
// tests together under -race on a loaded dev box failed the grant test, so it is 5.
const renewalLease = 5 * time.Second

// renewalSecretValue is the value the rig's human-tier TEST_SECRET holds; the use command's child
// prints its length, so no test output carries it.
const renewalSecretValue = "test-secret-v1"

// renewalRig is one machine with a person's sign-in on it: the chainRig's own home, runtime and
// state directories, and a sign-in record naming an enrollment the rig's broker holds under that
// person, with the sign-in's box key as the enrollment's key.
type renewalRig struct {
	t            *testing.T
	broker       *ciphertest.Rig
	machine      *chainRig
	enrollmentID string
}

func newRenewalRig(t *testing.T) *renewalRig {
	t.Helper()
	broker := ciphertest.NewRig(t, ciphertest.WithLease(renewalLease))
	machine := newChainRig(t, chainBinary(t), nil)
	machine.env["AGENT_SECRETS_URL"] = broker.URL
	enrollmentID, key := broker.NewSession(t, broker.Operator)
	// The session's lease is the rig's, as the broker's own registration gives one: InsertSession
	// writes an hour.
	if _, err := broker.Store.Pool.Exec(context.Background(),
		`update enrollments set lease_expires_at = now() + make_interval(secs => $2) where id = $1`,
		enrollmentID, renewalLease.Seconds()); err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := cipherclient.SignIns(machine.getenv).Write(cipherclient.SignInRecord{
		Person: broker.Operator, Issuer: "https://accounts.google.com", ClientID: "device-client", RefreshToken: "not-a-secret-refresh-token",
		BoxKey: string(keyPEM(key)), Thumbprint: thumbprint, EnrollmentID: enrollmentID, SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	rig := &renewalRig{t: t, broker: broker, machine: machine, enrollmentID: enrollmentID}
	t.Cleanup(rig.stopRenewers)
	return rig
}

// sweep runs the broker's sweep once, as BROKER_SWEEP_SECONDS runs it: it ends every enrollment
// whose lease has lapsed, revoking its grants and cancelling its pending requests.
func (r *renewalRig) sweep() {
	r.t.Helper()
	if _, err := (&enroll.Service{Store: r.broker.Store}).EndLapsed(context.Background()); err != nil {
		r.t.Fatalf("the broker's sweep: %v", err)
	}
}

// waitLeases lets n leases pass, sweeping as the broker does, a sweep each half lease and one at
// the end: an enrollment nothing renewed in that time is ended by then.
func (r *renewalRig) waitLeases(n int) {
	r.t.Helper()
	end := time.Now().Add(time.Duration(n) * renewalLease)
	for time.Now().Before(end) {
		time.Sleep(min(renewalLease/2, time.Until(end)))
		r.sweep()
	}
}

// enrollmentLive reports whether the sign-in's enrollment is live at the broker: not revoked, and
// its lease not lapsed.
func (r *renewalRig) enrollmentLive() bool {
	r.t.Helper()
	var live bool
	if err := r.broker.Store.Pool.QueryRow(context.Background(),
		`select revoked_at is null and enrollment_lease_live(lease_expires_at) from enrollments where id = $1`, r.enrollmentID).Scan(&live); err != nil {
		r.t.Fatal(err)
	}
	return live
}

// requestRow is what the broker holds of one secret request.
type requestRow struct {
	ID, State, RecordID, Detail string
}

// requests is every request of the sign-in's enrollment, oldest first.
func (r *renewalRig) requests() []requestRow {
	r.t.Helper()
	rows, _ := r.broker.Store.Pool.Query(context.Background(),
		`select id::text, state, coalesce(record_id, ''), coalesce(decision_detail, '') from requests where enrollment_id = $1 order by created_at`, r.enrollmentID)
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (requestRow, error) {
		var req requestRow
		err := row.Scan(&req.ID, &req.State, &req.RecordID, &req.Detail)
		return req, err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// grantOf is the grant request requestID made and whether it is live, or "" where it made none.
func (r *renewalRig) grantOf(requestID string) (grantID, revokedBy string, live bool) {
	r.t.Helper()
	err := r.broker.Store.Pool.QueryRow(context.Background(),
		`select id::text, coalesce(revoked_by, ''), revoked_at is null and (expires_at is null or expires_at > now()) from grants where request_id = $1`, requestID).
		Scan(&grantID, &revokedBy, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return grantID, revokedBy, live
}

// useSecret is the use command for TEST_SECRET with a grant of a minute and the wait given, whose
// child prints the length of the value it got (chain_test.go's useArgs is TEST_KEY's).
func (r *renewalRig) useSecret(wait string) []string {
	return []string{"TEST_SECRET", "--reason", "check the sign-in renewal", "--ttl", "1m", "--wait", wait, "--", "sh", "-c", "echo ${#TEST_SECRET}"}
}

// assertSessionLeader fails t unless process pid leads a session of its own (/proc/<pid>/stat's
// sixth field is its pid), as `setsid` makes the renewer: no hang-up of the command's terminal and
// no signal to the command's process group reaches it.
func (r *renewalRig) assertSessionLeader(pid int) {
	r.t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		r.t.Fatal(err)
	}
	// The command name, the second field, is in parentheses and may hold spaces.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 4 || fields[3] != strconv.Itoa(pid) {
		r.t.Fatalf("renewer %d: /proc stat fields after the name %q; want its session (the fourth) to be its own pid", pid, fields)
	}
}

// renewerPIDs is every agent-secrets renewer of this rig's sign-in that runs now: a process of the
// rig's binary whose arguments are `renew --sign-in` and whose HOME is the rig's.
func (r *renewalRig) renewerPIDs() []int {
	r.t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		r.t.Fatal(err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(args) != 3 || args[0] != r.machine.binary || args[1] != "renew" || args[2] != "--sign-in" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, variable := range strings.Split(string(environ), "\x00") {
			if variable == "HOME="+r.machine.home {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// stopRenewers ends every renewer of this rig's sign-in, each by its own pid, and waits until none
// runs, so no test leaves one behind renewing at a broker that is gone.
func (r *renewalRig) stopRenewers() {
	for _, pid := range r.renewerPIDs() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	r.waitFor("every renewer of the rig's sign-in to end", func() bool { return len(r.renewerPIDs()) == 0 })
}

// waitFor waits up to a minute for condition, asking ten times a second: each reading scans /proc
// or asks the broker's database.
func (r *renewalRig) waitFor(what string, condition func() bool) {
	r.t.Helper()
	testwait.WithinPolling(r.t, time.Minute, 100*time.Millisecond, what, condition)
}

// renewalDir is the sign-in's renewal directory.
func (r *renewalRig) renewalDir() string {
	return cipherclient.SignIns(r.machine.getenv).RenewalDir(r.broker.Operator)
}

// renewerLog is what the sign-in's renewers wrote, for a failure message.
func (r *renewalRig) renewerLog() string {
	raw, _ := os.ReadFile(filepath.Join(r.renewalDir(), "renewer.log"))
	return string(raw)
}

// rewriteRecord changes the sign-in's record with edit, as a command or a sign-in writes it.
func (r *renewalRig) rewriteRecord(edit func(*cipherclient.SignInRecord)) {
	r.t.Helper()
	store := cipherclient.SignIns(r.machine.getenv)
	record, err := store.Read(store.Path(r.broker.Operator))
	if err != nil {
		r.t.Fatal(err)
	}
	edit(&record)
	if err := store.Write(record); err != nil {
		r.t.Fatal(err)
	}
}

// assertRenewalModes fails t unless the renewal directory and what a command made in it are its
// owner's alone: the directories 0700, the files 0600.
func (r *renewalRig) assertRenewalModes() {
	r.t.Helper()
	for name, want := range map[string]os.FileMode{"": 0o700 | os.ModeDir, "pending": 0o700 | os.ModeDir, "used": 0o600, "lock": 0o600, "renewer.log": 0o600} {
		info, err := os.Stat(filepath.Join(r.renewalDir(), name))
		if err != nil {
			r.t.Fatalf("the renewal directory's %q: %v", name, err)
		}
		if got := info.Mode() & (os.ModeDir | os.ModePerm); got != want {
			r.t.Fatalf("the renewal directory's %q has mode %v, want %v", name, got, want)
		}
	}
}

// killRenewer kills the sign-in's one renewer by its pid, as a kill of its process tree does, and
// answers that pid once it is gone.
func (r *renewalRig) killRenewer() int {
	r.t.Helper()
	var pids []int
	r.waitFor("one renewer of the sign-in", func() bool { pids = r.renewerPIDs(); return len(pids) == 1 })
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		r.t.Fatal(err)
	}
	r.waitFor("the killed renewer to be gone", func() bool { return !slices.Contains(r.renewerPIDs(), pids[0]) })
	return pids[0]
}

// TestAPersonApprovedGrantOutlivesTheSignInsLease: a person approves a sign-in's request for a
// human-tier secret, for a minute, and no command runs for four leases after it. The grant is
// still live then, on the same enrollment, so the same session's next use of the secret is served
// by that grant at once and asks nobody again. In the second row the renewer is killed after the
// approval and one command of the sign-in runs within the lease, which starts a renewer again.
// Before the renewer, the sweep revoked the grant with the enrollment one lease after the
// enrollment was made, as a dev box showed on 2026-11-24.
func TestAPersonApprovedGrantOutlivesTheSignInsLease(t *testing.T) {
	for _, kill := range []bool{false, true} {
		t.Run(map[bool]string{false: "renewed", true: "renewer killed"}[kill], func(t *testing.T) {
			rig := newRenewalRig(t)
			code, stdout, stderr := rig.machine.run("request", "TEST_SECRET", "--reason", "check the sign-in renewal", "--ttl", "1m", "--json")
			if code != exitPending {
				t.Fatalf("request: exit %d, stdout %q, stderr %q; want %d, waiting for the secret's person", code, stdout, stderr, exitPending)
			}
			var asked RequestResult
			if err := json.Unmarshal([]byte(stdout), &asked); err != nil || asked.RecordID == nil {
				t.Fatalf("request --json printed %q (%v); want the pending request with its record", stdout, err)
			}
			rig.broker.Decide(t, *asked.RecordID, true)
			grantID, _, live := rig.grantOf(asked.RequestID)
			if !live {
				t.Fatalf("request %s made no live grant once its person approved it", asked.RequestID)
			}
			if kill {
				killed := rig.killRenewer()
				if code, stdout, stderr := rig.machine.run("self"); code != 0 {
					t.Fatalf("self after the renewer was killed: exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
				}
				rig.waitFor("a renewer that the command started again", func() bool {
					pids := rig.renewerPIDs()
					return len(pids) == 1 && pids[0] != killed
				})
			}

			rig.waitLeases(4)

			if _, revokedBy, live := rig.grantOf(asked.RequestID); !live || !rig.enrollmentLive() {
				t.Fatalf("after four leases with no command: the grant %s is live %v (revoked by %q) and the enrollment is live %v; want both live (renewer log %q)",
					grantID, live, revokedBy, rig.enrollmentLive(), rig.renewerLog())
			}
			code, stdout, stderr = rig.machine.run(rig.useSecret("0s")...)
			if code != 0 || strings.TrimSpace(stdout) != strconv.Itoa(len(renewalSecretValue)) {
				t.Fatalf("the next use: exit %d, stdout %q, stderr %q; want the value from the live grant", code, stdout, stderr)
			}
			if requests := rig.requests(); len(requests) != 1 {
				t.Fatalf("the enrollment holds requests %+v; want the one the person approved, its grant reused", requests)
			}
			rig.assertRenewalModes()
		})
	}
}

// TestAPendingRequestWaitsAcrossTheSignInsLease: a request for a human-tier secret waits for its
// person, who answers four leases later, and it is still pending when they answer. Rows: a use
// command waits for it and then runs with the value; the same, with its renewer killed while the
// command waits, which the waiting command starts again; and a `request` that left it pending with
// no command running after it. Before the renewer, the sweep cancelled the request one lease after
// the enrollment was made, "the requesting enrollment ended: its lease lapsed", while the command
// still waited.
func TestAPendingRequestWaitsAcrossTheSignInsLease(t *testing.T) {
	for _, row := range []struct {
		name          string
		waits, killed bool
	}{
		{name: "the use command waits", waits: true},
		{name: "renewer killed while the use command waits", waits: true, killed: true},
		{name: "request left pending", waits: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newRenewalRig(t)
			var cmd *exec.Cmd
			var stdout, stderr bytes.Buffer
			var waited sync.WaitGroup
			if row.waits {
				cmd = rig.machine.command(rig.useSecret("1m")...)
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waited.Add(1)
				go func() { defer waited.Done(); _ = cmd.Wait() }()
				t.Cleanup(func() { _ = cmd.Process.Kill(); waited.Wait() })
			} else if code, stdout, stderr := rig.machine.run("request", "TEST_SECRET", "--reason", "check the sign-in renewal", "--ttl", "1m"); code != exitPending {
				t.Fatalf("request: exit %d, stdout %q, stderr %q; want %d", code, stdout, stderr, exitPending)
			}
			pending := testwait.UntilPolling(t, 100*time.Millisecond, "the request to wait for its person", func() (requestRow, bool) {
				requests := rig.requests()
				return firstOr(requests), len(requests) == 1 && requests[0].State == "pending"
			})
			killed := 0
			if row.killed {
				killed = rig.killRenewer()
			}

			rig.waitLeases(4)

			if pids := rig.renewerPIDs(); row.killed && (len(pids) != 1 || pids[0] == killed) {
				t.Fatalf("four leases after renewer %d was killed, renewers %v; want one the waiting command started again (renewer log %q)", killed, pids, rig.renewerLog())
			}
			if requests := rig.requests(); len(requests) != 1 || requests[0].State != "pending" || !rig.enrollmentLive() {
				t.Fatalf("after four leases: the enrollment holds requests %+v and is live %v; want the one request still pending (renewer log %q)", requests, rig.enrollmentLive(), rig.renewerLog())
			}
			rig.broker.Decide(t, pending.RecordID, true)
			if !row.waits {
				if _, _, live := rig.grantOf(pending.ID); !live {
					t.Fatalf("request %s made no live grant once its person approved it", pending.ID)
				}
				return
			}
			waited.Wait()
			if code := cmd.ProcessState.ExitCode(); code != 0 || strings.TrimSpace(stdout.String()) != strconv.Itoa(len(renewalSecretValue)) {
				t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want the value once its person approved", code, stdout.String(), stderr.String())
			}
		})
	}
}

// firstOr is the first of rows, or the zero row.
func firstOr(rows []requestRow) requestRow {
	if len(rows) == 0 {
		return requestRow{}
	}
	return rows[0]
}

// TestASignInsRenewerStopsWhenNothingNeedsTheEnrollment: the renewer keeps the enrollment only
// while the machine needs it, so a renewed enrollment still ends, each way R3 names. Rows: no
// command for a lease with no request and no grant that has an end left; a request that waited and
// was then cancelled; a sign-in taken off the machine, and one whose record names no enrollment,
// though a grant is still live; and an enrollment the broker refuses for good. In each the renewer
// stops and the lease lapses.
func TestASignInsRenewerStopsWhenNothingNeedsTheEnrollment(t *testing.T) {
	// use runs the agent-tier TEST_AUTO_SECRET, granted at once, for ttl, and waits for the
	// renewer it started.
	use := func(t *testing.T, rig *renewalRig, ttl string) time.Time {
		t.Helper()
		code, stdout, stderr := rig.machine.run("TEST_AUTO_SECRET", "--ttl", ttl, "--wait", "0s", "--", "true")
		if code != 0 {
			t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
		}
		done := time.Now()
		rig.waitFor("a renewer of the sign-in", func() bool { return len(rig.renewerPIDs()) == 1 })
		return done
	}
	// ends waits for the renewer to stop and the lease to lapse, and fails t unless the renewer's
	// log says why with want.
	ends := func(t *testing.T, rig *renewalRig, want string) {
		t.Helper()
		rig.waitFor("the sign-in's renewer to stop", func() bool { return len(rig.renewerPIDs()) == 0 })
		rig.waitFor("the enrollment's lease to lapse", func() bool { rig.sweep(); return !rig.enrollmentLive() })
		if log := rig.renewerLog(); !strings.Contains(log, want) {
			t.Fatalf("the renewer log %q; want the line that says %q", log, want)
		}
	}
	t.Run("idle", func(t *testing.T) {
		rig := newRenewalRig(t)
		// A grant of two seconds: once it ends and a lease has passed since the command, nothing
		// needs the enrollment.
		done := use(t, rig, "2s")
		rig.waitLeases(1)
		if !rig.enrollmentLive() {
			t.Fatalf("one lease after the command, the enrollment ended; want it renewed while the command's use is recent (renewer log %q)", rig.renewerLog())
		}
		ends(t, rig, "stopped: nothing has used it")
		// The renewer renews only while the command's use is less than a lease old, so the lease
		// its last renewal got lapses less than two leases after the command. The slack is a third
		// of a lease, one cycle of the renewer: under a whole-module test run a loaded machine has
		// delayed a cycle, the renewals and this wait's polls by more than half a second.
		if took := time.Since(done); took > 2*renewalLease+renewalLease/3 {
			t.Fatalf("the idle enrollment ended %s after the command, want two leases at most, %s, and a cycle (renewer log %q)", took, 2*renewalLease, rig.renewerLog())
		}
	})
	t.Run("request cancelled", func(t *testing.T) {
		rig := newRenewalRig(t)
		code, stdout, stderr := rig.machine.run("request", "TEST_SECRET", "--reason", "check the sign-in renewal", "--ttl", "1m", "--json")
		if code != exitPending {
			t.Fatalf("request: exit %d, stdout %q, stderr %q; want %d", code, stdout, stderr, exitPending)
		}
		var asked RequestResult
		if err := json.Unmarshal([]byte(stdout), &asked); err != nil {
			t.Fatal(err)
		}
		rig.waitLeases(3)
		if len(rig.renewerPIDs()) != 1 || !rig.enrollmentLive() {
			t.Fatalf("three leases after the request: renewers %v, enrollment live %v; want the pending request to keep it (renewer log %q)", rig.renewerPIDs(), rig.enrollmentLive(), rig.renewerLog())
		}
		if code, stdout, stderr := rig.machine.run("cancel", asked.RequestID); code != 0 {
			t.Fatalf("cancel: exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
		}
		ends(t, rig, "stopped: nothing has used it")
		if entries, err := os.ReadDir(filepath.Join(rig.renewalDir(), "pending")); err != nil || len(entries) != 0 {
			t.Fatalf("the pending directory holds %v (%v); want the cancelled request's file removed", entries, err)
		}
	})
	t.Run("record removed", func(t *testing.T) {
		rig := newRenewalRig(t)
		use(t, rig, "1m")
		if err := os.Remove(cipherclient.SignIns(rig.machine.getenv).Path(rig.broker.Operator)); err != nil {
			t.Fatal(err)
		}
		ends(t, rig, "stopped: the sign-in is no longer on this machine")
	})
	t.Run("no enrollment", func(t *testing.T) {
		rig := newRenewalRig(t)
		use(t, rig, "1m")
		rig.rewriteRecord(func(record *cipherclient.SignInRecord) { record.EnrollmentID = "" })
		ends(t, rig, "stopped: the sign-in holds no enrollment")
	})
	t.Run("refused", func(t *testing.T) {
		rig := newRenewalRig(t)
		use(t, rig, "1m")
		// As a person's revoke of the sign-in ends its enrollment.
		if _, err := rig.broker.Store.Pool.Exec(context.Background(), `update enrollments set revoked_at = now() where id = $1`, rig.enrollmentID); err != nil {
			t.Fatal(err)
		}
		ends(t, rig, "permanently refused by the broker AGENT_SECRETS_URL names: HTTP 401 ENROLLMENT_NOT_LIVE")
	})
}

// TestARenewerFollowsTheEnrollmentTheRecordNames: a command that found the enrollment ended
// registers the next one and writes it to the record. The renewer already running renews that one
// from its next renewal on: while a command uses the sign-in each lease, the same renewer, by its
// pid, keeps the new enrollment live, and the commands start no other.
func TestARenewerFollowsTheEnrollmentTheRecordNames(t *testing.T) {
	rig := newRenewalRig(t)
	code, stdout, stderr := rig.machine.run("TEST_AUTO_SECRET", "--ttl", "1m", "--wait", "0s", "--", "true")
	if code != 0 {
		t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
	}
	var first []int
	rig.waitFor("a renewer of the sign-in", func() bool { first = rig.renewerPIDs(); return len(first) == 1 })
	// The next enrollment of the same box key under the same sign-in, as registerSignInSession
	// makes one once the broker ended the first.
	next := uuid.NewString()
	if _, err := rig.broker.Store.Pool.Exec(context.Background(), `with ended as (update enrollments set revoked_at = now() where id = $1 returning *)
		insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		select $2::text::uuid, kind, 'session-' || $2::text, operator, thumbprint, launcher_credential_id, now() + make_interval(secs => $3) from ended`,
		rig.enrollmentID, next, renewalLease.Seconds()); err != nil {
		t.Fatal(err)
	}
	rig.rewriteRecord(func(record *cipherclient.SignInRecord) { record.EnrollmentID = next })
	rig.enrollmentID = next

	for range 6 {
		if code, stdout, stderr := rig.machine.run("self"); code != 0 {
			t.Fatalf("self: exit %d, stdout %q, stderr %q; want the new enrollment live", code, stdout, stderr)
		}
		time.Sleep(renewalLease / 2)
		rig.sweep()
		if pids := rig.renewerPIDs(); len(pids) != 1 || pids[0] != first[0] || !rig.enrollmentLive() {
			t.Fatalf("after the record named enrollment %s: it is live %v with renewers %v; want it renewed by renewer %d (renewer log %q)", next, rig.enrollmentLive(), pids, first[0], rig.renewerLog())
		}
	}
}

// TestACommandThatCannotKeepTheSignInRenewedSaysSoAndGoesOn: where the renewal directory cannot
// be made, the command still runs, and one stderr line says that the sign-in's grants and pending
// requests will end with its lease.
func TestACommandThatCannotKeepTheSignInRenewedSaysSoAndGoesOn(t *testing.T) {
	rig := newRenewalRig(t)
	// A file where the renewal directory goes.
	if err := os.WriteFile(rig.renewalDir(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := rig.machine.run("TEST_AUTO_SECRET", "--ttl", "1m", "--wait", "0s", "--", "sh", "-c", "echo ${#TEST_AUTO_SECRET}")
	if code != 0 || strings.TrimSpace(stdout) != strconv.Itoa(len("test-auto-secret-v1")) {
		t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want the value", code, stdout, stderr)
	}
	want := "agent-secrets: " + rig.broker.Operator + "'s sign-in enrollment is not kept renewed ("
	tail := "), so its grants and its requests waiting for a person end when its lease lapses\n"
	if strings.Count(stderr, want) != 1 || !strings.Contains(stderr, tail) {
		t.Fatalf("stderr %q; want one line %q…%q", stderr, want, tail)
	}
	if pids := rig.renewerPIDs(); len(pids) != 0 {
		t.Fatalf("renewers %v started; want none", pids)
	}
}

// TestCommandsAtOnceKeepOneRenewer: a busy machine runs eight commands of one sign-in at once,
// every half lease for three leases. The first eight leave exactly one renewer, every later round
// finds it and starts none, so the same renewer runs to the end, and the enrollment lives on
// through it.
func TestCommandsAtOnceKeepOneRenewer(t *testing.T) {
	rig := newRenewalRig(t)
	round := func() {
		t.Helper()
		var started sync.WaitGroup
		failures := make(chan string, 8)
		for range 8 {
			started.Add(1)
			go func() {
				defer started.Done()
				cmd := rig.machine.command("self")
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				if err := cmd.Run(); err != nil {
					failures <- fmt.Sprintf("self: %v: %s", err, out.String())
				}
			}()
		}
		started.Wait()
		close(failures)
		for failure := range failures {
			t.Fatal(failure)
		}
	}
	round()
	var first []int
	rig.waitFor("one renewer of the sign-in", func() bool { first = rig.renewerPIDs(); return len(first) == 1 })
	rig.assertSessionLeader(first[0])
	for end := time.Now().Add(3 * renewalLease); time.Now().Before(end); {
		time.Sleep(renewalLease / 2)
		rig.sweep()
		round()
		if pids := rig.renewerPIDs(); len(pids) != 1 || pids[0] != first[0] || !rig.enrollmentLive() {
			t.Fatalf("after a round of eight commands: renewers %v, enrollment live %v; want the one renewer %d and a live enrollment (renewer log %q)",
				pids, rig.enrollmentLive(), first[0], rig.renewerLog())
		}
	}
}
