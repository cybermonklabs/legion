package docs

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/collabtest"
)

// checkedIdentity is the docs window rigs' decorator of the identity a document service resolves
// a socket's person with: it checks Login, which takes its context from the request it resolves
// (collabtest.Check), and calls through.
func checkedIdentity(t testing.TB, next identity.Identity) identity.Identity {
	return checkedLogin{t: t, next: next}
}

type checkedLogin struct {
	t    testing.TB
	next identity.Identity
}

func (c checkedLogin) Login(r *http.Request) (string, error) {
	collabtest.Check(c.t, r.Context(), "identity.Identity", "Login")
	return c.next.Login(r)
}

// checkedVersioned is the docs window rigs' decorator of the document store (VersionedStore): it
// checks every method that takes a context (collabtest.Check) and calls through. The interface has
// unexported methods, so it lives here rather than in collabtest. pgVersioned takes no context,
// so it cannot be checked, and what the service does through the *PgVersioned it answers is not
// seen here.
func checkedVersioned(t testing.TB, next VersionedStore) VersionedStore {
	return checkedVersionedStore{t: t, next: next}
}

type checkedVersionedStore struct {
	t    testing.TB
	next VersionedStore
}

func (c checkedVersionedStore) check(ctx context.Context, method string) {
	collabtest.Check(c.t, ctx, "docs.VersionedStore", method)
}

func (c checkedVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	c.check(ctx, "Load")
	return c.next.Load(ctx, room)
}

func (c checkedVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	c.check(ctx, "AppendUpdate")
	return c.next.AppendUpdate(ctx, room, update)
}

func (c checkedVersionedStore) ListVersions(ctx context.Context, room string) ([]persistence.VersionMeta, error) {
	c.check(ctx, "ListVersions")
	return c.next.ListVersions(ctx, room)
}

func (c checkedVersionedStore) GetUpdate(ctx context.Context, room string, v persistence.Version) ([]byte, persistence.VersionMeta, bool, error) {
	c.check(ctx, "GetUpdate")
	return c.next.GetUpdate(ctx, room, v)
}

func (c checkedVersionedStore) MaterializeAt(ctx context.Context, room string, v persistence.Version) ([]byte, error) {
	c.check(ctx, "MaterializeAt")
	return c.next.MaterializeAt(ctx, room, v)
}

func (c checkedVersionedStore) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (persistence.Version, error) {
	c.check(ctx, "CaptureSnapshot")
	return c.next.CaptureSnapshot(ctx, room, name, state)
}

func (c checkedVersionedStore) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, persistence.Version, bool, error) {
	c.check(ctx, "RestoreSnapshot")
	return c.next.RestoreSnapshot(ctx, room, name)
}

func (c checkedVersionedStore) PruneAfter(ctx context.Context, room string, target persistence.Version, rolledBack []byte) error {
	c.check(ctx, "PruneAfter")
	return c.next.PruneAfter(ctx, room, target, rolledBack)
}

func (c checkedVersionedStore) Compact(ctx context.Context, room string, keep int) (int, error) {
	c.check(ctx, "Compact")
	return c.next.Compact(ctx, room, keep)
}

func (c checkedVersionedStore) Delete(ctx context.Context, room string) error {
	c.check(ctx, "Delete")
	return c.next.Delete(ctx, room)
}

func (c checkedVersionedStore) AppendRoomFlush(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit) (persistence.Version, error) {
	c.check(ctx, "AppendRoomFlush")
	return c.next.AppendRoomFlush(ctx, room, flush, through, credit)
}

func (c checkedVersionedStore) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	c.check(ctx, "AppendUpdatesWithCredit")
	return c.next.AppendUpdatesWithCredit(ctx, room, updates)
}

func (c checkedVersionedStore) AppendParkedUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	c.check(ctx, "AppendParkedUpdate")
	return c.next.AppendParkedUpdate(ctx, room, update)
}

func (c checkedVersionedStore) AppendRoomUpdatesTx(ctx context.Context, tx pgx.Tx, room string, flush *RoomFlush, updates []BrowserUpdate) (persistence.Version, int, error) {
	c.check(ctx, "AppendRoomUpdatesTx")
	return c.next.AppendRoomUpdatesTx(ctx, tx, room, flush, updates)
}

func (c checkedVersionedStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	c.check(ctx, "AppendUpdateTx")
	return c.next.AppendUpdateTx(ctx, tx, room, update, contentChanged)
}

func (c checkedVersionedStore) RebuildTx(ctx context.Context, tx pgx.Tx, room string, loaded persistence.Version, seed []byte) (RebuildReport, error) {
	c.check(ctx, "RebuildTx")
	return c.next.RebuildTx(ctx, tx, room, loaded, seed)
}

func (c checkedVersionedStore) Head(ctx context.Context, room string) (persistence.Version, error) {
	c.check(ctx, "Head")
	return c.next.Head(ctx, room)
}

func (c checkedVersionedStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	c.check(ctx, "DocumentStamp")
	return c.next.DocumentStamp(ctx, room)
}

func (c checkedVersionedStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	c.check(ctx, "LoadDocument")
	return c.next.LoadDocument(ctx, room)
}

func (c checkedVersionedStore) compactRoom(ctx context.Context, room string, rule compactRule, wait bool) (compactResult, error) {
	c.check(ctx, "compactRoom")
	return c.next.compactRoom(ctx, room, rule, wait)
}

func (c checkedVersionedStore) pgVersioned() *PgVersioned { return c.next.pgVersioned() }
