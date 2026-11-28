package refs

import "context"

// keptPointerTargetsKey marks the context of the versions a delete of the node route writes
// (WithKeptPointerTargets).
type keptPointerTargetsKey struct{}

// WithKeptPointerTargets marks ctx, the context a delete of the node route (R7 of
// the issue-tree spec: api's DELETE /api/v1/issues/{key}/nodes/{id}) writes the versions of the
// documents whose text it changes under, with the node addresses of the blocks it takes. The index
// pass of such a version states each of them gone, and leaves each prevented_by into one of them
// standing (retractGonePointers): a tuple a node that stays holds into a node the delete takes is
// that node's fact, and the risk stays.
func WithKeptPointerTargets(ctx context.Context, addresses []string) context.Context {
	kept := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		kept[address] = true
	}
	return context.WithValue(ctx, keptPointerTargetsKey{}, kept)
}

// keptPointerTargets is the set WithKeptPointerTargets marked ctx with, nil for any other context.
func keptPointerTargets(ctx context.Context) map[string]bool {
	kept, _ := ctx.Value(keptPointerTargetsKey{}).(map[string]bool)
	return kept
}
