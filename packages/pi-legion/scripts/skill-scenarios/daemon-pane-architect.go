package main

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// init gives daemon-pane.go the daemon's own composition of what an architect is told after its
// addressing (architectAddressing). This file is the one part of the program that calls
// daemon.ArchitectAddressing, so daemon-pane.ts builds it in, as cmd/daemon-pane/architect.go
// beside daemon-pane.go, only inside a checkout whose daemon declares that function; inside an
// older one the program composes an architect's addressing from its own restatements instead.
func init() {
	architectAddressing = rigArchitectAddressing
}

// rigArchitectAddressing is what the architect of c is told after its addressing under the rig's
// deployment, which defines the roles of rigRoles, lists no model, leaves `gates.design` unset
// (config.Gates.Steps) and sets no `production_check` (rigProductionCheck), composed by the
// daemon's own function, as workerSpawnSpec composes it.
func rigArchitectAddressing(c supervise.Claim) string {
	roles := make([]config.Role, len(rigRoles))
	for i, role := range rigRoles {
		roles[i] = config.Role{Name: claim.Role(role.name), Type: plan.Type(role.typ), Model: role.model}
	}
	return daemon.ArchitectAddressing(c, roles, nil, config.Gates{}.Steps(), rigProductionCheck)
}
