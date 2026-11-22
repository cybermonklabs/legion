// packages/envoy/internal/cipher/helper/registry_test.go
//go:build linux

package helper

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEndDrainEndsOnlyTheDrainItWasStartedFor: each drain that marks the registry draining
// starts its own bound (Server.drain), and a bound that passes ends its own drain and no later
// one: an install that drained, undrained and drained again keeps its second drain for the whole
// of its own bound, whatever the first bound's timer does when it fires.
func TestEndDrainEndsOnlyTheDrainItWasStartedFor(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	live, first := reg.Drain()
	if len(live) != 0 || first == 0 || !reg.Draining() {
		t.Fatalf("Drain on an empty registry: %d sessions, drain %d, draining %v", len(live), first, reg.Draining())
	}
	reg.Undrain()
	_, second := reg.Drain()
	if second == first {
		t.Fatalf("two drains share the token %d", first)
	}
	if reg.EndDrain(first) || !reg.Draining() {
		t.Fatal("the first drain's end ended the second drain")
	}
	if !reg.EndDrain(second) || reg.Draining() {
		t.Fatal("the second drain's end left the registry draining")
	}
	if reg.EndDrain(second) {
		t.Fatal("a drain already ended was ended again")
	}
}

func TestRootFindsSelfAndDescendantOnly(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	self := os.Getpid()
	sess, err := newSession(self, 1, "h:1:1", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Add(sess)
	child := sleeper(t)
	if got := reg.Root(self); got != sess {
		t.Fatal("the root itself belongs to its session")
	}
	if got := reg.Root(child.Process.Pid); got != sess {
		t.Fatal("a child of the root belongs to its session")
	}
	if got := reg.Root(1); got != nil {
		t.Fatal("pid 1 belongs to no session")
	}
	if !reg.Remove(sess) {
		t.Fatal("Remove reports the removal")
	}
	if got := reg.Root(child.Process.Pid); got != nil {
		t.Fatal("a removed session matches nothing")
	}
	select {
	case <-sess.stop:
	default:
		t.Fatal("Remove must close stop")
	}
	if reg.Remove(sess) {
		t.Fatal("a second Remove of the same session is a no-op")
	}
	other, _ := newSession(self, 2, "h:1:2", "", nil, nil)
	reg.Add(other)
	if reg.Remove(sess) || reg.Get(self) != other {
		t.Fatal("Remove takes only the very session it is given, never a later one with the same pid")
	}
}

// fakeTree is a parent lookup over the tree it is given (child -> parent), for a registry walk
// that reads no /proc; a pid it does not hold has no parent it can read.
func fakeTree(parents map[int]int) func(int) (int, error) {
	return func(pid int) (int, error) {
		parent, ok := parents[pid]
		if !ok {
			return 0, os.ErrNotExist
		}
		return parent, nil
	}
}

// TestRootIsTheNearestEnclosingSession: with one session's root inside another's tree (records a
// restart re-pins can hold such a pair), a process belongs to the nearest root above it, never
// to the one with the lowest pid, which an ancestor usually has: otherwise a root registered high
// in the tree takes every lower session's sign calls and requests.
func TestRootIsTheNearestEnclosingSession(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	// 100 (outer root) -> 200 (inner root) -> 300; 150 -> 100 only.
	reg.parent = fakeTree(map[int]int{300: 200, 200: 100, 150: 100, 100: 1})
	outer, _ := newSession(100, 1, "h:100:1", "", nil, nil)
	inner, _ := newSession(200, 1, "h:200:1", "", nil, nil)
	reg.Add(outer)
	reg.Add(inner)
	for _, tc := range []struct {
		pid  int
		want *Session
	}{
		{300, inner},
		{200, inner},
		{150, outer},
		{100, outer},
	} {
		if got, err := reg.RootOrUnknown(tc.pid); got != tc.want || err != nil {
			t.Fatalf("RootOrUnknown(%d) = %+v, %v; want the session rooted at %d", tc.pid, got, err, tc.want.PID)
		}
	}
	if got, err := reg.RootOrUnknown(999); got != nil || err == nil {
		t.Fatalf("RootOrUnknown of a pid whose parent cannot be read = %+v, %v; want no session and an error", got, err)
	}
}

// TestAdmitRootRefusesARootAboveALiveSession: a pid that a live session's root descends from is
// refused as a root of its own, under the registry's lock and with nothing built or changed,
// since its session would hold that session's processes too. A sibling of the live roots is
// admitted.
func TestAdmitRootRefusesARootAboveALiveSession(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "sessions.json"))
	// 50 -> 100 (a tmux server, say) -> 201, 202 (live roots) and 300 (in no session).
	reg.parent = fakeTree(map[int]int{201: 100, 202: 100, 300: 100, 100: 50, 50: 1})
	first, _ := newSession(201, 1, "h:201:1", "", nil, nil)
	second, _ := newSession(202, 1, "h:202:1", "", nil, nil)
	reg.Add(first)
	reg.Add(second)
	for _, above := range []int{100, 50} {
		built := false
		adm := reg.AdmitRoot(above, "", func(Record, bool) (*Session, error) {
			built = true
			return newSession(above, 1, "h:above:1", "", nil, nil)
		}, nil)
		if !adm.Covers || adm.Won || built || (adm.Session != first && adm.Session != second) {
			t.Fatalf("AdmitRoot(%d) above two live roots = %+v, built %v; want Covers naming one of them, nothing built", above, adm, built)
		}
		if sessions := reg.List(); len(sessions) != 2 || sessions[0] != first || sessions[1] != second {
			t.Fatalf("a refused admission changed the sessions: %+v", sessions)
		}
	}
	adm := reg.AdmitRoot(300, "", func(Record, bool) (*Session, error) {
		return newSession(300, 1, "h:300:1", "", nil, nil)
	}, nil)
	if !adm.Won || adm.Covers {
		t.Fatalf("AdmitRoot(300), a sibling of the live roots = %+v; want it admitted", adm)
	}
}

func TestSaveAndLoadRecordsCarryNoKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	reg := NewRegistry(path)
	sess, err := newSession(4242, 99, "h:4242:99", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.setEnrolledAt("enr-1", sess.identity().Generation)
	reg.Add(sess)
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json must be 0600: %v %v", info, err)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if len(data) == 0 || strings.Contains(text, "PRIVATE") || strings.Contains(text, "\"d\":") || strings.Contains(text, "key") {
		t.Fatalf("the record must not carry key material: %s", data)
	}
	recs, err := LoadRecords(path)
	// The boot id is on every saved record: it is what makes the pid and the start ticks name
	// one process incarnation across a reboot (Registry.SameBoot).
	want := Record{PID: 4242, StartTicks: 99, BootID: reg.bootID, RuntimeID: "h:4242:99", EnrollmentID: "enr-1"}
	if err != nil || len(recs) != 1 || !reflect.DeepEqual(recs[0], want) {
		t.Fatalf("records: %+v %v, want %+v", recs, err, want)
	}
	if recs, err := LoadRecords(filepath.Join(t.TempDir(), "none.json")); err != nil || recs != nil {
		t.Fatalf("a missing file is no records: %+v %v", recs, err)
	}
}

// TestASaveAfterCloseWritesNothing: once Close returns, no Save writes into the records'
// directory, so the caller that closed the registry can remove that directory while the
// helper's goroutines are still finishing a retire or an enrollment.
func TestASaveAfterCloseWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	reg := NewRegistry(path)
	sess, err := newSession(4242, 99, "h:4242:99", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Add(sess)
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reg.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("a Save after Close: %v; want nil, since it writes nothing", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("a Save after Close wrote into the directory: %v %v", entries, err)
	}
}

func TestStateFollowsEnrollment(t *testing.T) {
	sess, err := newSession(1, 1, "h:1:1", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := sess.snapshot(); st.State != "enrolling" || st.EnrollmentID != "" {
		t.Fatal("a fresh session is enrolling")
	}
	sess.setError("broker 503 DATABASE: postgres unreachable")
	if sess.snapshot().LastError == "" {
		t.Fatal("the last broker error is kept for NOT_ENROLLED replies")
	}
	sess.setEnrolledAt("enr-2", sess.identity().Generation)
	if st := sess.snapshot(); st.State != "enrolled" || st.LastError != "" {
		t.Fatalf("enrolled: %s %q", st.State, st.LastError)
	}
	select {
	case <-sess.ready:
	default:
		t.Fatal("setEnrolled must close ready")
	}
	if id := sess.markLapsed("the broker refused this session's renew (PROOF_INVALID); enrolling again"); id != "enr-2" {
		t.Fatalf("markLapsed returns the lapsed id: %q", id)
	}
	if st := sess.snapshot(); st.State != "enrolling" || st.EnrollmentID != "" {
		t.Fatal("a refused renew puts the session back to enrolling at once")
	}
	if got := sess.snapshot().LastError; got != "the broker refused this session's renew (PROOF_INVALID); enrolling again" {
		t.Fatalf("a lapse records its reason as the last error: %q", got)
	}
	select {
	case <-sess.readyCh():
		t.Fatal("a lapse must reopen ready, so a register --wait waits for the re-enrollment")
	default:
	}
	// The record keeps the lapsed id in its own field until the revoke lands, separately
	// from the live one, so a restart can tell an enrollment the key holds from one it only
	// has to end.
	if got := sess.identity().LapsedID; got != "enr-2" {
		t.Fatalf("the record keeps the lapsed id until its revoke: %q", got)
	}
	if sess.clearLapsedIf("enr-other") {
		t.Fatal("a revoke's clear must leave a lapsed id that is no longer the one it revoked")
	}
	if got := sess.identity().LapsedID; got != "enr-2" {
		t.Fatalf("a clear for another id took the lapsed slot: %q", got)
	}
	if !sess.clearLapsedIf("enr-2") {
		t.Fatal("the revoked lapsed id must clear")
	}
	if got := sess.identity().LapsedID; got != "" {
		t.Fatalf("a revoked lapsed id leaves the record: %q", got)
	}
	sess.setEnrolledAt("enr-3", sess.identity().Generation)
	select {
	case <-sess.readyCh():
	default:
		t.Fatal("the re-enrollment must close the reopened ready")
	}
}
