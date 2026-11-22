package consumernames

import (
	"errors"
	"fmt"
	"strings"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
)

// The selection bucket's values are read as the selection package's store reads them. Any
// principal whose grant holds a JetStream API request can store the server's JSON answer under any
// key of the bucket, by naming the key's subject ($KV.<bucket>.<key>) as the request's reply
// subject, and a holder of the bucket's direct get can copy one key's record onto another; a
// lenient decode takes the first as an empty record, which selects nothing, and the second as the
// other key's, and a reader would then act on either. So every reader of the bucket's raw messages
// takes a value only through readSelectionValue, which decodes a record or the sentinel with the
// package's own decode (selection.DecodeKVValue, which selection.KVStore's Read and State call) and
// answers a value that is not exactly its key's own as malformed.

// strictBarrierPrefix begins every key of a hold's bucket barrier (holdrecord.Barrier), the system
// key prefix a restore skips (kvrecords.ForRestore).
const strictBarrierPrefix = kvrecords.HoldBarrierKeyPrefix

// strictBarrier refuses data under key, a hold's barrier key hold.barrier.<hold id>, unless it is
// exactly what holdrecord.Barrier puts there: the hold id itself, one subject token.
func strictBarrier(key string, data []byte) error {
	id, ok := strings.CutPrefix(key, strictBarrierPrefix)
	if !ok || id == "" || strings.ContainsAny(id, ".*> \t\r\n") {
		return fmt.Errorf("%q is no hold's barrier key", key)
	}
	if string(data) != id {
		return errors.New("no barrier value: a barrier's value is its hold id")
	}
	return nil
}

// selectionValue is one key's value of the selection bucket as readSelectionValue read it: a
// session's or a durable's record, or the sentinel's store state; a hold's barrier holds neither.
type selectionValue struct {
	record *selection.Record
	state  *selection.StoreState
}

// readSelectionValue reads key's value, whose last write is no delete or purge marker, by its
// key's kind: a session's (selection.CheckKey) or a durable's (selection.CheckDurableKey) record,
// or the sentinel's store state, each through selection.DecodeKVValue, which refuses a value that
// is not exactly that key's own record or the bucket's own sentinel (selection.ErrMalformedRecord,
// selection.ErrMalformedSentinel); or a hold's barrier, its hold id (strictBarrier). A key of no
// kind the bucket holds is refused whatever it holds: nothing writes one but a reply subject. An
// error is a malformed value, which every caller answers as UNKNOWN or refuses, naming the key, and
// acts on nothing. It is the one place the census, status, cleanup, restore, create-bucket and
// repair-sentinel decode a raw message of the bucket; their reads through selection.KVStore decode
// with the same selection.DecodeKVValue.
func readSelectionValue(key statusKey) (selectionValue, error) {
	switch {
	case selection.CheckKey(key.Key) == nil || selection.CheckDurableKey(key.Key) == nil || key.Key == selection.SentinelKey:
		record, state, isSentinel, err := selection.DecodeKVValue(key.Key, key.Value)
		switch {
		case err != nil:
			return selectionValue{}, err
		case isSentinel:
			return selectionValue{state: &state}, nil
		}
		return selectionValue{record: &record}, nil
	case strings.HasPrefix(key.Key, strictBarrierPrefix):
		return selectionValue{}, strictBarrier(key.Key, key.Value)
	}
	return selectionValue{}, errors.New("no key of a kind the bucket holds (a session's or a durable's record, the sentinel, a hold's barrier)")
}
