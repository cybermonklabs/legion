package sandbox

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
	t.Parallel()
	g := newRig(t, nil, exchangeKeyed(), withWaitsToTheDeadline())
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
	t.Parallel()
	for _, tc := range []struct {
		name    string
		options []rigOption
		byAgent bool
		named   string
	}{
		{"through a launcher that looks the id up", []rigOption{exchangeKeyed(), withResumesBySession(), withWaitsToTheDeadline()}, false, resumeSessionID},
		{"through the agent's own lookup", []rigOption{exchangeKeyed(), withWaitsToTheDeadline()}, true, ""},
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

// A claim that records a session id and no file is resumed by that id: a pod that trades and one
// that trades nothing both start the resume with the one word `--resume=<id>` (agentArgv), and
// neither refuses it, an id that begins with `-` included. Only the two-word `--resume <id>` is
// refused for an id of a shape sessionIDFault names
// (TestAResumedIdNoParserCanMisreadIsTheOnlyOnePassed), and such a claim records no file a
// refusal could name.
func TestATradingResumeOfARecordedSessionIDIsNeverRefusedForAFileItHasNot(t *testing.T) {
	t.Parallel()
	const id = "-" + resumeSessionID
	untraded := newRig(t, nil, withResumesBySession(), withWaitsToTheDeadline())
	first := untraded.spawn(workerSpec(t))
	spec := byIDSpec(t)
	spec.ResumeSession = id
	if _, err := untraded.r.Resume(untraded.ctx, &first, spec); err != nil {
		t.Fatalf("the control: a pod that trades nothing refuses the resume of session %q: %v", id, err)
	}
	if got, want := sessionWords(lastStart(t, untraded, workerToken).Argv), []string{"--resume=" + id}; !slices.Equal(got, want) {
		t.Fatalf("the control: a pod that trades nothing names its session as %q, want %q", got, want)
	}

	g := newRig(t, nil, exchangeKeyed(), withResumesBySession(), withWaitsToTheDeadline())
	first = g.spawn(workerSpec(t))
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
	t.Parallel()
	const id = "0198f3a2-6b7c-7d8e-9f01-23456789abcd"
	file := ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_" + id + ".jsonl"
	g := newRig(t, nil, exchangeKeyed(), withWaitsToTheDeadline())
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
	t.Parallel()
	dir := ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/"
	g := newRig(t, nil, exchangeKeyed(), withWaitsToTheDeadline())
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

// With exchange_audience unset — every deployment before it, with no agent_secrets block or with
// one that names no audience — no Oh My Pi launch names a session id: each agent command is the one
// it was before the runtime chose ids, `--resume=<file>` on the recorded session when it resumes
// and no session word when it starts one, a resume a launch that trades is refused (a file whose
// name gives no id and no recorded id, or a recorded id that reads as a flag) resumes by its file
// as it always did, the runtime answers no session for any launch, and every golden of
// manifestCases without the key is byte for byte as it was. A Claude Code launch's command is
// claudelaunch's, which named its session before this change too, so it is passed over.
func TestNothingChangesWithoutTheKey(t *testing.T) {
	t.Parallel()
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pinned := manifestCases(t)
	cases := maps.Clone(pinned)
	cases["resume-unnamed"] = manifestCase{spec: resumeAt(workerSpec(t), unnamedSession, "")}
	cases["resume-flag"] = manifestCase{spec: resumeAt(workerSpec(t), resumeSession, "--help")}
	for name, tc := range cases {
		if tc.agentSecrets != nil && tc.agentSecrets.ExchangeAudience != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			if _, golden := pinned[name]; golden {
				checkGolden(t, name, manifestOf(t, r, tc.spec))
			}
			l := launchOf(t, r, tc.spec)
			if l.runsClaude() {
				return
			}
			var session []string
			if tc.spec.ResumeSessionFile != "" {
				session = []string{"--resume=" + tc.spec.ResumeSessionFile}
			}
			want := slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default"},
				session, []string{"--mode", "rpc", "--append-system-prompt", systemPromptPath(tc.spec.Generation)})
			if got := l.agentArgv(r.agent); !slices.Equal(got, want) {
				t.Errorf("the agent command is %q, want %q", got, want)
			}
			if start := launcherCommand(l, r); start.ResumeFile != tc.spec.ResumeSessionFile {
				t.Errorf("the start command resumes %q, want %q", start.ResumeFile, tc.spec.ResumeSessionFile)
			}
		})
	}
	for name, option := range map[string]rigOption{
		"no agent_secrets block":         withOptions(func(*Options) {}),
		"a block that names no audience": withOptions(enrolledWith(keptBrokerURL)),
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, option, withWaitsToTheDeadline())
			spec := workerSpec(t)
			loc := g.spawn(spec)
			if words := sessionWords(lastStart(t, g, workerToken).Argv); len(words) != 0 {
				t.Errorf("the fresh launch's agent command names its session as %q, want no session word", words)
			}
			if session, named := g.r.LaunchSession(loc); named {
				t.Errorf("LaunchSession(%s) = %q, want none", loc.Incarnation, session)
			}
			for i, resume := range []struct{ file, id string }{{unnamedSession, ""}, {resumeSession, "--help"}} {
				next := resumeAt(spec, resume.file, resume.id)
				next.Generation, next.BootToken = uint64(2+i), fmt.Sprint("boot-tester-g", 2+i)
				resumed, err := g.r.Resume(g.ctx, &loc, next)
				if err != nil {
					t.Fatalf("Resume from %s with the recorded id %q: %v, want its resume by the file", resume.file, resume.id, err)
				}
				if got, want := sessionWords(lastStart(t, g, workerToken).Argv), []string{"--resume=" + resume.file}; !slices.Equal(got, want) {
					t.Errorf("the resume from %s with the recorded id %q names its session as %q, want %q", resume.file, resume.id, got, want)
				}
				loc = resumed
			}
			if recorded := launchSessions(g.r); len(recorded) != 0 {
				t.Errorf("the runtime recorded %+v, want nothing", recorded)
			}
		})
	}
}

// With exchange_audience set, a launch into a kept pod built before the exchange identity, which
// carries no exchange token and so trades nothing (keptPod.trades), starts its Oh My Pi role as it
// started before the runtime chose ids: the resumed role with `--resume=<file>` and no session id,
// even where its claim recorded one, and a role started fresh in that pod with no session word at
// all. The runtime records no chosen id for either, so the daemon registers the session the agent
// reports at its registration, as it did before. Such a pod may run the Oh My Pi of an image older
// than `--session-id`, which refuses a flag it does not know. A resume whose id is none a launch
// that trades can pass — no id at all, or one that reads as a flag — needs no id here and resumes
// by its file, where the same resume in a pod that trades, the control, is refused naming what it
// lacks.
func TestAKeptPodWithoutTheExchangeIdentityKeepsItsOhMyPiArgv(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(enrolledWith(keptBrokerURL)), withWaitsToTheDeadline())
	spec := workerSpec(t)
	loc := g.spawn(spec)
	r2 := secondRuntime(t, g, func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
	})()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	kept, err := r2.Resume(g.ctx, &loc, resumeAt(spec, resumeSession, recordedSessionID))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this test is about the pod a launch keeps")
	}
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) > 0 {
		t.Fatalf("the kept pod projects %+v, so it does carry the identity and the case proves nothing", found)
	}
	start := lastStart(t, g, workerToken)
	if got, want := sessionWords(start.Argv), []string{"--resume=" + resumeSession}; !slices.Equal(got, want) {
		t.Errorf("the resumed role in the kept pod names its session as %q, want %q", got, want)
	}
	if start.ResumeFile != resumeSession {
		t.Errorf("the start command resumes %q, want the recorded file %s", start.ResumeFile, resumeSession)
	}
	if session, named := r2.LaunchSession(kept); named {
		t.Errorf("LaunchSession of the resumed role in the kept pod = %q, want none", session)
	}

	trading := newRig(t, nil, exchangeKeyed(), withWaitsToTheDeadline())
	tradingLoc := trading.spawn(workerSpec(t))
	for i, tc := range []struct{ name, file, id, refusal string }{
		{"a claim that recorded no id, whose file's name gives none", unnamedSession, "", unnamedSession},
		{"a claim whose recorded id reads as a flag", resumeSession, "--help", "begins with -"},
	} {
		next := resumeAt(spec, tc.file, tc.id)
		next.Generation, next.BootToken = uint64(3+i), fmt.Sprint("boot-tester-g", 3+i)
		resumed, err := r2.Resume(g.ctx, &kept, next)
		if err != nil {
			t.Fatalf("%s: Resume into the kept pod = %v, want its resume by the file", tc.name, err)
		}
		if got, want := sessionWords(lastStart(t, g, workerToken).Argv), []string{"--resume=" + tc.file}; !slices.Equal(got, want) {
			t.Errorf("%s: the resumed role in the kept pod names its session as %q, want %q", tc.name, got, want)
		}
		kept = resumed
		if _, err := trading.r.Resume(trading.ctx, &tradingLoc, resumeAt(workerSpec(t), tc.file, tc.id)); err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: the same resume in a pod that trades = %v, want a refusal naming %q", tc.name, err, tc.refusal)
		}
	}

	reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
	g.launcher(otherToken)
	fresh, err := r2.Spawn(g.ctx, reviewer)
	if err != nil {
		t.Fatalf("Spawn the reviewer: %v", err)
	}
	if fresh.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("the reviewer's Spawn replaced the pod; this test is about the pod a launch keeps")
	}
	if words := sessionWords(lastStart(t, g, otherToken).Argv); len(words) != 0 {
		t.Errorf("the role started fresh in the kept pod names its session as %q, want no session word", words)
	}
	if recorded := launchSessions(r2); len(recorded) != 0 {
		t.Errorf("the runtime recorded %+v for launches into the kept pod, want nothing", recorded)
	}
}
