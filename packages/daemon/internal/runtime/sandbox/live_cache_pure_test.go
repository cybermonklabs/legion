// The Stage 4a cache checks' pure logic. None of it touches a cluster, so it sits apart from
// live_cache_test.go, which the kind job runs whole, and daemon-go exercises it directly
// (live_cache_unit_test.go).
package sandbox

import (
	"fmt"
	"hash/fnv"
)

// runIssue is a Stage 4a check's issue key in the run its seed names: S4A- and the FNV-1a hash of
// that seed. Each check of a run that pushes its issue's branch, legion/<issue>, onto the run's
// repository has a seed of its own, so each pushes a branch of its own (cacheRunIssue,
// podUpdateRunIssue).
func runIssue(seed string) string {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(seed))
	return fmt.Sprintf("S4A-%d", sum.Sum32())
}

// cacheRunIssue is cache-b's issue key in the run whose project is project, seeded by the project
// itself. cache-lost-volume pushes the issue's branch onto the run's repository.
func cacheRunIssue(project string) string { return runIssue(project) }

// runCommitMessage is the message of the commit a check pushes on branch.
func runCommitMessage(branch string) string { return "Stage 4a: " + branch + "'s own commit" }
