package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// checkingLauncher is a fake launcher of one slot whose hello says it waits for a start's
// credential and, where it checks, answers a check of its slot's gh files: from hosts, the token its
// hosts.yml holds now, which the test changes as the kubelet's projection would. It records each
// frame it was sent.
type checkingLauncher struct {
	hosts atomic.Value
	mu    sync.Mutex
	sent  []shimwire.Frame
}

// connectChecking connects a checkingLauncher of token's slot from its issue's pod in place of the
// rig's launcher, holding hosts, and waits until the runtime holds the connection ready.
func connectChecking(t *testing.T, g *rig, token claim.Token, checks bool, hosts string) *checkingLauncher {
	t.Helper()
	l := &checkingLauncher{}
	l.hosts.Store(hosts)
	server, client := net.Pipe()
	pod := g.pod(SandboxName(token))
	session := g.r.launchers.accept(token, launcherIssue(token), g.keptSince(token), shimwire.LauncherHello{
		LauncherID: "checking-" + string(token), PodUID: string(pod.UID), CredentialWait: true, CredentialCheck: checks,
	})
	t.Cleanup(func() { _ = client.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	go func() {
		writer := shimwire.NewWriter(client)
		_ = writer.WriteFrame(shimwire.LauncherState{})
		reader := bufio.NewReader(client)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				return
			}
			l.mu.Lock()
			l.sent = append(l.sent, frame)
			l.mu.Unlock()
			if check, ok := frame.(shimwire.LauncherCredentialCheck); ok {
				sum := sha256.Sum256([]byte(l.hosts.Load().(string)))
				_ = writer.WriteFrame(shimwire.LauncherCredentialCheckResult{ID: check.ID, OK: hex.EncodeToString(sum[:]) == check.TokenDigest})
			}
		}
	}()
	testwait.Eventually(t, "the checking launcher of "+string(token)+" is ready", func() bool {
		_, ready := g.r.launchers.state(token, string(pod.UID))
		return ready
	})
	return l
}

// checks is every credential check the launcher was sent.
func (l *checkingLauncher) checks() []shimwire.LauncherCredentialCheck {
	l.mu.Lock()
	defer l.mu.Unlock()
	var checks []shimwire.LauncherCredentialCheck
	for _, frame := range l.sent {
		if check, ok := frame.(shimwire.LauncherCredentialCheck); ok {
			checks = append(checks, check)
		}
	}
	return checks
}

// slotToken is the token the hosts.yml of the default owner of slot's Secret in the pod of
// sandbox holds.
func slotToken(t *testing.T, g *rig, sandbox string, slot claim.Role) string {
	t.Helper()
	secret := g.secret(roleSecretName(sandbox, slot))
	if secret == nil {
		t.Fatalf("the Sandbox %s has no Secret of the slot %s", sandbox, slot)
	}
	token, err := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(string(secret.Data[GitHubDefaultKey]))])
	if err != nil {
		t.Fatalf("the %s Secret's hosts.yml: %v", slot, err)
	}
	return token
}

// actingOwner gives the rig's runtime an ActsAs that answers, for the rig's root architect, the role
// acting holds ("" for none: its owner does no step).
func actingOwner(acting *atomic.Value) rigOption {
	acting.Store(claim.Role(""))
	return withOptions(func(o *Options) {
		o.ActsAs = func(_ context.Context, token claim.Token) (claim.Role, bool, error) {
			if role := acting.Load().(claim.Role); token == rootToken && role != "" {
				return role, true, nil
			}
			return "", false, nil
		}
	})
}

// storeOwner stores the rig's root architect's claim, as the supervisor stores it, so a write of
// its slot judges the role of the claim in it (judgeSlot).
func storeOwner(g *rig) {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	g.store.claims = append(g.store.claims, supervise.Claim{Token: rootToken, Project: testProject, Tree: testTree, Issue: testTree, Role: claim.RoleArchitect})
}

// A runtime writes the architect's slot for the App of the step its owner does, and says when the
// slot's gh files hold it: while the runtime's ActsAs gives the architect the implementer,
// WriteSlotCredential writes the implement App's token into the slot's Secret; while the slot's
// launcher reads the review App's token in its hosts.yml, SlotHoldsCredential answers a wait,
// having asked the launcher once for the default owner's hosts.yml and the digest of the implement
// App's token; once the launcher reads the implement App's, it answers nil. Once ActsAs gives the
// architect no other role, as when its step ended and no other act of the owner's runs, the write
// puts the review App's back. A launcher whose hello says it answers no check is sent
// none: the write still happens, and the answer is runtime.ErrAwaitsPodUpdate, which only the pod's
// update round or its release ends.
func TestASlotSaysWhenItsFilesHoldTheCredential(t *testing.T) {
	var acting atomic.Value
	g := newRig(t, nil, actingOwner(&acting))
	g.spawn(rootSpec(t))
	storeOwner(g)
	sandbox := SandboxName(rootToken)
	implement, review := staticCredentialToken(claim.RoleImplementer), staticCredentialToken(claim.RoleArchitect)
	launcher := connectChecking(t, g, rootToken, true, review)

	acting.Store(claim.RoleImplementer)
	if err := g.r.WriteSlotCredential(g.ctx, rootToken); err != nil {
		t.Fatalf("WriteSlotCredential(the architect) while it does the implementer step = %v", err)
	}
	if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != implement {
		t.Fatalf("the architect's Secret holds %q after the write, want %q", got, implement)
	}

	err := g.r.SlotHoldsCredential(g.ctx, rootToken, claim.RoleImplementer)
	if !errors.Is(err, runtime.ErrSlotCredentialPending) || !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("SlotHoldsCredential while the slot's hosts.yml holds the review App's token = %v, want a wait that is runtime.ErrSlotCredentialPending", err)
	}
	sum := sha256.Sum256([]byte(implement))
	checks := launcher.checks()
	if len(checks) != 1 || checks[0].HostsFile != "sjawhar/"+ghconfig.HostsFile || checks[0].TokenDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("the launcher was sent the checks %+v, want one of sjawhar/hosts.yml and the digest of %q", checks, implement)
	}

	launcher.hosts.Store(implement)
	if err := g.r.SlotHoldsCredential(g.ctx, rootToken, claim.RoleImplementer); err != nil {
		t.Fatalf("SlotHoldsCredential once the slot's hosts.yml holds the implement App's token = %v, want nil", err)
	}

	before := connectChecking(t, g, rootToken, false, implement)
	acting.Store(claim.Role(""))
	if err := g.r.WriteSlotCredential(g.ctx, rootToken); err != nil {
		t.Fatalf("WriteSlotCredential(the architect) once its step ended = %v", err)
	}
	if got := slotToken(t, g, sandbox, claim.RoleArchitect); got != review {
		t.Fatalf("the architect's Secret holds %q after the write to a launcher that answers no check, want %q", got, review)
	}
	err = g.r.SlotHoldsCredential(g.ctx, rootToken, claim.RoleArchitect)
	if !errors.Is(err, runtime.ErrAwaitsPodUpdate) || !strings.Contains(err.Error(), string(rootToken)) {
		t.Fatalf("SlotHoldsCredential of a launcher that answers no check = %v, want runtime.ErrAwaitsPodUpdate naming %s", err, rootToken)
	}
	if checks := before.checks(); len(checks) != 0 {
		t.Fatalf("a launcher that answers no check was sent %+v", checks)
	}
}

// The App a slot is written for on request is the App of the step's role on the slot's own issue:
// on the rig's issue, whose plan defines coder of the type implementation and prober of the type
// testing, WriteSlotCredential of the architect while it does coder writes the implement App's
// lease and while it does prober the review App's, read on the issue the slot's Secret records.
func TestASlotCredentialIsTheAppOfTheStepsRoleOnItsIssue(t *testing.T) {
	var acting atomic.Value
	g := newRig(t, nil, withProjectTokensOf(refusingOwnerTokens{refuse: new(atomic.Bool)}, planOfTheRigsIssue), actingOwner(&acting))
	g.spawn(rootSpec(t))
	storeOwner(g)
	sandbox := SandboxName(rootToken)
	for _, tc := range []struct {
		role   claim.Role
		prefix string
	}{
		{"coder", "ghs_implement_"},
		{"prober", "ghs_review_"},
		{"coder", "ghs_implement_"},
	} {
		acting.Store(tc.role)
		if err := g.r.WriteSlotCredential(g.ctx, rootToken); err != nil {
			t.Fatalf("WriteSlotCredential(the architect) while it does %s = %v", tc.role, err)
		}
		if got := slotToken(t, g, sandbox, claim.RoleArchitect); !strings.HasPrefix(got, tc.prefix) {
			t.Fatalf("the architect's Secret holds %q after the write while it does %s, want a token that begins %s", got, tc.role, tc.prefix)
		}
	}
}
