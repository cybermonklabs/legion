// Package supervisetest is what a test outside supervise needs to stand in for a part of
// supervise.Store, or for supervise.Clock.
package supervisetest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Holds is supervise.Store.HoldSessions and ClaimHolds, and the release half of ReleaseHolds
// (ReleaseWith), as the store's claim_sessions table keeps them, in memory: the first claim of a
// project to record a name holds it, all of one call's names or none, until its own release frees
// it. The zero value holds nothing and is ready to use.
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

// ClaimHolds is supervise.Store.ClaimHolds.
func (h *Holds) ClaimHolds(_ context.Context, project string, token claim.Token) ([]supervise.Hold, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var holds []supervise.Hold
	for k, holder := range h.holds {
		if k.project == project && holder == token {
			holds = append(holds, k.hold)
		}
	}
	slices.SortFunc(holds, supervise.CompareHolds)
	return holds, nil
}

// ReleaseWith is supervise.Store.ReleaseHolds for a fake whose claim write is put: it writes c with
// put, and only once put took it deletes every one of holds held for c's claim within c's project,
// answering those in kind and then name order. A claim write put refuses frees nothing, as the
// store's one transaction rolls its delete back. A fake that embeds Holds answers ReleaseHolds
// with its own PutClaim: `return s.Holds.ReleaseWith(ctx, s.PutClaim, c, holds)`.
func (h *Holds) ReleaseWith(ctx context.Context, put func(context.Context, supervise.Claim) error, c supervise.Claim, holds []supervise.Hold) ([]supervise.Hold, error) {
	if err := put(ctx, c); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var freed []supervise.Hold
	for _, hold := range holds {
		k := holdKey{c.Project, hold}
		if holder, held := h.holds[k]; held && holder == c.Token {
			delete(h.holds, k)
			freed = append(freed, hold)
		}
	}
	slices.SortFunc(freed, supervise.CompareHolds)
	return freed, nil
}
