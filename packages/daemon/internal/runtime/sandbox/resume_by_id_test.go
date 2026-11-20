package sandbox

// A claim can record the session its agent registered and no file of it (dispatch://INFRA-4108).
// Its launch resumes the session by its id: through a pod launcher that has said in its hello that
// it can look a session up by its id, the start names the session to that launcher, which finds
// its file in the session table; through any other, the runtime names no session and no file to
// the launcher, and Oh My Pi's own `--resume=<id>` resolves it.

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// resumeSessionID is the id of the session a pod's PI_SESSION_FILE named, captured in the
// planner's pod on 2026-11-19: what a claim that records its session and no file holds.
const resumeSessionID = "01a1f5de-4835-7667-b005-e99e3e17edd3"

// startsSent are the start commands token's fake launcher received after the first from commands
// it was sent.
func startsSent(g *rig, token claim.Token, from int) []shimwire.LauncherStart {
	var starts []shimwire.LauncherStart
	for _, frame := range g.sent(token)[from:] {
		if start, ok := frame.(shimwire.LauncherStart); ok {
			starts = append(starts, start)
		}
	}
	return starts
}

// resumesIn are the elements of argv that name a session to resume.
func resumesIn(argv []string) []string {
	var resumes []string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--resume") {
			resumes = append(resumes, arg)
		}
	}
	return resumes
}

// byIDSpec is the tester's claim resumed at generation 2 on the session it recorded by its id
// alone, with no file of it.
func byIDSpec(t *testing.T) runtime.SpawnSpec {
	spec := workerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSession = 2, "boot-tester-g2", resumeSessionID
	return spec
}

// Through a launcher that has said in its hello it can look a session up by its id, a Resume of a
// claim that records its session and no file sends one start, which names the session to the
// launcher (resumeSession) and no file, and whose agent command carries `--resume=<id>` once, the
// element the launcher replaces with the file it finds (internal/launcher's start). The launcher's
// answer that its session table holds no such session is runtime.ErrSessionFileMissing, as its
// answer for a file the table does not hold is.
func TestAResumeByIDNamesTheSessionToALauncherThatCanLookItUp(t *testing.T) {
	t.Run("a session the launcher finds", func(t *testing.T) {
		g := newRig(t, nil, withResumesBySession())
		first := g.spawn(workerSpec(t))
		before := len(g.sent(workerToken))
		if _, err := g.r.Resume(g.ctx, &first, byIDSpec(t)); err != nil {
			t.Fatalf("Resume of session %s by its id: %v", resumeSessionID, err)
		}
		starts := startsSent(g, workerToken, before)
		if len(starts) != 1 {
			t.Fatalf("the Resume sent %d start commands, want one", len(starts))
		}
		start := starts[0]
		if start.Generation != 2 || start.ResumeSession != resumeSessionID || start.ResumeFile != "" {
			t.Errorf("the start command is generation %d resuming session %q and file %q, want generation 2 resuming session %s and no file",
				start.Generation, start.ResumeSession, start.ResumeFile, resumeSessionID)
		}
		if resumes := resumesIn(start.Argv); !slices.Equal(resumes, []string{"--resume=" + resumeSessionID}) {
			t.Errorf("the agent command resumes %q, want --resume=%s once", resumes, resumeSessionID)
		}
		if err := start.Validate(); err != nil {
			t.Errorf("the launcher refuses the start command: %v", err)
		}
	})

	t.Run("a session the launcher's table does not hold", func(t *testing.T) {
		g := newRig(t, nil, withResumesBySession())
		first := g.spawn(workerSpec(t))
		refusing := g.scriptedStartLauncher(workerToken, 1, func(start shimwire.LauncherStart) *shimwire.LauncherStartResult {
			return &shimwire.LauncherStartResult{ID: start.ID, Error: "resume session " + start.ResumeSession + ": the session table holds no such session",
				Session: shimwire.SessionGone}
		})
		if _, err := g.r.Resume(g.ctx, &first, byIDSpec(t)); !errors.Is(err, runtime.ErrSessionFileMissing) {
			t.Errorf("Resume of session %s, which the launcher's table does not hold = %v, want runtime.ErrSessionFileMissing", resumeSessionID, err)
		}
		if start := refusing.start(t, 2); start.ResumeSession != resumeSessionID {
			t.Errorf("the start command resumes session %q, want %s", start.ResumeSession, resumeSessionID)
		}
	})
}

// Through a launcher that has not said in its hello it can look a session up by its id, which
// would decode a start naming one and look nothing up, a Resume by the id alone sends nothing: no
// start, and no stop of the generation still running, and it answers
// runtime.ErrSessionLookupUnavailable naming the claim, its Sandbox and the pod. The Resume the
// supervisor makes next, in the same generation, has the agent's own command resolve the id
// (ResumeByAgent): it sends one start that names no session and no file to the launcher, whose
// agent command carries `--resume=<id>` once.
func TestAResumeByIDThroughALauncherWithoutTheLookupStartsOhMyPiWithTheSessionID(t *testing.T) {
	g := newRig(t, nil)
	first := g.spawn(workerSpec(t))
	before := len(g.sent(workerToken))
	spec := byIDSpec(t)
	_, err := g.r.Resume(g.ctx, &first, spec)
	if !errors.Is(err, runtime.ErrSessionLookupUnavailable) {
		t.Fatalf("Resume of session %s by its id through a launcher that cannot look it up = %v, want runtime.ErrSessionLookupUnavailable",
			resumeSessionID, err)
	}
	for what, named := range map[string]string{
		"claim": string(workerToken), "Sandbox": "sandbox " + first.Sandbox.Name, "pod": first.Sandbox.PodUID,
	} {
		if !strings.Contains(strings.ToLower(err.Error()), named) {
			t.Errorf("the refusal %q does not name its %s, %s", err, what, named)
		}
	}
	if sent := g.sent(workerToken)[before:]; len(sent) != 0 {
		t.Fatalf("the refused Resume sent its launcher %#v, want nothing: no generation starts, and generation 1 runs on", sent)
	}

	spec.ResumeByAgent = true
	if _, err := g.r.Resume(g.ctx, &first, spec); err != nil {
		t.Fatalf("Resume of session %s by the agent's own lookup: %v", resumeSessionID, err)
	}
	starts := startsSent(g, workerToken, before)
	if len(starts) != 1 {
		t.Fatalf("the two Resumes sent %d start commands, want one", len(starts))
	}
	start := starts[0]
	if start.Generation != 2 || start.ResumeSession != "" || start.ResumeFile != "" {
		t.Errorf("the start command is generation %d resuming session %q and file %q, want generation 2 naming neither",
			start.Generation, start.ResumeSession, start.ResumeFile)
	}
	if resumes := resumesIn(start.Argv); !slices.Equal(resumes, []string{"--resume=" + resumeSessionID}) {
		t.Errorf("the agent command resumes %q, want --resume=%s once", resumes, resumeSessionID)
	}
}

// A fresh agent names no session (Spawn): a spec naming one by its id, or asking the agent's own
// command to resolve one, is refused before anything is touched, as one naming a session file is.
// A Resume names its session by exactly one of its file and its id, and has the agent resolve the
// id only where it names one: one asking the agent to resolve an id it does not name, or naming
// both a file and an id, is refused before anything is touched too. Nothing touched is no request
// to the cluster, no command to the launcher, and generation 1 still the process the runtime
// records.
func TestSandboxSpawnRefusesAResumeSession(t *testing.T) {
	spawn := func(g *rig, _ runtime.Locator, spec runtime.SpawnSpec) error {
		_, err := g.r.Spawn(g.ctx, spec)
		return err
	}
	resume := func(g *rig, prev runtime.Locator, spec runtime.SpawnSpec) error {
		_, err := g.r.Resume(g.ctx, &prev, spec)
		return err
	}
	for name, tc := range map[string]struct {
		change func(*runtime.SpawnSpec)
		launch func(*rig, runtime.Locator, runtime.SpawnSpec) error
	}{
		"Spawn naming a session by its id":            {func(s *runtime.SpawnSpec) { s.ResumeSession = resumeSessionID }, spawn},
		"Spawn asking the agent to resolve a session": {func(s *runtime.SpawnSpec) { s.ResumeByAgent = true }, spawn},
		"Resume asking the agent to resolve an id it does not name": {
			func(s *runtime.SpawnSpec) { s.ResumeSessionFile, s.ResumeByAgent = resumeSession, true }, resume,
		},
		"Resume naming both a file and an id": {
			func(s *runtime.SpawnSpec) { s.ResumeSessionFile, s.ResumeSession = resumeSession, resumeSessionID }, resume,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			first := g.spawn(workerSpec(t))
			spec := workerSpec(t)
			spec.Generation, spec.BootToken = 2, "boot-tester-g2"
			tc.change(&spec)
			before := len(g.sent(workerToken))
			g.clearActions()
			if err := tc.launch(g, first, spec); err == nil {
				t.Fatalf("the %s was not refused: it started generation 2", name)
			}
			if requests := g.requests(); len(requests) != 0 {
				t.Errorf("the refused launch made the requests %v, want none", requests)
			}
			if sent := g.sent(workerToken)[before:]; len(sent) != 0 {
				t.Errorf("the refused launch sent its launcher %#v, want nothing", sent)
			}
			if recorded, found := g.r.recorded(workerToken); !found || recorded.Incarnation != first.Incarnation {
				t.Errorf("after the refusal the runtime records %q (found %t), want generation 1's %s", recorded.Incarnation, found, first.Incarnation)
			}
		})
	}
}

// A Claude Code claim never resumes by a session id alone: the session it records is its
// transcript's path, which prepare reads the id from (claudelaunch.ResumeSession), and supervise
// forgets a Claude Code session recorded with no file rather than look it up. A Claude Code spec
// naming its session by its id is refused where the launch is built, whoever was to resolve the
// id, so no Claude Code command is started on a session it would not find.
func TestSandboxPrepareRefusesAClaudeCodeResumeByID(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, byAgent := range map[string]bool{"by the launcher's lookup": false, "by the agent's own lookup": true} {
		t.Run(name, func(t *testing.T) {
			spec := claudeWorkerSpec(t)
			spec.Generation, spec.BootToken, spec.ResumeSession, spec.ResumeByAgent = 2, "boot-g2", claudeTranscriptID, byAgent
			if _, err := r.prepare(spec); err == nil {
				t.Errorf("prepare of a Claude Code claim resuming session %s by its id was not refused", claudeTranscriptID)
			}
		})
	}
}
