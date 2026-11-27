//go:build natslive

package natsgrant_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
)

// The grant rows (TestOnlyUsersOnTheListenersGrantWriteOrReadAnExpiryNotice) spell the expiry
// subject out so they run at a tree with no expiry notice too; these hold those strings to the
// subject and the filter bus writes and reads the notices by, so the grant rows test the subject
// the listener writes.
func TestTheGrantRowsNameTheExpirySubjectBusWrites(t *testing.T) {
	require.Equal(t, bus.KeptExpiredSubject(expiryNoticeRole), expiryNoticeSubject)
	require.Equal(t, bus.KeptExpiredSubjectPrefix+">", expiryNoticeSubjects)
}

// The listener's user stores an expiry notice the way its walk does (bus.Client.PublishKeptCopyExpiry),
// under the copy's own message id (bus.KeptExpiryMsgID), and Dispatch's user reads the same notice
// back with the stored-message read its outbox makes, on the hub's image with the grant rows' users.
func TestAListenersUserStoresAnExpiryNoticeDispatchReadsBack(t *testing.T) {
	hub := startExpiryGrantHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	notice := bus.KeptCopyExpiry{EventID: "dispatch-7", SourceEventID: "7", Role: expiryNoticeRole, Attempt: 1, DedupeKey: "dispatch-7", Sequence: 3, KeptAt: 1, DroppedAt: 2}
	require.NoError(t, hub.owner.PublishKeptCopyExpiry(ctx, notice))
	outbox, err := bus.Connect([]string{hub.uri}, bus.WithName("dispatch"), seedEnvironment(hub.dispatch))
	require.NoError(t, err, "Dispatch's user connects")
	t.Cleanup(outbox.Close)
	stored, err := outbox.NextStoredMessage(ctx, bus.KeptExpiredSubjectPrefix+">", 1)
	require.NoError(t, err, "Dispatch's user reads the notice")
	var read bus.KeptCopyExpiry
	require.NoError(t, json.Unmarshal(stored.Data, &read))
	require.Equal(t, notice, read)
	require.Equal(t, bus.KeptExpiryMsgID(expiryNoticeRole, 3), stored.MsgID)
}
