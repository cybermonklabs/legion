package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimPodEndVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimPodEndVersion = 20261111091500

// 20261111091500 adds claims.pod_end, the pod end a claim's process was last observed to have
// died of: the kubelet's reason and message, and nothing of the role's own log. The negative
// control is the write of that end itself, which has no column to land in before the migration.
//
// What it must give after it is the one thing the column exists for: the end survives a restart
// of the daemon, because it is read back off the claim. The claim written below is read again
// through a fresh read, which is exactly what a boot's replay does with a failed claim before it
// tells the workflow (workflowRuntime.replayTerminal).
func TestTheClaimsPodEndArrivesWith20261111091500(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimPodEndVersion-1)

	token, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatalf("derive the implementer's token: %v", err)
	}
	seed := `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'failed',
		'deaths with work outstanding ran out', 0, 0, 0, 0)`
	if _, err := store.Pool().Exec(ctx, seed, string(token)); err != nil {
		t.Fatalf("seed the failed claim: %v", err)
	}

	// The negative control: before the migration the end has nowhere to go.
	const write = `update claims set pod_end = 'Evicted: The node was low on resource: ephemeral-storage.' where token = $1`
	if _, err := store.Pool().Exec(ctx, write, string(token)); err == nil || !strings.Contains(err.Error(), "pod_end") {
		t.Fatalf("before the migration, writing the pod end = %v, want the column to be missing", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim's pod end: %v", err)
	}

	// A claim of a daemon that ran before the migration holds the empty string, not null, so
	// every read of it is one branch.
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims after the migration: %v", err)
	}
	var migrated *supervise.Claim
	for i, c := range claims {
		if c.Token == token {
			migrated = &claims[i]
		}
	}
	if migrated == nil {
		t.Fatalf("the migrated claim is gone: %+v", claims)
	}
	if migrated.PodEnd != "" {
		t.Fatalf("the claim written before the migration reads pod end %q, want the empty string", migrated.PodEnd)
	}

	// The end written on the claim is the end read back: that is what makes it survive a restart
	// between the pod's death and the moment the relaunch budget runs out.
	const evicted = "Evicted: The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."
	migrated.PodEnd = evicted
	if err := store.PutClaim(ctx, *migrated); err != nil {
		t.Fatalf("write the claim with its pod end: %v", err)
	}
	again, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims back: %v", err)
	}
	for _, c := range again {
		if c.Token != token {
			continue
		}
		if c.PodEnd != evicted {
			t.Fatalf("the claim read back holds pod end %q, want %q", c.PodEnd, evicted)
		}
		if c.Failure != "deaths with work outstanding ran out" {
			t.Fatalf("the claim read back holds failure %q, want the sentence the supervisor wrote beside the pod end", c.Failure)
		}
		return
	}
	t.Fatalf("the claim is gone after its pod end was written: %+v", again)
}
