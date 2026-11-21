package main

// edge_roles_test.go holds the one edge role-release row that edge_mode_test.go does not: a
// registration at the edge releasing a claim of another project's role that the session's stored
// row does not name, while the role claim copy is refilling, so the release reads the role bucket
// itself. The role hold, free-form, read and the other release rows are in edge_mode_test.go, the
// GitHub rows in edge_repositories_test.go, and the rig every edge row runs on is in
// edge_rig_test.go.
//
// As there, every row reads the projects its listener serves from ENVOY_EDGE_PROJECTS when the
// test's own environment sets it, and serves PRIV otherwise (edgeTestProjects), so the control
// every row is run under once is the same command with ENVOY_EDGE_PROJECTS set empty.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// A registration at the edge releases a claim of another project's role that the session's
// stored row does not name, also while the role claim copy is refilling and names no role
// (store.Registry.RolesHeldBy refuses): neither source names the claim then, so the release reads
// the role bucket itself, as a deregistration does when the copy refuses it. Controls: with edge
// mode off, and with the release taking the copy's refusal as no claims (no read of the bucket),
// the claim still names the session after the registration, and the row fails.
func TestAnEdgeReleasesAClaimNoRowNamesWhileTheClaimViewRefills(t *testing.T) {
	rig := newEdgeRig(t)
	const moved, infraRole = "ses_edge_role_unnamed", "legion-infra-infra-8-reviewer"
	rig.placeElsewhere(t, moved)
	// A claim whose lane topic is on no row of the session: written to the role bucket alone, as
	// a grant through another listener whose row write was lost leaves it.
	record, err := json.Marshal(store.RoleClaim{HolderSessionID: moved, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode %s's claim: %v", infraRole, err)
	}
	if _, err := registryRoleKV(t, rig.deps.registry).Put(infraRole, record); err != nil {
		t.Fatalf("store %s's claim for %s: %v", infraRole, moved, err)
	}
	testwait.Eventually(t, "the role claim copy to name the claim", func() bool {
		held, err := rig.deps.registry.RolesHeldBy(moved)
		return err == nil && slices.Contains(held, infraRole)
	})
	if row, ok := rig.deps.registry.Cached(moved); ok && slices.Contains(row.Topics, contracts.RoleTopicPrefix+infraRole) {
		t.Fatalf("UNKNOWN: %s's stored row names %s's lane (%v), so the release would read the claim from it", moved, infraRole, row.Topics)
	}
	refilled := refuseRoleClaimView(t, rig.deps.registry)
	defer refilled()
	if _, err := rig.deps.registry.RolesHeldBy(moved); err == nil {
		t.Fatal("UNKNOWN: the role claim copy still answers, so the registration would read the claim from it")
	}

	answer := rig.subscribe(t, rig.personToken(moved, "PRIV"), moved)
	requireStatus(t, answer, http.StatusOK, "the moved session's registration at the edge while the claim copy refills")
	var registered listenerwire.SubscribeResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", answer.Body.String(), err)
	}
	if !slices.ContainsFunc(registered.Warnings, func(warning string) bool { return strings.HasPrefix(warning, "role "+infraRole+" was released") }) {
		t.Errorf("the registration's warnings %q name no release of %s", registered.Warnings, infraRole)
	}
	if claim, err := rig.deps.registry.RoleClaim(t.Context(), infraRole); err != nil || (claim.Revision != 0 && claim.HolderSessionID == moved) {
		t.Fatalf("after the registration at the edge %s's claim reads %+v (%v): %s still holds a role of INFRA", infraRole, claim, err, moved)
	}
}
