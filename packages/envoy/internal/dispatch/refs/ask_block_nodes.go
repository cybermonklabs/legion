package refs

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loadAskBlockNodes resolves the part nodes of asks, stored under kind ask_block and addressed
// 'ask/<ask uuid>#<part id>', as loadItemBlockNodes resolves a comment's blocks: a part no current
// revision holds resolves to nothing, the node's kind is block and its parent is its ask, at the
// revision the part's text last changed in. A question and an option are the ask's author's; the
// answer part is the answerer's, who is not the ask's author (ask_blocks.made_by). It carries no
// Text: only the node a reference read names does (CurrentItemBlockText).
func loadAskBlockNodes(ctx context.Context, q Queryer, who model.Actor, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	return loadItemBlockNodes(ctx, q, who, ItemAsk, kind, ids, nodes)
}
