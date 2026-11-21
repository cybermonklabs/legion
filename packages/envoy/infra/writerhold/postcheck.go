package writerhold

import (
	"context"

	"github.com/nats-io/nats.go"
)

// ChangeKind is what a held command did to one protected stream.
type ChangeKind string

// The three things a command can have done to a protected stream.
const (
	// ChangeKept is a stream the command neither recreated nor restored; it may have written it.
	ChangeKept ChangeKind = "kept"
	// ChangeRecreated is a stream the command created: deleted and created again, or created
	// where step 1 recorded it absent.
	ChangeRecreated ChangeKind = "recreated"
	// ChangeRestored is a stream the command restored from an archive (STREAM.RESTORE).
	ChangeRestored ChangeKind = "restored"
)

// ArchiveState is the state of the archive a command restored a stream from, as the archive's own
// snapshot answer stated it and the command validated it before the restore: its last sequence and
// its message count.
type ArchiveState struct {
	LastSeq  uint64 `json:"last_seq"`
	Messages uint64 `json:"messages"`
}

// Change is a held command's own account of one protected stream: what it did (Kind), how many
// writes it made to it (Writes: since step 1 for a kept stream, after its create for a recreated
// one, after the restore for a restored one), and, for a restore, the archive's state. A write
// whose answer was lost counts when the command read its own record or revision back (§2,
// "Ambiguous results"): the command states it here, and the postcheck does not guess.
type Change struct {
	Kind    ChangeKind
	Writes  uint64
	Archive ArchiveState
}

// Postcheck is the postcheck of a held command (§3a, "The postcheck, per stream incarnation and per
// operation"), on nc, the command's connection under the held grant, after its last mutation. It
// reads every protected stream by name again and compares it with step1, the preflight's
// readings, through changes, the command's account of each stream it changed (a stream changes
// does not name was kept with no write):
//   - a kept stream keeps its `created`, and its `last_seq` is step 1's plus the command's writes;
//   - a recreated stream has a `created` other than step 1's, and its `last_seq` is exactly the
//     command's writes;
//   - a restored stream's `created` is not compared, since no version keeps it the same way:
//     nats-server 2.15.1 snapshots in its V2 archive (stream.go:9477), whose restore
//     (stream_backup.go's RestoreStreamV2) sets a new one, while the V1 restore of 2.10.29 keeps
//     the archive's (stream.go:9622-9623); its `last_seq` and message count are the archive's
//     plus the command's writes after the restore;
//   - a stream step 1 found absent and the command did not create stays absent, and no other
//     stream is absent.
//
// Each present stream must again show no sources, no mirror, no republish, no subject transform
// and allow_msg_schedules false, and then the writer census runs again over the account. Any other
// difference stops the command naming the stream. The readings, in ProtectedStreams' order, are
// returned.
func Postcheck(ctx context.Context, nc *nats.Conn, step1 []StreamReading, changes map[string]Change) ([]StreamReading, error) {
	return postcheck(ctx, nc, step1, changes, streamChecks{})
}

func postcheck(ctx context.Context, nc *nats.Conn, step1 []StreamReading, changes map[string]Change, checks streamChecks) ([]StreamReading, error) {
	recorded := make(map[string]StreamReading, len(step1))
	for _, reading := range step1 {
		recorded[reading.Name] = reading
	}
	names := ProtectedStreams()
	for name, change := range changes {
		if _, ok := recorded[name]; !ok {
			return nil, unknownf(StepPostcheck, nil, "the command states a change to %s, which step 1 did not read", name)
		}
		switch change.Kind {
		case ChangeKept, ChangeRecreated, ChangeRestored:
		default:
			return nil, unknownf(StepPostcheck, nil, "the command states change %q to %s, which is not kept, recreated or restored", change.Kind, name)
		}
	}
	readings := make([]StreamReading, 0, len(names))
	var present []string
	for _, name := range names {
		was, ok := recorded[name]
		if !ok {
			return nil, unknownf(StepPostcheck, nil, "step 1 recorded no reading of %s", name)
		}
		change, stated := changes[name]
		if !stated {
			change = Change{Kind: ChangeKept}
		}
		info, found, err := streamInfo(ctx, nc, name)
		if err != nil {
			return nil, unknownf(StepPostcheck, err, "STREAM.INFO of %s", name)
		}
		if !found {
			if was.Absent && change.Kind == ChangeKept && change.Writes == 0 {
				readings = append(readings, StreamReading{Name: name, Absent: true})
				continue
			}
			return nil, refusef(StepPostcheck, "%s is absent after the command, which states it %s it with %d writes", name, change.Kind, change.Writes)
		}
		if err := checks.serverWriters(StepPostcheck, info); err != nil {
			return nil, err
		}
		now := readingOf(info)
		if err := checks.compare(name, was, now, change); err != nil {
			return nil, err
		}
		readings = append(readings, now)
		present = append(present, name)
	}
	if _, err := takeCensus(ctx, nc, StepPostcheck, present, checks); err != nil {
		return nil, err
	}
	return readings, nil
}

// compare refuses a stream whose reading after the command, now, is not what step 1's reading,
// was, and the command's change make it.
func (c streamChecks) compare(name string, was, now StreamReading, change Change) error {
	switch change.Kind {
	case ChangeKept:
		if was.Absent {
			return refusef(StepPostcheck, "%s was absent at step 1 and is present after the command, which states it kept it", name)
		}
		if !now.Created.Equal(was.Created) {
			return refusef(StepPostcheck, "%s's created moved from %s to %s, and the command states it neither recreated nor restored it", name, was.Created, now.Created)
		}
		if now.LastSeq != was.LastSeq+change.Writes {
			return refusef(StepPostcheck, "%s's last_seq is %d, and step 1's %d plus the command's %d writes is %d", name, now.LastSeq, was.LastSeq, change.Writes, was.LastSeq+change.Writes)
		}
	case ChangeRecreated:
		if !was.Absent && now.Created.Equal(was.Created) {
			return refusef(StepPostcheck, "%s keeps step 1's created %s, and the command states it recreated it", name, was.Created)
		}
		if now.LastSeq != change.Writes {
			return refusef(StepPostcheck, "%s's last_seq is %d, and the command states %d writes to the stream it created", name, now.LastSeq, change.Writes)
		}
	case ChangeRestored:
		if c.restoreNewCreated && now.Created.Equal(was.Created) {
			return refusef(StepPostcheck, "%s keeps step 1's created %s, and the command states it restored it", name, was.Created)
		}
		if c.restoreSameCreated && !now.Created.Equal(was.Created) {
			return refusef(StepPostcheck, "%s's created moved from %s to %s, and the command states it restored it", name, was.Created, now.Created)
		}
		if now.LastSeq != change.Archive.LastSeq+change.Writes {
			return refusef(StepPostcheck, "%s's last_seq is %d, and the archive's %d plus the command's %d writes after the restore is %d", name, now.LastSeq, change.Archive.LastSeq, change.Writes, change.Archive.LastSeq+change.Writes)
		}
		if now.Messages != change.Archive.Messages+change.Writes {
			return refusef(StepPostcheck, "%s holds %d messages, and the archive's %d plus the command's %d writes after the restore is %d", name, now.Messages, change.Archive.Messages, change.Writes, change.Archive.Messages+change.Writes)
		}
	}
	return nil
}
