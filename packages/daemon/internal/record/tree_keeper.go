package record

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
)

// TreeKeeper is the shadow keeper as one daemon's decision passes run it (R22 of
// dispatch://INFRA-3320): each pass keeps the tree lifecycles its project's stored trees need
// open (Store.KeepTreeLifecyclesOpen), and a stored tree value whose row the keeper leaves as it
// stands is logged once, with the reason, the first time a pass finds it left for that reason. A
// later pass that finds it left for the same reason logs nothing; one that finds it kept forgets
// it, so a value left again afterwards is logged again. The memory is the process's own: a
// restarted daemon logs each value it still leaves once more.
type TreeKeeper struct {
	log  *slog.Logger
	mu   sync.Mutex
	told map[string]string
}

// NewTreeKeeper is a keeper that logs to log.
func NewTreeKeeper(log *slog.Logger) *TreeKeeper {
	return &TreeKeeper{log: log, told: map[string]string{}}
}

// Keep runs the keeper for the Dispatch project key project in tx and logs each value it leaves
// that it has not logged for that reason yet. The log line is written whether or not tx commits:
// it reports what the store held when the pass read it.
func (k *TreeKeeper) Keep(ctx context.Context, tx pgx.Tx, store Store, project string) error {
	unkept, err := store.KeepTreeLifecyclesOpen(ctx, tx, project)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	left := make(map[string]string, len(unkept))
	for _, tree := range unkept {
		left[tree.Tree] = tree.Reason
		if k.told[tree.Tree] == tree.Reason {
			continue
		}
		k.log.Warn("the tree lifecycle a stored tree needs open for the release before stays as it stands",
			"project", project, "tree", tree.Tree, "reason", tree.Reason)
	}
	k.told = left
	return nil
}
