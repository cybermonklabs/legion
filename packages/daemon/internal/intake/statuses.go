package intake

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// rawStatuses reads a settlement's commit statuses, which Envoy's listener writes as `statuses`, a
// list of {context, state, created_at} in context order, and leaves out when the record holds
// none: each with a context of its own, a state of success, failure or error, and GitHub's
// created_at as an RFC 3339 time, which orders two statuses of one context
// (classify.ApplySettlement) and is kept in UTC, sorted by context. An absent or null field names
// no status, which is the empty list. defect says what else the field holds, "" when it was read;
// an entry's other keys are the listener's to add and are not read.
func rawStatuses(value json.RawMessage) ([]record.CommitStatus, string) {
	if value == nil || string(value) == "null" {
		return nil, ""
	}
	var incoming []struct {
		Context   string                   `json:"context"`
		State     record.CommitStatusState `json:"state"`
		CreatedAt *string                  `json:"created_at"`
	}
	if err := json.Unmarshal(value, &incoming); err != nil {
		return nil, "is not a list of commit statuses"
	}
	seen := make(map[string]struct{}, len(incoming))
	statuses := make([]record.CommitStatus, 0, len(incoming))
	for i, item := range incoming {
		if item.Context == "" {
			return nil, fmt.Sprintf("names no context at entry %d", i)
		}
		switch item.State {
		case record.CommitStatusSuccess, record.CommitStatusFailure, record.CommitStatusError:
		default:
			return nil, fmt.Sprintf("names %q in the state %q, which is not success, failure or error", item.Context, item.State)
		}
		if item.CreatedAt == nil {
			return nil, fmt.Sprintf("names %q with no created_at", item.Context)
		}
		createdAt, err := time.Parse(time.RFC3339, *item.CreatedAt)
		if err != nil {
			return nil, fmt.Sprintf("names %q with the created_at %q, which is no RFC 3339 time", item.Context, *item.CreatedAt)
		}
		if _, duplicate := seen[item.Context]; duplicate {
			return nil, fmt.Sprintf("names the context %q twice", item.Context)
		}
		seen[item.Context] = struct{}{}
		statuses = append(statuses, record.CommitStatus{Context: item.Context, State: item.State, CreatedAt: createdAt.UTC()})
	}
	slices.SortFunc(statuses, func(a, b record.CommitStatus) int { return strings.Compare(a.Context, b.Context) })
	return statuses, ""
}
