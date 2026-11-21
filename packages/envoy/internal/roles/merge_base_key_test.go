package roles_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// claimRig is a NATS server, the bus client Buckets reads through, and the registries a listener
// writes claims and sessions through.
func claimRig(t *testing.T) (*bus.Client, *store.Registry, *session.SessionRegistry) {
	t.Helper()
	_, url := testnats.Start(t)
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, url)
	registry, err := store.Open(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn, session.WithSessionTTL(2*time.Minute))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	return client, registry, sessions
}

// askedKeys records the claim key each RoleClaim was asked for, as contracts.RoleClaimKey names
// it, and answers through the store it wraps.
type askedKeys struct {
	roles.ClaimStore
	keys []string
}

func (a *askedKeys) RoleClaim(ctx context.Context, project, role string) (store.RoleClaim, error) {
	a.keys = append(a.keys, contracts.RoleClaimKey(project, role))
	return a.ClaimStore.RoleClaim(ctx, project, role)
}

// c4KeyedEarly is the control's decider: it reads every role under project.role, the key a
// project-keyed claim takes after C4, before C4, so a bare role's merge-base claim is not there.
type c4KeyedEarly struct{ roles.ClaimStore }

func (c c4KeyedEarly) RoleClaim(ctx context.Context, project, role string) (store.RoleClaim, error) {
	return c.ClaimStore.RoleClaim(ctx, contracts.NoProject, project+"."+role)
}

// TestBothDecidersReadTheMergeBaseKeyBeforeC4: until C4 every caller resolves a role under its own
// project (contracts.RoleProject: no project for a free-form role, the Legion role's own project
// for a Legion role), and the key both deciders read is then the merge base's, the role itself:
// the listener's claim store and Dispatch's roles.Buckets both read contracts.RoleClaimKey, so a
// claim the merge base's writer (store.Registry.SetRole) wrote at the role's key resolves to its
// holder through ResolveHolder over Buckets, in both modes, and the key asked is the role. Control:
// a decider reading the project-keyed name before C4 misses every free-form role's claim, so the
// row fails for the key it names.
func TestBothDecidersReadTheMergeBaseKeyBeforeC4(t *testing.T) {
	client, registry, sessions := claimRig(t)
	if err := sessions.Put("ses_holder", session.SessionEntry{MachineID: "test-machine", Projects: []string{}, Restricted: new(false)}); err != nil {
		t.Fatalf("register the holder: %v", err)
	}
	for _, role := range []string{"reviewer", "lead-reviewer", "legion-priv-priv-1-architect", "legion-infra-infra-2-implementer"} {
		if _, err := registry.SetRole("ses_holder", "test-machine", role, false); err != nil {
			t.Fatalf("the merge base's writer claims %s: %v", role, err)
		}
		project := contracts.RoleProject(role)
		if key := contracts.RoleClaimKey(project, role); key != role {
			t.Errorf("%s resolved under its own project %q reads key %q, want the merge base's %q", role, project, key, role)
		}
		for _, mode := range []roles.Mode{roles.AllPublic, roles.Required} {
			decider := roles.NewBuckets(client, roles.Windows{}, func(context.Context, string) (bool, error) { return false, nil }, mode)
			asked := &askedKeys{ClaimStore: decider}
			resolved, err := roles.ResolveHolder(context.Background(), asked, decider, project, role)
			if err != nil || resolved.Holder != "ses_holder" || resolved.State != roles.Live {
				t.Errorf("mode %d: %s resolved to %+v, %v; want the merge base's claim, ses_holder live", mode, role, resolved, err)
			}
			if len(asked.keys) == 0 || asked.keys[0] != role {
				t.Errorf("mode %d: %s's resolution asked keys %v, want %q first", mode, role, asked.keys, role)
			}
			if _, free := contracts.ParseRoleClaimKey(role); project == contracts.NoProject && free == role {
				early, err := roles.ResolveHolder(context.Background(), c4KeyedEarly{decider}, decider, project, role)
				if err != nil || early.State != roles.Unclaimed {
					t.Errorf("control, mode %d: a decider reading %s's project-keyed name before C4 resolved %+v, %v; want no holder", mode, role, early, err)
				}
			}
		}
	}
}
