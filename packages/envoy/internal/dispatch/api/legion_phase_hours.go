package api

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionPhaseHoursAge is how long one computation of a project's per-issue phase hours serves the
// board route: the board is read again on every live push, and the hours read the project's
// whole history (metrics.ReadProject), so a minute-old answer is what a push reads.
const legionPhaseHoursAge = time.Minute

type legionPhaseHoursKey struct {
	pool    *store.Pool
	project string
}

type legionPhaseHoursEntry struct {
	at    time.Time
	hours map[string]map[string]float64
}

var (
	legionPhaseHoursMu    sync.Mutex
	legionPhaseHoursCache = map[legionPhaseHoursKey]legionPhaseHoursEntry{}
)

// legionPhaseHours is every issue's hours by phase for the board route, computed at most once a
// minute per project. Two reads that miss at once both compute, which costs a read and is right.
func (s *server) legionPhaseHours(ctx context.Context, project string, now time.Time) (map[string]map[string]float64, error) {
	key := legionPhaseHoursKey{pool: s.deps.Store.Pool, project: project}
	legionPhaseHoursMu.Lock()
	entry, held := legionPhaseHoursCache[key]
	legionPhaseHoursMu.Unlock()
	if held && now.Sub(entry.at) < legionPhaseHoursAge {
		return entry.hours, nil
	}
	population, err := metrics.ReadProject(ctx, s.deps.Store.Pool, s.deps.GitHub, project, now)
	if err != nil {
		return nil, err
	}
	hours := metrics.PhaseHours(population)
	legionPhaseHoursMu.Lock()
	legionPhaseHoursCache[key] = legionPhaseHoursEntry{at: now, hours: hours}
	legionPhaseHoursMu.Unlock()
	return hours, nil
}
