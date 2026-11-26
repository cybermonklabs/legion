// Package shapetest builds the largest registration production has seen, for the tests in
// cmd/listener and internal/bus that hold a registration of that shape to what it hands the server
// and what its reduction to consumer filters costs. It is an ordinary package rather than a
// _test.go file because Go cannot share test-only code across packages (see internal/ordertest).
package shapetest

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Topics is about twice the largest registration production has seen (802 topics, 2026-11-16):
// a holder of role following 800 Dispatch issues, each as `<KEY>` and `<KEY>.>`, with the per-kind
// topics of one issue that its `<KEY>.>` already holds, and the lane topic of role, 1,609 topics
// in all. The per-kind topics are those of an issue that sorts among the others, as production's
// did, and not first: each is then found contained only after the pairs before it are weighed,
// so a reduction that went back to the first pair after a drop would weigh every pair before
// them again.
func Topics(role string) []string {
	const issues = 800
	topics := make([]string, 0, 2*issues+9)
	for issue := range issues {
		topic := fmt.Sprintf("notifications.dispatch.issue.K-%d", issue)
		topics = append(topics, topic, topic+".>")
	}
	for _, kind := range []string{"message", "issue", "comment", "ask"} {
		topic := "notifications.dispatch.issue.K-400." + kind
		topics = append(topics, topic, topic+".>")
	}
	return append(topics, contracts.RoleTopicPrefix+role)
}

// Planned is Topics with holder's own agent subject, which a registration adds to the topics it is
// sent before it plans them (cmd/listener's subscribeHandler): 1,610 topics.
func Planned(role, holder string) []string {
	return append(Topics(role), contracts.LegacyAgentSubject(holder))
}
