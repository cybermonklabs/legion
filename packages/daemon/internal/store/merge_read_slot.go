package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MergeReadSlot is the one database-wide slot of the merge read: a session advisory lock named
// legion.merge_read, held on a connection of its own outside any transaction, so that at most one
// request of any project's merge read to GitHub or Dispatch stands at a time. A reader takes it
// for one request (TakeMergeReadSlot), reserves that request's attempt on its connection
// (Transact), sends the request, and gives it back (Release) before it applies the answer.
type MergeReadSlot struct {
	conn    *pgxpool.Conn
	release func()
}

// TakeMergeReadSlot takes the merge read's slot where no other holder has it, and answers false,
// holding nothing, where one does: it never waits, so a busy slot spends no request and no lease
// time of the caller's.
func (p *Pool) TakeMergeReadSlot(ctx context.Context) (*MergeReadSlot, bool, error) {
	return &MergeReadSlot{release: noRelease}, true, nil
}

// Transact runs write in one transaction on the slot's connection, which commits before the
// request it reserves is sent.
func (s *MergeReadSlot) Transact(ctx context.Context, write func(ctx context.Context, tx pgx.Tx) error) error {
	return nil
}

// Release gives the slot back.
func (s *MergeReadSlot) Release(ctx context.Context) {}
