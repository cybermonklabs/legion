// Package supervisetest is what a test outside supervise needs to stand in for a part of
// supervise.Store.
package supervisetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Holds is supervise.Store.HoldSessions, ClaimHolds and FreeHolds as the store's claim_sessions
// table keeps them, in memory: the first claim of a project to record a name holds it, all of one
// call's names or none, until its own release frees it. The zero value holds nothing and is ready
// to use.
type Holds struct {
	mu    sync.Mutex
	holds map[holdKey]claim.Token
}

type holdKey struct {
	project string
	hold    supervise.Hold
}

// HoldSessions is supervise.Store.HoldSessions: a call whose new names take its claim past
// supervise.MaxHeldNamesPerClaim records none and answers supervise.ErrHeldNamesCap.
func (h *Holds) HoldSessions(_ context.Context, project string, token claim.Token, holds []supervise.Hold) ([]claim.Token, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holds == nil {
		h.holds = map[holdKey]claim.Token{}
	}
	holders := make([]claim.Token, len(holds))
	mine, added := true, map[holdKey]bool{}
	for i, hold := range holds {
		holder, held := h.holds[holdKey{project, hold}]
		if !held {
			holder = token
			added[holdKey{project, hold}] = true
		}
		holders[i] = holder
		mine = mine && holder == token
	}
	if !mine {
		return holders, nil
	}
	if len(added) > 0 {
		held := len(added)
		for k, holder := range h.holds {
			if k.project == project && holder == token {
				held++
			}
		}
		if held > supervise.MaxHeldNamesPerClaim {
			return nil, fmt.Errorf("%d names held with this call's %d new ones: %w", held, len(added), supervise.ErrHeldNamesCap)
		}
	}
	for _, hold := range holds {
		h.holds[holdKey{project, hold}] = token
	}
	return holders, nil
}

// ClaimHolds is supervise.Store.ClaimHolds: every name token's claim holds within project, in kind
// and then name order.
func (h *Holds) ClaimHolds(_ context.Context, project string, token claim.Token) ([]supervise.Hold, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var holds []supervise.Hold
	for k, holder := range h.holds {
		if k.project == project && holder == token {
			holds = append(holds, k.hold)
		}
	}
	sortHolds(holds)
	return holds, nil
}

// FreeHolds is supervise.Store.FreeHolds: it deletes every one of holds held for token's claim
// within project and answers those, in kind and then name order.
func (h *Holds) FreeHolds(_ context.Context, project string, token claim.Token, holds []supervise.Hold) ([]supervise.Hold, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var freed []supervise.Hold
	for _, hold := range holds {
		k := holdKey{project, hold}
		if holder, held := h.holds[k]; held && holder == token {
			delete(h.holds, k)
			freed = append(freed, hold)
		}
	}
	sortHolds(freed)
	return freed, nil
}

// sortHolds puts holds in the store's order: kind, then name.
func sortHolds(holds []supervise.Hold) {
	slices.SortFunc(holds, func(a, b supervise.Hold) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
}
