package sandbox

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// recordedSessionID is the session id an Oh My Pi claim recorded at its registration, as a
// resume's spec carries it (SpawnSpec.ResumeSessionID): another id than the one resumeSession's
// name gives, so a launch that resumes by it is told apart from one that read the file's name.
const recordedSessionID = "019a2b3c-4d5e-7f60-8a71-92b3c4d5e6f7"

// unnamedSession is a recorded session file whose name gives no session id, `<timestamp>_<id>`
// being the only name that gives one.
const unnamedSession = ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/recorded.jsonl"

// sessionWords are the words of an agent argv that name its session, in the argv's order:
// `--session-id <id>`, `--resume <id>`, and `--resume=<file>` or `--resume=<id>`.
func sessionWords(argv []string) []string {
	var words []string
	for i, word := range argv {
		switch {
		case word == "--session-id" || word == "--resume":
			words = append(words, argv[i:min(i+2, len(argv))]...)
		case strings.HasPrefix(word, "--resume="):
			words = append(words, word)
		}
	}
	return words
}

// resumeAt is spec resumed at generation 2 from file, with the session id the claim recorded
// beside it (id, "" for a claim that recorded none).
func resumeAt(spec runtime.SpawnSpec, file, id string) runtime.SpawnSpec {
	spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.ResumeSessionID = 2, "boot-"+string(spec.Claim)+"-g2", file, id
	return spec
}

// An Oh My Pi role whose pod trades an exchange identity starts under a session id its daemon
// chose before the role started (ompInputs), as a Claude Code role does: a fresh launch's agent
// command carries `--session-id` on a fresh UUIDv7, and the runtime answers that same id for the
// launch's incarnation (LaunchSession), which the daemon registers for the pod at the shim's
// hello. A resumed launch carries `--resume` on the session id its claim recorded
// (SpawnSpec.ResumeSessionID) and no path, while its start command still names the recorded file
// (ResumeFile), by which the launcher looks the session up before it starts the agent.
func TestAnOhMyPiRoleStartsUnderTheIdItsDaemonChose(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	chosen, named := g.r.LaunchSession(loc)
	if !named {
		t.Fatalf("LaunchSession(%s) names nothing for an Oh My Pi launch whose pod trades", loc.Incarnation)
	}
	if id, err := uuid.Parse(chosen); err != nil || id.Version() != 7 {
		t.Errorf("the chosen session %q is not a UUIDv7 (%v)", chosen, err)
	}
	fresh := lastStart(t, g, workerToken)
	if got, want := sessionWords(fresh.Argv), []string{"--session-id", chosen}; !slices.Equal(got, want) {
		t.Errorf("the fresh launch's agent command names its session as %q, want %q", got, want)
	}
	if fresh.ResumeFile != "" {
		t.Errorf("the fresh launch's start command resumes %q, want nothing", fresh.ResumeFile)
	}

	resumed, err := g.r.Resume(g.ctx, &loc, resumeAt(spec, resumeSession, recordedSessionID))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	start := lastStart(t, g, workerToken)
	if got, want := sessionWords(start.Argv), []string{"--resume", recordedSessionID}; !slices.Equal(got, want) {
		t.Errorf("the resumed launch's agent command names its session as %q, want %q", got, want)
	}
	if at := slices.IndexFunc(start.Argv, func(word string) bool { return strings.Contains(word, resumeSession) }); at >= 0 {
		t.Errorf("the resumed launch's argv carries the recorded path in %q, want the session's id alone", start.Argv[at])
	}
	if start.ResumeFile != resumeSession {
		t.Errorf("the resumed launch's start command resumes %q, want the recorded file %s for the launcher's own lookup", start.ResumeFile, resumeSession)
	}
	if session, named := g.r.LaunchSession(resumed); !named || session != recordedSessionID {
		t.Errorf("LaunchSession of the resumed launch = %q, %t; want the id it resumes, %s", session, named, recordedSessionID)
	}
}

// A claim that records its session and no file (dispatch://INFRA-4108) is resumed by that id in a
// pod that trades as in any other: the agent command carries entry 189's one `--resume=<id>` and
// no `--session-id`, through a launcher that looks the id up (resumeSession) and through one that
// has the agent's own command resolve it (ResumeByAgent) alike, and the session the runtime
// answers for the launch, which the daemon registers for the pod, is that id and no fresh one.
func TestATradingLaunchResumesASessionRecordedWithNoFileByItsID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []rigOption
		byAgent bool
		named   string
	}{
		{"through a launcher that looks the id up", []rigOption{withResumesBySession()}, false, resumeSessionID},
		{"through the agent's own lookup", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, tc.options...)
			first := g.spawn(workerSpec(t))
			spec := byIDSpec(t)
			spec.ResumeByAgent = tc.byAgent
			resumed, err := g.r.Resume(g.ctx, &first, spec)
			if err != nil {
				t.Fatalf("Resume of session %s by its id: %v", resumeSessionID, err)
			}
			start := lastStart(t, g, workerToken)
			if got, want := sessionWords(start.Argv), []string{"--resume=" + resumeSessionID}; !slices.Equal(got, want) {
				t.Errorf("the agent command names its session as %q, want %q alone", got, want)
			}
			if start.ResumeSession != tc.named || start.ResumeFile != "" {
				t.Errorf("the start command names session %q and file %q, want session %q and no file", start.ResumeSession, start.ResumeFile, tc.named)
			}
			if err := start.Validate(); err != nil {
				t.Errorf("the launcher refuses the start command: %v", err)
			}
			if session, named := g.r.LaunchSession(resumed); !named || session != resumeSessionID {
				t.Errorf("LaunchSession of the resumed launch = %q, %t; want the recorded session %s", session, named, resumeSessionID)
			}
		})
	}
}

// A claim that records a session id and no file is resumed by that id: a pod that trades starts
// the resume with the one word `--resume=<id>` (agentArgv), and does not refuse it, an id that
// begins with `-` included. Only the two-word `--resume <id>` is refused for an id of a shape
// sessionIDFault names (TestAResumedIdNoParserCanMisreadIsTheOnlyOnePassed), and such a claim
// records no file a refusal could name.
func TestATradingResumeOfARecordedSessionIDIsNeverRefusedForAFileItHasNot(t *testing.T) {
	const id = "-" + resumeSessionID
	spec := byIDSpec(t)
	spec.ResumeSession = id
	g := newRig(t, nil, withResumesBySession())
	first := g.spawn(workerSpec(t))
	resumed, err := g.r.Resume(g.ctx, &first, spec)
	if err != nil {
		t.Fatalf("a pod that trades refuses the resume of the recorded session %q, which it must start as `--resume=%s`: %v", id, id, err)
	}
	if got, want := sessionWords(lastStart(t, g, workerToken).Argv), []string{"--resume=" + id}; !slices.Equal(got, want) {
		t.Errorf("a pod that trades names its session as %q, want %q", got, want)
	}
	if session, named := g.r.LaunchSession(resumed); !named || session != id {
		t.Errorf("LaunchSession of the resumed launch = %q, %t; want the recorded session %s", session, named, id)
	}
}

// A claim that recorded a session file and no session id beside it is resumed by the id its
// file's name gives, `<timestamp>_<id>.jsonl` (the claim's file-id name, supervise.RecordedHolds):
// its Oh My Pi starts with `--resume <id>`, and the start command still names the file. A recorded
// file whose name gives no id is refused, naming the file, before anything of the pod is touched:
// `--resume <id>` takes no path.
func TestAnOldClaimResumesByTheIdInItsFileName(t *testing.T) {
	const id = "0198f3a2-6b7c-7d8e-9f01-23456789abcd"
	file := ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_" + id + ".jsonl"
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	resumed, err := g.r.Resume(g.ctx, &loc, resumeAt(spec, file, ""))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	start := lastStart(t, g, workerToken)
	if got, want := sessionWords(start.Argv), []string{"--resume", id}; !slices.Equal(got, want) {
		t.Errorf("the resumed launch's agent command names its session as %q, want %q", got, want)
	}
	if start.ResumeFile != file {
		t.Errorf("the resumed launch's start command resumes %q, want the recorded file %s", start.ResumeFile, file)
	}
	if session, named := g.r.LaunchSession(resumed); !named || session != id {
		t.Errorf("LaunchSession of the resumed launch = %q, %t; want the id its file's name gives, %s", session, named, id)
	}

	for _, unnamed := range []string{
		unnamedSession,
		ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_.jsonl",
	} {
		next := resumeAt(spec, unnamed, "")
		next.Generation, next.BootToken = 3, "boot-tester-g3"
		if _, err := g.r.Resume(g.ctx, &resumed, next); err == nil || !strings.Contains(err.Error(), unnamed) {
			t.Errorf("Resume from %s, whose name gives no id = %v, want a refusal naming the file", unnamed, err)
		}
		if start := lastStart(t, g, workerToken); start.Generation != 2 {
			t.Errorf("the refused resume from %s sent generation %d's start command, want none sent", unnamed, start.Generation)
		}
	}
}

// A launch that trades and resumes a recorded file passes the id it resumes as a word of its own,
// `--resume <id>`, so an id an argument parser would read as something else — one that begins
// with `-`, or holds white space or a control character — is refused before anything of the pod is
// touched, naming the claim and what is wrong with the id, whether the claim recorded it or its
// file's name gives it. An id of a session id's shape, the control, resumes. A resume of a session
// the claim records with no file passes the one word `--resume=<id>` instead, which no such id is
// refused for (TestATradingResumeOfARecordedSessionIDIsNeverRefusedForAFileItHasNot).
func TestAResumedIdNoParserCanMisreadIsTheOnlyOnePassed(t *testing.T) {
	dir := ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/"
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	for _, tc := range []struct{ name, file, id, fault string }{
		{"a recorded id that is a flag", resumeSession, "--help", "begins with -"},
		{"a recorded id of a lone dash", resumeSession, "-", "begins with -"},
		{"a recorded id holding a space", resumeSession, "ses tester", "holds white space"},
		{"a recorded id holding a tab", resumeSession, "ses\ttester", "holds white space"},
		{"a recorded id holding a line separator", resumeSession, "ses\u2028tester", "holds white space"},
		{"a recorded id holding an escape", resumeSession, "ses\x1b[2Jtester", "holds a control character"},
		{"a recorded id holding a NUL", resumeSession, "ses\x00tester", "holds a control character"},
		{"the id of a file's name that is a flag", dir + "2026-09-23T12-00-00-000Z_--version.jsonl", "", "begins with -"},
	} {
		_, err := g.r.Resume(g.ctx, &loc, resumeAt(spec, tc.file, tc.id))
		if err == nil || !strings.Contains(err.Error(), string(workerToken)) || !strings.Contains(err.Error(), tc.fault) {
			t.Errorf("%s: Resume = %v, want a refusal naming %s and that the id %s", tc.name, err, workerToken, tc.fault)
		}
		if start := lastStart(t, g, workerToken); start.Generation != 1 {
			t.Errorf("%s: the refused resume sent generation %d's start command, want none sent", tc.name, start.Generation)
		}
	}
	if _, err := g.r.Resume(g.ctx, &loc, resumeAt(spec, resumeSession, recordedSessionID)); err != nil {
		t.Fatalf("Resume by the id %s, of a session id's shape: %v", recordedSessionID, err)
	}
	if got, want := sessionWords(lastStart(t, g, workerToken).Argv), []string{"--resume", recordedSessionID}; !slices.Equal(got, want) {
		t.Errorf("the resume by a well-formed id names its session as %q, want %q", got, want)
	}
}

// A launch into a kept pod built before the exchange identity, which carries no exchange token and
// so trades nothing (keptPod.trades), waits for that pod's update round whatever its Oh My Pi would
// have started with. listenerCredential refuses it with runtime.ErrAwaitingPodRound naming the
// pod, since the Envoy listener takes no shared bearer and the role would hold no credential for
// it, before the launch's argv is settled (ompResumeRefusal) and before anything of the pod is
// touched. That holds for a resume on the session id its claim recorded; for a resume whose id no
// launch that trades can pass — no id at all, or one that reads as a flag — which the same resume
// in a pod that trades, the control, is refused for naming what it lacks rather than for the
// round; and for a role started fresh in that pod. No start reaches the pod and the runtime
// records no session for any of them.
func TestAnOhMyPiLaunchIntoAKeptPodWithoutTheExchangeIdentityWaitsForItsRound(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	g.builtWithout(loc.Sandbox.Name, exchangeIdentityVolume)
	r2 := restartedOver(t, g, loc, func(*Options) {})
	awaiting := func(what string, err error) {
		t.Helper()
		var refusal *runtime.AwaitingPodRound
		if !errors.As(err, &refusal) || refusal.Pod.Sandbox == nil || refusal.Pod.Sandbox.PodUID != loc.Sandbox.PodUID ||
			!strings.Contains(err.Error(), "the Envoy listener, which takes no shared bearer") {
			t.Errorf("%s = %v, want listenerCredential's runtime.AwaitingPodRound naming the kept pod %s", what, err, loc.Sandbox.PodUID)
		}
	}
	before := starts(g)
	_, err := r2.Resume(g.ctx, &loc, resumeAt(spec, resumeSession, recordedSessionID))
	awaiting("Resume on the recorded session id", err)

	trading := newRig(t, nil)
	tradingLoc := trading.spawn(workerSpec(t))
	for i, tc := range []struct{ name, file, id, refusal string }{
		{"a claim that recorded no id, whose file's name gives none", unnamedSession, "", unnamedSession},
		{"a claim whose recorded id reads as a flag", resumeSession, "--help", "begins with -"},
	} {
		next := resumeAt(spec, tc.file, tc.id)
		next.Generation, next.BootToken = uint64(3+i), fmt.Sprint("boot-tester-g", 3+i)
		_, err := r2.Resume(g.ctx, &loc, next)
		awaiting(tc.name+": Resume into the kept pod", err)
		if _, err := trading.r.Resume(trading.ctx, &tradingLoc, resumeAt(workerSpec(t), tc.file, tc.id)); err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: the same resume in a pod that trades = %v, want a refusal naming %q", tc.name, err, tc.refusal)
		}
	}

	reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
	g.launcher(otherToken)
	_, err = r2.Spawn(g.ctx, reviewer)
	awaiting("Spawn the reviewer into the kept pod", err)
	if after := starts(g); after != before {
		t.Errorf("the tester's launcher was sent %d start commands by the refused launches, want none", after-before)
	}
	for _, frame := range g.sent(otherToken) {
		if start, ok := frame.(shimwire.LauncherStart); ok {
			t.Errorf("the role started fresh in the kept pod was sent generation %d's start command, naming its session as %q, want none", start.Generation, sessionWords(start.Argv))
		}
	}
	if recorded := launchSessions(r2); len(recorded) != 0 {
		t.Errorf("the runtime recorded %+v for launches into the kept pod, want nothing", recorded)
	}
	if pod := g.pod(loc.Sandbox.Name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
		t.Errorf("the pod after the refusals is %v, want %s still running", pod, loc.Sandbox.PodUID)
	}
}
