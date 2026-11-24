// The Stage 4a update-round check's pure logic. Neither function touches a cluster, so, unlike
// live_pod_update_test.go's own `//go:build e2e`, this file carries no build tag: ordinary
// `go test ./...` compiles and exercises it directly (live_pod_update_unit_test.go).
package sandbox

import "strconv"

// podUpdateRunIssue is pod-update's issue key in the run whose project is project, seeded by that
// project and this check's own name (runIssue), so it is neither cache-b's key
// (cacheRunIssue) nor another run's. Both workers of the issue push its branch, legion/<issue>,
// onto the run's repository, which every Stage 4a run shares, so each run only ever creates and
// deletes its own; the key keeps cache-b's `legion/S4A-<digits>` shape, so a branch a killed run
// left is swept by cache-lost-volume's own sweep (staleRunBranches).
func podUpdateRunIssue(project string) string { return runIssue(project + " pod-update") }

// podUpdateFleetRunIssue is pod-update-fleet's second issue key in the run whose project is
// project, seeded as podUpdateRunIssue's is but by this check's own name, so the two issues the
// one template change drifts at once are two issues, each pushing a branch of its own onto the
// repository every Stage 4a run shares. It keeps the same `legion/S4A-<digits>` shape, so a
// branch a killed run left is swept by cache-lost-volume's own sweep (staleRunBranches).
func podUpdateFleetRunIssue(project string) string { return runIssue(project + " pod-update-fleet") }

// belowTheFloor is the contract the update-round checks write as the maker of a running pod
// (contractAnnotation): one below every floor a daemon of this release can hold, since the first
// floor stands for every daemon before the record (api.DaemonAPIFirstFloor). A pod recorded so
// gives the floor kind (floorDrift), which breaks, and so is what a round is opened on: the Sandbox
// keeps the pod it has and every process in it keeps running, and the only thing that has moved
// is the record of the daemon that made it. A move of the operator's configuration (the disk, the
// image, a volume) drains, and opens no round at all.
const belowTheFloor = 1

// belowTheFloorRecord is belowTheFloor as the annotation holds it.
var belowTheFloorRecord = strconv.Itoa(belowTheFloor)
