package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// A join of a write on a document of an issue room (JoinWrite) is refused with
// ErrServiceUnavailable while that room shuts down, as an operation on the document is
// (openLiveOperation): the service's shutdown marks the rooms it closes (shutdownRooms), the issue
// room's tree among them, not each document of it. Once the room no longer shuts down, the same
// join is taken.
func TestAJoinOnADocumentOfAnIssueRoomThatShutsDownIsRefused(t *testing.T) {
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	service.shutdownRooms.Store(room.tree, struct{}{})
	testcleanup.Add(t, func() { service.shutdownRooms.Delete(room.tree) })

	join := func(ctx context.Context) error { return service.JoinWrite(ctx, room.spec) }
	if err := joinedWrite(service, join); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("a join on the spec %s while its issue room %s shuts down answered %v, want ErrServiceUnavailable", room.spec, room.tree, err)
	}
	service.shutdownRooms.Delete(room.tree)
	if err := joinedWrite(service, join); err != nil {
		t.Fatalf("a join on the spec %s once its issue room %s no longer shuts down answered %v, want it taken", room.spec, room.tree, err)
	}
}
