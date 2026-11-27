// Package supervisetest holds in-memory doubles of what a supervise machine is built from, for
// the tests of the packages that build machines (supervise.Deps).
package supervisetest

import (
	"context"
	"slices"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// epoch is the bucket epoch every TaskValues writes under.
const epoch = "2026-11-13T00:00:00Z"

// TaskValues is the task bucket in memory (supervise.TaskValues): one value per claim, a
// revision counter the whole bucket shares, as a NATS key-value bucket's does, and a record of
// every write and delete in order. Its zero value is not usable: build it with NewTaskValues.
type TaskValues struct {
	mu       sync.Mutex
	revision uint64
	values   map[claim.Token]supervise.TaskValue
	writes   []Write
	deletes  []claim.Token
}

// Write is one write the bucket took: the claim it was for, and the value it left.
type Write struct {
	Token claim.Token
	supervise.TaskValue
}

// NewTaskValues is an empty bucket.
func NewTaskValues() *TaskValues {
	return &TaskValues{values: map[claim.Token]supervise.TaskValue{}}
}

// Write puts number as token's value under the next revision.
func (v *TaskValues) Write(_ context.Context, token claim.Token, number uint64) (string, uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.revision++
	value := supervise.TaskValue{Number: number, Epoch: epoch, Revision: v.revision}
	v.values[token] = value
	v.writes = append(v.writes, Write{Token: token, TaskValue: value})
	return value.Epoch, value.Revision, nil
}

// Delete deletes token's value; the delete takes a revision, as a bucket's delete marker does.
func (v *TaskValues) Delete(_ context.Context, token claim.Token) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.revision++
	delete(v.values, token)
	v.deletes = append(v.deletes, token)
	return nil
}

// Read is token's current value, and false where the bucket holds none.
func (v *TaskValues) Read(_ context.Context, token claim.Token) (supervise.TaskValue, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[token]
	return value, ok, nil
}

// Value is token's current value, and false for none.
func (v *TaskValues) Value(token claim.Token) (supervise.TaskValue, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[token]
	return value, ok
}

// Writes is every write made, in order.
func (v *TaskValues) Writes() []Write {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.writes)
}

// Deletes is every delete made, in order.
func (v *TaskValues) Deletes() []claim.Token {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.deletes)
}
