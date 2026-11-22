package consumernames

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
)

// familyNames is the selection protocol of m's family as the family's creator configures it, in
// the parts a consumer's check reads (selection.Names.Owns): the fixed name, the key's own check
// and the metadata every consumer of the family carries (bus newFreshSessions, the listener's
// durableNames, Dispatch's nameFreshly, the daemon's newDurableNames). member.Metadata is each
// family's metadata as its creator writes it. A record's check is the library's own
// (checkRecordFence).
func familyNames(m member) *selection.Names {
	config := selection.Config{
		Prefix:   func(string) string { return m.Prefix },
		Fixed:    func(string) string { return m.Fixed },
		Metadata: func(string) map[string]string { return m.Metadata() },
	}
	if m.Family == familySession && m.SessionID == "" {
		config.CheckKey = selection.CheckDurableKey
	}
	return selection.New(config)
}

// familyMember is logical's member of family (replayMember), refused as selection.ErrMalformedRecord
// when logical is no logical name of a family, or of another family than family.
func familyMember(family, logical string) (member, error) {
	m, err := replayMember(logical)
	if err != nil {
		return member{}, fmt.Errorf("%w: the key %s is no logical name of a family: %v", selection.ErrMalformedRecord, logical, err)
	}
	if m.Family != family {
		return member{}, fmt.Errorf("%w: the key %s is %s's logical name, not %s's", selection.ErrMalformedRecord, logical, m.Family, family)
	}
	return m, nil
}

// checkRecordFence is the one fence of a record status, the census's reader and cleanup read: the
// record of logical in family's store must be one the family's creator takes, by the library's own
// table of the families' naming (selection.CheckFamily): its current, each retired name and an open
// operation's candidate a fresh name of logical, and an open operation's old name one of those or
// logical's fixed name. Its refusal is selection.ErrMalformedRecord naming the field and the name,
// or the library's refusal of a family or a durable key no store keeps. A nil is no leave to
// delete: a name this package deletes also passes deleteOwned.
func checkRecordFence(family, logical string, record selection.Record) error {
	return selection.CheckFamily(family, logical, record)
}

// deleteOwned deletes consumer name, which m's record or m's logical name names, only once the
// server's info of it shows it is m's own (selection.Names.Owns of familyNames: the fixed name by
// its name, any other by the metadata the family's consumers carry). One the server does not hold
// is no error; one of another key is selection.ErrMalformedRecord and is not deleted.
func deleteOwned(ctx context.Context, nc *nats.Conn, m member, name string) error {
	info, err := consumerInfo(ctx, nc, notifications, name)
	if err != nil {
		return fmt.Errorf("the info of %s: %w", name, err)
	}
	return deleteIfOwned(ctx, nc, m, info)
}

// deleteIfOwned is deleteOwned of a consumer whose info the caller has read (info), nil for one the
// server does not hold.
func deleteIfOwned(ctx context.Context, nc *nats.Conn, m member, info *jetstream.ConsumerInfo) error {
	if info == nil {
		return nil
	}
	if err := familyNames(m).Owns(m.Logical, info); err != nil {
		return err
	}
	return deleteConsumer(ctx, nc, notifications, info.Name)
}
