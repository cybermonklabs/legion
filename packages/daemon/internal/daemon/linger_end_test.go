package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// endLinger brings the end of root's linger forward to now: once the park has queued its
// linger_close row, which await waits for as its caller's waits are bounded, the moment the record
// says the linger runs to, and then that row, so the linger's end runs at the outbox's next sweep
// rather than at the configured linger. Another root's linger keeps its own. The idle rig and the
// retro-after-close live proof's driver both call it. It stands in a file of its own, which names
// nothing main lacks, so the live proof's negative control lays it over main with the driver.
func endLinger(t *testing.T, pool *store.Pool, root string, await func(what string, cond func() bool)) {
	t.Helper()
	ctx := context.Background()
	await("the park of "+root+" queueing its linger's end", func() bool {
		var queued int
		if err := pool.QueryRow(ctx, "select count(*) from outbox where issue = $1 and kind = 'linger_close'", root).Scan(&queued); err != nil {
			t.Fatalf("read the linger_close row of %s: %v", root, err)
		}
		return queued > 0
	})
	if _, err := pool.Exec(ctx, "update issues set linger_until = now() - interval '1 minute' where key = $1", root); err != nil {
		t.Fatalf("end the linger of %s: %v", root, err)
	}
	if _, err := pool.Exec(ctx, "update outbox set next_at = now() where issue = $1 and kind = 'linger_close'", root); err != nil {
		t.Fatalf("bring the linger_close row of %s due: %v", root, err)
	}
}
