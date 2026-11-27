// packages/envoy/internal/cipher/helper/projects_test.go
//go:build linux

// A session's Dispatch projects: named at its first register, stated by every enrollment the
// helper makes for it, kept on its record across a restart, and never changed by a later register.
package helper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipherclient"
)

// TestHelperKeepsASessionsProjectsAcrossARestart: a register naming projects enrols the session
// stating them, sorted, and its record keeps them; a helper restarted while the session's process
// lives re-pins it under the same projects, so its next enrollment states them again. A register
// naming other projects for the session is refused, and a reboot that brings the named session
// back under another list gives it a new identity rather than the old one's key and enrollment.
func TestHelperKeepsASessionsProjectsAcrossARestart(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	r.callerPID.Store(int64(os.Getpid()))
	id := testSessionID
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id, Projects: []string{"PUB", "PRIV"}})
	if !resp.OK || resp.State != "enrolled" {
		t.Fatalf("register naming PUB and PRIV: %+v", resp)
	}
	posts, _, _ := r.fake.snapshot()
	i := slices.IndexFunc(posts, func(post map[string]any) bool { return post["runtime_id"] == resp.RuntimeID })
	if i < 0 {
		t.Fatalf("no enrolment for runtime id %q: %+v", resp.RuntimeID, posts)
	}
	if got, ok := posts[i]["projects"].([]any); !ok || len(got) != 2 || got[0] != "PRIV" || got[1] != "PUB" {
		t.Fatalf("the enrolment body states projects %v, want [PRIV PUB]: %+v", posts[i]["projects"], posts[i])
	}

	other := r.call(t, Request{Op: "register", SessionID: &id, Projects: []string{"PUB"}})
	if other.OK || other.Code != CodeBadRequest || !strings.Contains(other.Error, "fixed at its first register") {
		t.Fatalf("a register naming other projects for the session = %+v; want BAD_REQUEST saying the projects are fixed", other)
	}
	// A key that is no project key is where a credential pasted into the wrong place shows up,
	// so the refusal names the rule and never the value.
	const pasted = "hunter2pw-helper-projects"
	malformed := r.call(t, Request{Op: "register", SessionID: &id, Projects: []string{pasted}})
	if malformed.OK || malformed.Code != CodeBadRequest || strings.Contains(malformed.Error, pasted) || !strings.Contains(malformed.Error, "not a list of project keys") {
		t.Fatalf("a register naming a key that is no project key = %+v; want BAD_REQUEST naming the rule and not the key", malformed)
	}
	r.callerPID.Store(0)

	recs, err := LoadRecords(state)
	if err != nil {
		t.Fatal(err)
	}
	k := slices.IndexFunc(recs, func(rec Record) bool { return rec.SessionID == testSessionID })
	if k < 0 || !slices.Equal(recs[k].Projects, []string{"PRIV", "PUB"}) {
		t.Fatalf("the session's record = %+v; want it to keep the projects PRIV PUB", recs)
	}

	// The helper restarts while the session's process lives: the record re-pins it, and the
	// restarted helper's enrollment states the same projects. The first helper stops first, as a
	// restarted one has: its registry writes nothing more to the records file (Close), so its own
	// saves cannot race the records this test writes for the next helper on the file's one
	// temporary name, which failed one run in five with "chmod: no such file or directory".
	r.srv.Registry.Close()
	key := recordNamed(t, state, os.Getpid(), mustTicks(t), testSessionID, resp.EnrollmentID)
	recs, _ = LoadRecords(state)
	for n := range recs {
		recs[n].Projects = []string{"PRIV", "PUB"}
	}
	writeRecords(t, state, recs)
	next := newRig(t, state)
	next.login(t)
	sess := next.srv.Registry.Get(os.Getpid())
	if sess == nil || !slices.Equal(sess.identity().Projects, []string{"PRIV", "PUB"}) || sess.identity().Thumbprint != thumbprintOf(t, key) {
		t.Fatalf("the re-pinned session = %+v; want its key and the projects PRIV PUB", sess)
	}
	waitFor(t, func() bool {
		posts, _, _ := next.fake.snapshot()
		return slices.ContainsFunc(posts, func(post map[string]any) bool {
			got, ok := post["projects"].([]any)
			return post["runtime_id"] == SessionRuntimeID(testSessionID) && ok && len(got) == 2
		})
	})
}

// TestAnAttachToARegisteredSessionKeepsItsProjects: `agent-secrets register` without --exec
// attaches (Attach, and a RootPID naming the caller's launcher), and from inside a session it is
// answered with that session. A session's projects are fixed at its first register on that path
// too: an attach naming other projects, none, or one more is refused, and one naming the same list
// is answered with the session's own runtime id, the control that shows the refusals are the
// projects' doing and not the attach's.
func TestAnAttachToARegisteredSessionKeepsItsProjects(t *testing.T) {
	t.Parallel()
	r := startRig(t, "")
	r.callerPID.Store(int64(os.Getpid()))
	defer r.callerPID.Store(0)
	id := testSessionID
	first := r.call(t, Request{Op: "register", SessionID: &id, Projects: []string{"PUB", "PRIV"}})
	if !first.OK {
		t.Fatalf("first register: %+v", first)
	}
	for _, c := range []struct {
		name     string
		projects []string
		wantOK   bool
	}{
		{"other projects", []string{"PUB"}, false},
		{"no projects", nil, false},
		{"another private project", []string{"CORE", "PRIV", "PUB"}, false},
		{"the same projects (control)", []string{"PRIV", "PUB"}, true},
	} {
		resp := r.call(t, Request{Op: "register", Attach: true, RootPID: os.Getppid(), Projects: c.projects})
		if c.wantOK {
			if !resp.OK || resp.RuntimeID != first.RuntimeID {
				t.Errorf("%s: attach = %+v; want the session's own answer, runtime %s", c.name, resp, first.RuntimeID)
			}
			continue
		}
		if resp.OK || resp.Code != CodeBadRequest || !strings.Contains(resp.Error, "fixed at its first register") {
			t.Errorf("%s: attach = %+v; want BAD_REQUEST saying the projects are fixed", c.name, resp)
		}
	}
}

// TestASessionComingBackUnderOtherProjectsGetsANewIdentity: a named session whose process is
// gone is parked with its projects; coming back naming another list, it does not take the parked
// key or enrollment, whose grants were decided under the old list: it gets a fresh key, and the
// old enrollment is revoked.
func TestASessionComingBackUnderOtherProjectsGetsANewIdentity(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "sessions.json")
	gone := sleeper(t)
	gonePID := gone.Process.Pid
	_ = gone.Process.Kill()
	_, _ = gone.Process.Wait()
	key := recordNamed(t, state, gonePID, 1, testSessionID, "enr-before")
	recs, _ := LoadRecords(state)
	recs[0].Projects = []string{"PRIV"}
	writeRecords(t, state, recs)

	r := startRig(t, state)
	seedEnrollment(r.fake, "enr-before", thumbprintOf(t, key), SessionRuntimeID(testSessionID))
	id := testSessionID
	r.callerPID.Store(int64(os.Getpid()))
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id, Projects: []string{"PUB"}})
	r.callerPID.Store(0)
	if !resp.OK {
		t.Fatalf("register under other projects: %+v", resp)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if sess == nil || sess.identity().Thumbprint == thumbprintOf(t, key) || resp.EnrollmentID == "enr-before" {
		t.Fatalf("the session came back under other projects with its old identity (%+v); want a new key and a new enrollment", resp)
	}
	waitFor(t, func() bool { return slices.Contains(r.revokes(t), "enr-before") })
}

// TestHelperRefusesAProjectElementThatIsNotOneKey: the helper reads a list's elements as they
// are given, each one a project key named once, as the broker's enrollment route reads them. So
// the element "PRIV CORE" is refused, not read as the two projects CORE and PRIV, and so are an
// element with a space before its key and an empty one; register and enroll-session-key both
// refuse each with BAD_REQUEST, and none of them reaches the broker. The refusal says the list
// holds one key in each element, and never that keys are separated by white space, which would
// send the caller to write the very element it refused.
func TestHelperRefusesAProjectElementThatIsNotOneKey(t *testing.T) {
	t.Parallel()
	r := startRig(t, filepath.Join(t.TempDir(), "sessions.json"))
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	said := func(resp Response) bool {
		return strings.Contains(resp.Error, "projects") && strings.Contains(resp.Error, cipherclient.ProjectsListed) && !strings.Contains(resp.Error, cipherclient.ProjectsSpaced)
	}
	for _, projects := range [][]string{{"PRIV CORE"}, {" PRIV"}, {""}, {"PUB", "PRIV PRIV"}, {"PRIV", "PRIV"}} {
		reg := r.call(t, Request{Op: "register", Projects: projects})
		if reg.OK || reg.Code != CodeBadRequest || !said(reg) {
			t.Errorf("register naming the projects %q = %+v; want BAD_REQUEST naming projects, one key in each element", projects, reg)
		}
		key := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "box:1", Thumbprint: "tp", Projects: projects})
		if key.OK || key.Code != CodeBadRequest || !said(key) {
			t.Errorf("enroll-session-key naming the projects %q = %+v; want BAD_REQUEST naming projects, one key in each element", projects, key)
		}
	}
	r.callerPID.Store(0)
	if posts, _, _ := r.fake.snapshot(); len(posts) != 0 {
		t.Fatalf("a refused list reached the broker: %+v", posts)
	}
}

// mustTicks is this process's start ticks, for a record naming the test process as live.
func mustTicks(t *testing.T) uint64 {
	t.Helper()
	ticks, err := StartTicks(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return ticks
}
