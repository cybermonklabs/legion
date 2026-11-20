package supervise

// The ends of a claim that records a session and no file that are charged nothing, and the log
// lines the production check of dispatch://INFRA-4108 reads (the plan's "Log lines the production
// check reads"). At the deploy, a claim caught in the loop runs a fresh agent an earlier release
// spawned, which registers as another session on that release's plugin contract: the refusal
// stands, and its end is charged nothing (the roll edge, spec R1 and the architect's answer D2).

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The lines a production check reads, each by the words it is matched on: the launched line
// whole, and each other line by its prefix, the words a check's query names.
const (
	launchedLine         = "supervise: launched"
	goneForgetLine       = "supervise: the recorded session is found nowhere"
	notResumedForgetLine = "supervise: the pod's launcher cannot look the recorded session up by its id"
	unchargedEndLine     = "supervise: a start that had Oh My Pi resume the recorded session by its id did not register"
	rollEdgeLine         = "supervise: refused the registration of a process an earlier release started"
)

// prefixed is every record whose message begins with prefix, in the order they were written: a
// line a check reads by its first words, which name it, where the words after them may still be
// settled.
func (l *logRecords) prefixed(prefix string) []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []loggedRecord
	for _, r := range l.kept {
		if strings.HasPrefix(r.message, prefix) {
			matched = append(matched, r)
		}
	}
	return matched
}

// carries is whether one of rec's attributes, the logger's own included, has the value value,
// whatever its key.
func (rec loggedRecord) carries(value string) bool {
	for _, a := range rec.attrs {
		if a.Value.String() == value {
			return true
		}
	}
	return false
}

// wantNamesTheLaunch is that rec names session and the launch's generation.
func wantNamesTheLaunch(t *testing.T, rec loggedRecord, session string, generation uint64) {
	t.Helper()
	if !rec.carries(session) {
		t.Errorf("the line %q carries %v; want it to name session %s", rec.message, rec.attrs, session)
	}
	if g, ok := rec.attr("generation"); !ok || g.String() != fmt.Sprint(generation) {
		t.Errorf("the line %q carries generation %v (present %t); want %d", rec.message, g, ok, generation)
	}
}

// The roll edge: a claim that records a session and no file carries two of its three launch
// failures, and its running process, which this daemon adopted at its boot, was spawned by an
// earlier release. That process registers as another session on the earlier release's older plugin
// contract. The fence still refuses it with the same-agent refusal (409), and writes the one
// roll-edge line, which a production check reads because no launched line of this daemon run comes
// before the refusal. The process's end — its death, or its registration deadline — is charged
// nothing, so the claim does not fail, and the next launch takes one of R1's two ways: it resumes
// the session by its id where the runtime finds it, or forgets it once where it does not. Every
// other refused registration keeps its charge: a process of this release adopted at a restart,
// which registers at the daemon's contract, and this daemon run's own resume by id that comes back
// as another session. The row of this release's adopted process is today's behaviour whole.
func TestARefusedRegistrationOfAClaimWithNoFileIsChargedNothing(t *testing.T) {
	const failures = 2
	limit := func(h *harness) { h.deps.Limits.LaunchFailures = 3 }
	adopted := func() Claim {
		c := byIDClaim(StateLaunching, 3)
		c.Budgets.LaunchFailures = failures
		return c
	}
	// refuse is the registration of otherSession on contract, which the fence refuses.
	refuse := func(r *byIDRig, contract int) {
		r.t.Helper()
		r.connect()
		if err := r.registerAs(otherSession, otherSessionFile, contract); !errors.Is(err, claim.SameAgentRefusal) {
			r.t.Fatalf("the registration as %s on contract %d = %v; want the same-agent refusal, 409", otherSession, contract, err)
		}
		if c := r.claim(); c.Session != idSession || c.SessionFile != "" {
			r.t.Fatalf("the refused registration left the claim recording %q and %q; want %s and no file", c.Session, c.SessionFile, idSession)
		}
	}
	wantRollEdgeLines := func(r *byIDRig, want int) {
		r.t.Helper()
		if lines := r.records.prefixed(rollEdgeLine); len(lines) != want {
			r.t.Errorf("the refusal wrote %d lines starting %q; want %d", len(lines), rollEdgeLine, want)
		}
	}
	// wantUncharged is the end of the refused process charged nothing: the claim keeps its two
	// launch failures and has not failed.
	wantUncharged := func(r *byIDRig) {
		r.t.Helper()
		r.wantBudgets(Budgets{LaunchFailures: failures})
		r.wantNoFailure()
	}
	// wantCharged is the end charged as today: the third launch failure, which fails the claim.
	wantCharged := func(r *byIDRig) {
		r.t.Helper()
		r.wantState(StateFailed)
		r.wantBudgets(Budgets{LaunchFailures: 3})
	}

	t.Run("an earlier release's process dies, and the next launch finds the session", func(t *testing.T) {
		r := newByIDRig(t, adopted(), limit)
		refuse(r, r.olderContract())
		wantRollEdgeLines(r, 1)
		from := len(r.rt.Calls())

		r.observe(runtime.Gone)

		wantUncharged(r)
		r.wantFirstStartResumesByID(from)
		r.connect()
		if err := r.registerAs(idSession, idSessionFile, r.deps.PluginContract); err != nil {
			t.Fatalf("the resumed agent's registration as %s = %v; want it accepted", idSession, err)
		}
		r.wantState(StateRegistered)
		wantUncharged(r)
		r.wantNothingLost()
	})

	t.Run("an earlier release's process dies, and the session is found nowhere", func(t *testing.T) {
		r := newByIDRig(t, adopted(), limit)
		r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
		refuse(r, r.olderContract())
		wantRollEdgeLines(r, 1)
		from := len(r.rt.Calls())

		r.observe(runtime.Gone)

		wantUncharged(r)
		r.wantNoSpawnWhileRecorded()
		if starts := r.starts(from); len(starts) != 2 || !resumedByID(starts[0]) || starts[1].Method != "Spawn" {
			t.Fatalf("the next launch made %s; want a Resume of %s by its id, then one Spawn", startsText(starts), idSession)
		}
		r.wantLost(SessionLossGone)
		if c := r.claim(); c.Abandoned.Session != idSession || c.Session != "" {
			t.Errorf("the claim abandoned %q and records %q; want %s abandoned and no session", c.Abandoned.Session, c.Session, idSession)
		}
		r.connect()
		if err := r.registerAs(newSession, newSessionFile, r.deps.PluginContract); err != nil {
			t.Fatalf("the fresh agent's registration as %s = %v; want it accepted", newSession, err)
		}
		wantUncharged(r)
		r.wantLost(SessionLossGone)
	})

	t.Run("an earlier release's process passes its registration deadline", func(t *testing.T) {
		r := newByIDRig(t, adopted(), limit)
		refuse(r, r.olderContract())
		wantRollEdgeLines(r, 1)
		from := len(r.rt.Calls())

		r.advance(deadline)

		wantUncharged(r)
		r.wantFirstStartResumesByID(from)
	})

	t.Run("a process of this release registers as another session and dies", func(t *testing.T) {
		r := newByIDRig(t, adopted(), limit)
		refuse(r, r.deps.PluginContract)
		wantRollEdgeLines(r, 0)

		r.observe(runtime.Gone)

		wantCharged(r)
	})

	t.Run("this daemon run's own resume by id comes back as another session and dies", func(t *testing.T) {
		c := byIDQueued(failures)
		r := newByIDRig(t, c, limit)
		r.must(RequestSpawn{Claim: testToken})
		r.wantFirstStartResumesByID(0)
		refuse(r, r.deps.PluginContract)
		wantRollEdgeLines(r, 0)

		r.observe(runtime.Gone)

		wantCharged(r)
	})
}

// The supervise: launched line keeps `resumed`, true for every resume, and says in `resumedBy` how
// the launch resumed: `file`, the file the claim records; `id`, the session the launcher or the
// tmux runtime found by its id; `agent-lookup`, Oh My Pi's own lookup through a launcher without
// one; or `none`, a fresh spawn. The forget line keeps the prefix a production check reads for a
// session no store holds, and has one of its own for a forget through a launcher without the
// lookup. The end of a start by Oh My Pi's own lookup that did not register writes a line naming the
// session and the generation, and the roll edge's refusal writes a line of its own, naming the
// recorded session, the registering one and the generation. The forget line of a claim that
// records its file is today's.
func TestTheLaunchedLineNamesTheWayTheLaunchResumed(t *testing.T) {
	withFile := byIDClaim(StateQueued, 2)
	withFile.SessionFile = idSessionFile

	t.Run("the launched line", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			claim     Claim
			deps      func(h *harness)
			resumedBy string
			resumed   bool
		}{
			{"a resume of the file the claim records", withFile, nil, "file", true},
			{"a resume of a session the runtime finds by its id", byIDClaim(StateQueued, 2), nil, "id", true},
			{"a resume by Oh My Pi's own lookup, through a launcher without the lookup", byIDClaim(StateQueued, 2), olderLauncher, "agent-lookup", true},
			{"a fresh spawn of a claim that records no session", queuedClaim(), nil, "none", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := newByIDRig(t, tc.claim, tc.deps)

				r.must(RequestSpawn{Claim: testToken})

				launched := r.records.of(launchedLine)
				if len(launched) != 1 {
					t.Fatalf("the launch logged %d %q lines; want one", len(launched), launchedLine)
				}
				if by, ok := launched[0].attr("resumedBy"); !ok || by.String() != tc.resumedBy {
					t.Errorf("the launched line says resumedBy %v (present %t); want %q", by, ok, tc.resumedBy)
				}
				if resumed, ok := launched[0].attr("resumed"); !ok || resumed.Kind() != slog.KindBool || resumed.Bool() != tc.resumed {
					t.Errorf("the launched line says resumed %v (present %t); want %t", resumed, ok, tc.resumed)
				}
			})
		}
	})

	t.Run("the forget lines", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			claim  Claim
			deps   func(h *harness)
			script bool
			line   string
		}{
			{"a session no store holds, for a claim that records its id alone", byIDClaim(StateQueued, 2), nil, true, goneForgetLine},
			{"a session no store holds, for a claim that records its file", withFile, nil, true, goneForgetLine},
			{"a session Oh My Pi did not resume by its id, through a launcher without the lookup", markedClaim(0), olderLauncher, false, notResumedForgetLine},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := newByIDRig(t, tc.claim, tc.deps)
				if tc.script {
					r.rt.ScriptResume(fake.SpawnResult{Err: noSuchSession()})
				}

				r.must(RequestSpawn{Claim: testToken})

				if lines := r.records.prefixed(tc.line); len(lines) != 1 {
					t.Errorf("the forget wrote %d lines starting %q; want one", len(lines), tc.line)
				}
			})
		}
	})

	t.Run("the line of a start by Oh My Pi's own lookup that did not register", func(t *testing.T) {
		r := newByIDRig(t, byIDQueued(0), olderLauncher)
		g := r.startByAgentLookup()

		r.observe(runtime.Gone)

		ends := r.records.prefixed(unchargedEndLine)
		if len(ends) != 1 {
			t.Fatalf("the end wrote %d lines starting %q; want one", len(ends), unchargedEndLine)
		}
		wantNamesTheLaunch(t, ends[0], idSession, g)
	})

	t.Run("the roll edge's line", func(t *testing.T) {
		r := newByIDRig(t, byIDClaim(StateLaunching, 3), nil)
		r.connect()

		if err := r.registerAs(otherSession, otherSessionFile, r.olderContract()); !errors.Is(err, claim.SameAgentRefusal) {
			t.Fatalf("the registration as %s = %v; want the same-agent refusal", otherSession, err)
		}

		lines := r.records.prefixed(rollEdgeLine)
		if len(lines) != 1 {
			t.Fatalf("the refusal wrote %d lines starting %q; want one", len(lines), rollEdgeLine)
		}
		wantNamesTheLaunch(t, lines[0], idSession, 3)
		if !lines[0].carries(otherSession) {
			t.Errorf("the line %q carries %v; want it to name the registering session %s", lines[0].message, lines[0].attrs, otherSession)
		}
	})
}
