package api

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionIssueFactsAge is how long one computation of a project's per-issue Legion facts serves
// the board route: the board is read again on every live push, and the facts read the project's
// whole history (metrics.ReadProject), so a minute-old answer is what a push reads.
const legionIssueFactsAge = time.Minute

type legionIssueFactsKey struct {
	pool    *store.Pool
	project string
}

type legionIssueFactsEntry struct {
	at    time.Time
	facts map[string]metrics.IssueFacts
}

var (
	legionIssueFactsMu    sync.Mutex
	legionIssueFactsCache = map[legionIssueFactsKey]legionIssueFactsEntry{}
)

// legionIssueFacts is every issue's Legion facts for the board route — hours by phase, work
// time, review rounds, root and tree — computed at most once a minute per project. The rule
// itself is metrics.ProjectFacts and lives nowhere else, so every route that serves these
// numbers serves one computation of them. Two reads that miss at once both compute, which costs
// a read and is right.
func (s *server) legionIssueFacts(ctx context.Context, project string, now time.Time) (map[string]metrics.IssueFacts, error) {
	key := legionIssueFactsKey{pool: s.deps.Store.Pool, project: project}
	legionIssueFactsMu.Lock()
	entry, held := legionIssueFactsCache[key]
	legionIssueFactsMu.Unlock()
	if held && now.Sub(entry.at) < legionIssueFactsAge {
		return entry.facts, nil
	}
	population, err := metrics.ReadProject(ctx, s.deps.Store.Pool, s.deps.GitHub, project, now)
	if err != nil {
		return nil, err
	}
	facts := metrics.ProjectFacts(population)
	legionIssueFactsMu.Lock()
	legionIssueFactsCache[key] = legionIssueFactsEntry{at: now, facts: facts}
	legionIssueFactsMu.Unlock()
	return facts, nil
}
