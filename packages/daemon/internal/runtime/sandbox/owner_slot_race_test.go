package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldLaunch starts a launch of the owner, at the generation after its first, into the pod it
// keeps, and returns once that launch's start has reached the owner's launcher, which holds it
// unanswered until hold closes (rig.holdStart), with the launch's own error channel. ctx is the
// launch's own context, which a caller cancels to cut the launch short.
func heldLaunch(t *testing.T, g *rig, ctx context.Context, hold chan struct{}) <-chan error {
	t.Helper()
	g.commandsMu.Lock()
	g.holdStart = hold
	g.commandsMu.Unlock()
	again := rootSpec(t)
	again.Generation++
	launched := make(chan error, 1)
	go func() { _, err := g.r.Spawn(ctx, again); launched <- err }()
	testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the owner's launch's start to reach its launcher", func() bool {
		return slices.ContainsFunc(g.sent(rootToken), func(frame shimwire.Frame) bool {
			start, ok := frame.(shimwire.LauncherStart)
			return ok && start.Generation == again.Generation
		})
	})
	return launched
}

// ownerRig is a rig whose ActsAs says the owner acts as the role acting holds, or as itself while it
// holds none, with the owner launched once and stored, and its slot's Secret holding a lease the
// daemon has since re-minted, so its next launch has a write to make. held reads the token the
// owner slot's default hosts.yml holds.
func ownerRig(t *testing.T, acting *atomic.Value) (g *rig, held func() string) {
	t.Helper()
	g = newRig(t, nil, actingOwner(acting))
	g.spawn(rootSpec(t))
	storeOwner(g)
	slot := roleSecretName(SandboxName(rootToken), claim.RoleArchitect)
	editSecret(g, slot, func(s *corev1.Secret) { s.Data[GitHubHostsKey("sjawhar")] = []byte(ghconfig.Hosts("ghs_stale_lease")) })
	return g, func() string {
		t.Helper()
		return slotToken(t, g, SandboxName(rootToken), claim.RoleArchitect)
	}
}

// A launch of the owner into the pod it keeps writes the owner slot's Secret for the App it judges
// then, the architect's own, and its start waits for that token. While the start waits, the
// owner's step starts and the outbox writes the slot for the step's App (WriteSlotCredential). No
// write of the slot lands while a launch into it is in flight (markSlotLaunch, refreshUpdate), so
// the step's write waits, for the launch's own end and for no clock, and the slot keeps the token
// the start names. Once the start is answered the step's write is made, and it returns only with
// the step's App in the slot: the moment the outbox tells the owner its step (ownerSlotHolds), so
// a notice told then finds the step's App. The control is the same launch with no step starting,
// which leaves the architect's own App in the slot.
func TestALaunchOvertakenByAStepsWriteLeavesTheSlotOnTheStepsApp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		starts bool
	}{
		{name: "control: no step starts while the launch's start waits"},
		{name: "the owner's step starts while the launch's start waits", starts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var acting atomic.Value
			g, held := ownerRig(t, &acting)
			hold := make(chan struct{})
			launched := heldLaunch(t, g, g.ctx, hold)
			own := staticCredentialToken(claim.RoleArchitect)
			sum := sha256.Sum256([]byte(own))
			if start := lastStart(t, g, rootToken); start.GitHubCredential == nil || start.GitHubCredential.TokenDigest != hex.EncodeToString(sum[:]) {
				t.Fatalf("the owner's start waits for %+v, want the digest of %q, its own App's token: the case's premise", start.GitHubCredential, own)
			}
			wrote := make(chan error, 1)
			if tc.starts {
				acting.Store(claim.RoleImplementer)
				go func() { wrote <- g.r.WriteSlotCredential(g.ctx, rootToken) }()
				select {
				case err := <-wrote:
					t.Fatalf("the step's write of the owner's slot returned (%v) while the launch's start still waits: a notice told now finds %q in the slot",
						err, held())
				case <-time.After(300 * time.Millisecond):
				}
			}
			if got := held(); got != own {
				t.Fatalf("while the launch's start waits the owner's slot holds %q, want %q, the token the start names", got, own)
			}
			g.commandsMu.Lock()
			g.holdStart = nil
			g.commandsMu.Unlock()
			close(hold)
			if err := <-launched; err != nil {
				t.Fatalf("the owner's launch into the kept pod: %v", err)
			}
			want := own
			if tc.starts {
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("the step's write of the owner's slot once the launch ended = %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the step's write of the owner's slot did not return once the launch's start was answered")
				}
				want = staticCredentialToken(claim.RoleImplementer)
			}
			if got := held(); got != want {
				t.Fatalf("once the step's write returned the owner's slot holds %q, want %q", got, want)
			}
		})
	}
}

// A step's write of the owner's slot that a launch in flight refused waits for that launch's own
// end, however the launch ends: a start the launcher refuses, and a launch whose context ends
// while its start waits, each unmark the slot (relaunch's deferred unmark), and the write is then
// made, with the step's App in the slot, so a pending write never waits for ever.
func TestAStepsWriteTheFenceHeldIsMadeOnceTheLaunchEndsHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cut  bool
	}{
		{name: "the launcher refuses the start"},
		{name: "the launch's context ends while its start waits", cut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var acting atomic.Value
			g, held := ownerRig(t, &acting)
			hold := make(chan struct{})
			released := false
			t.Cleanup(func() {
				g.commandsMu.Lock()
				g.holdStart = nil
				g.commandsMu.Unlock()
				if !released {
					close(hold)
				}
			})
			ctx, cut := context.WithCancel(g.ctx)
			defer cut()
			launched := heldLaunch(t, g, ctx, hold)
			acting.Store(claim.RoleImplementer)
			wrote := make(chan error, 1)
			go func() { wrote <- g.r.WriteSlotCredential(g.ctx, rootToken) }()
			select {
			case err := <-wrote:
				t.Fatalf("the step's write of the owner's slot returned (%v) while the launch's start still waits", err)
			case <-time.After(300 * time.Millisecond):
			}
			if tc.cut {
				cut()
			} else {
				g.commandsMu.Lock()
				g.failStart = "the launcher refuses this start"
				g.holdStart = nil
				g.commandsMu.Unlock()
				close(hold)
				released = true
			}
			select {
			case err := <-launched:
				if err == nil {
					t.Fatal("the owner's launch succeeded, want it refused or cut short: the case's premise")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the owner's launch did not end")
			}
			select {
			case err := <-wrote:
				if err != nil {
					t.Fatalf("the step's write of the owner's slot once the launch ended = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the step's write of the owner's slot still waits after the launch ended")
			}
			if got, want := held(), staticCredentialToken(claim.RoleImplementer); got != want {
				t.Fatalf("once the step's write returned the owner's slot holds %q, want %q", got, want)
			}
		})
	}
}

// retroSteps is the plan rows of task a written as words (`role` or `role:key`), each of its
// built-in role's type.
func retroSteps(words ...string) []record.PlanStep {
	roles := plan.BuiltinRoles()
	steps := make([]record.PlanStep, 0, len(words))
	for i, word := range words {
		role, key, _ := strings.Cut(word, ":")
		steps = append(steps, record.PlanStep{Task: "a", Position: i + 1, Role: role, Type: roles[role].Type, AgentKey: key})
	}
	return steps
}

// The owner's slot holds the App of the role the owner's act is done as, and every write of the
// owner's slot judges that on the Secret it writes: the rig's ActsAs is the daemon's rule of the
// owner's place (record.OwnerActsAs) over the issue as the record would read it and the task's plan
// rows. While the issue stands at the reviewer's step of `implementer:owner reviewer` the owner's
// slot holds the review App, its own. Through the owner's retro of its own implementer step it
// holds the implement App: the outbox's write puts the implement App in the slot's Secret; a
// refresh tick in the retro leaves it there, and so does a launch of the owner into the pod it
// keeps, whose start frame waits for the implement App's token; once the retro ended and the issue
// stands in merging, the write-back puts the review App back. A merge forward asked of the owner
// holds the implement App by the same runtime path, its answer the daemon's ActsAs (internal/daemon's
// ownerSlotRole), so it has no case of its own here.
//
// The controls: at the retro of `acceptance:owner` alone, which the owner does as itself, and at the
// retro of `implementer reviewer`, which its implementer does, the same write, tick and launch
// leave the owner's slot on the review App.
func TestTheOwnersRetroHoldsTheImplementApp(t *testing.T) {
	implement, review := staticCredentialToken(claim.RoleImplementer), staticCredentialToken(claim.RoleArchitect)
	reviewing := record.Issue{Key: testTree, Phase: phase.Reviewing, PlaceRole: claim.RoleReviewer, StepRole: claim.RoleReviewer}
	for _, tc := range []struct {
		name  string
		steps []record.PlanStep
		// window is the issue as the record reads it through the window the test writes the slot in.
		window record.Issue
		want   string
	}{
		{name: "the owner's retro of implementer:owner reviewer", steps: retroSteps("implementer:owner", "reviewer"),
			window: record.Issue{Key: testTree, Phase: phase.Retro, PlaceRole: claim.RoleArchitect}, want: implement},
		{name: "control: acceptance:owner alone", steps: retroSteps("acceptance:owner"),
			window: record.Issue{Key: testTree, Phase: phase.Retro, PlaceRole: claim.RoleArchitect}, want: review},
		{name: "control: the retro of a fresh implementer", steps: retroSteps("implementer", "reviewer"),
			window: record.Issue{Key: testTree, Phase: phase.Retro, PlaceRole: claim.RoleImplementer}, want: review},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var issue atomic.Pointer[record.Issue]
			issue.Store(&reviewing)
			g := newRig(t, nil, withOptions(func(o *Options) {
				o.ActsAs = func(_ context.Context, token claim.Token) (claim.Role, bool, error) {
					if token != rootToken {
						return "", false, nil
					}
					role, acting := record.OwnerActsAs(*issue.Load(), tc.steps)
					return role, acting, nil
				}
			}))
			g.spawn(rootSpec(t))
			storeOwner(g)
			sandbox := SandboxName(rootToken)
			before := g.pod(sandbox).UID
			if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != review {
				t.Fatalf("the owner's slot holds %q at the reviewer's step, want %q", got, review)
			}

			window := tc.window
			issue.Store(&window)
			if err := g.r.WriteSlotCredential(g.ctx, rootToken); err != nil {
				t.Fatalf("WriteSlotCredential of the owner's slot in the window = %v", err)
			}
			if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != tc.want {
				t.Fatalf("the owner's slot holds %q in the window once written, want %q", got, tc.want)
			}
			g.r.refreshGitHubCredentials(g.ctx)
			if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != tc.want {
				t.Fatalf("the owner's slot holds %q after a refresh tick in the window, want %q", got, tc.want)
			}
			again := rootSpec(t)
			again.Generation++
			g.spawn(again)
			if after := g.pod(sandbox).UID; after != before {
				t.Fatalf("the owner's launch in the window replaced the pod (%s, then %s), want it kept", before, after)
			}
			if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != tc.want {
				t.Fatalf("the owner's slot holds %q after its launch in the window, want %q", got, tc.want)
			}
			sum := sha256.Sum256([]byte(tc.want))
			if start := lastStart(t, g, rootToken); start.GitHubCredential == nil || start.GitHubCredential.TokenDigest != hex.EncodeToString(sum[:]) {
				t.Fatalf("the owner's start frame in the window waits for the credential %+v, want the digest of %q", start.GitHubCredential, tc.want)
			}

			issue.Store(&record.Issue{Key: testTree, Phase: phase.Merging})
			if err := g.r.WriteSlotCredential(g.ctx, rootToken); err != nil {
				t.Fatalf("the write-back of the owner's slot = %v", err)
			}
			if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != review {
				t.Fatalf("the owner's slot holds %q after the write-back in merging, want %q", got, review)
			}
		})
	}
}
