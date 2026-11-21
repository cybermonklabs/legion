package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// recordVersion is the fence record's shape; a record of another version is refused.
const recordVersion = 1

// record is what fence wrote of each stream once it was sealed: the reading every later step
// holds a stream to.
type record struct {
	Version  int       `json:"version"`
	FencedAt time.Time `json:"fenced_at"`
	Streams  []fenced  `json:"streams"`
}

// fenced is one stream as the fence left it.
type fenced struct {
	Name      string            `json:"name"`
	StreamID  string            `json:"stream_id"`
	FirstSeq  uint64            `json:"first_seq"`
	LastSeq   uint64            `json:"last_seq"`
	Msgs      uint64            `json:"messages"`
	Subjects  map[string]uint64 `json:"subjects"`
	Consumers []consumer        `json:"durable_consumers"`
}

func readRecord(path string) (record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return record{}, fmt.Errorf("the record %s is no JSON: %w", path, err)
	}
	if r.Version != recordVersion {
		return record{}, fmt.Errorf("the record %s is version %d, not %d", path, r.Version, recordVersion)
	}
	if len(r.Streams) == 0 {
		return record{}, fmt.Errorf("the record %s names no stream", path)
	}
	return r, nil
}

// writeRecord writes r to path as a new file: a record already there is another move's, left as
// it is.
func writeRecord(path string, r record) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// fence seals every stream of the account and records each one, sealed: a write that reached a
// stream before its seal is in the record, and none after it is accepted. A stream it could not
// seal or read is UNKNOWN by name and left out of the record, which is written for every stream it
// sealed, so a seal that cannot be lifted (nats-server refuses to unseal) always has a record the
// snapshot, and then the rollback, can read; the verdict is UNKNOWN, and the move does not go on
// until every stream is accounted for.
func fence(ctx context.Context, nc *nats.Conn, path string, o out) (int, error) {
	if _, err := os.Stat(path); err == nil {
		return exitUsage, usageError("the record %s exists: a fence writes a new one", path)
	}
	names, err := streamNames(ctx, nc)
	if err != nil {
		return exitUnknown, err
	}
	if len(names) == 0 {
		o.line("UNKNOWN the account holds no stream: there is nothing to move, or this is not the account the streams are in")
		return o.verdict("FENCED", 0, 1), nil
	}
	slices.Sort(names)
	r := record{Version: recordVersion, FencedAt: time.Now().UTC()}
	unknown := 0
	for _, name := range names {
		before, err := info(ctx, nc, name)
		if err != nil {
			o.line("UNKNOWN %s: %v", name, err)
			unknown++
			continue
		}
		if err := seal(ctx, nc, name, before.Config); err != nil {
			o.line("UNKNOWN %s: the seal was refused: %v", name, err)
			unknown++
			continue
		}
		f, err := read(ctx, nc, name)
		if err != nil {
			o.line("UNKNOWN %s: sealed, but its state could not be read: %v", name, err)
			unknown++
			continue
		}
		r.Streams = append(r.Streams, f)
		o.line("FENCED %s first=%d last=%d messages=%d subjects=%d durable_consumers=%d", name, f.FirstSeq, f.LastSeq, f.Msgs, len(f.Subjects), len(f.Consumers))
	}
	if len(r.Streams) == 0 {
		return o.verdict("FENCED", 0, unknown), nil
	}
	if err := writeRecord(path, r); err != nil {
		return exitUnknown, fmt.Errorf("write the record %s: %w", path, err)
	}
	return o.verdict("FENCED", 0, unknown), nil
}

// read is the stream as a record keeps it, read now, refusing a stream that is not sealed.
func read(ctx context.Context, nc *nats.Conn, name string) (fenced, error) {
	i, err := info(ctx, nc, name)
	if err != nil {
		return fenced{}, err
	}
	config, err := i.config()
	if err != nil {
		return fenced{}, err
	}
	if !config.Sealed {
		return fenced{}, errors.New("it is not sealed")
	}
	consumers, err := durables(ctx, nc, name)
	if err != nil {
		return fenced{}, err
	}
	return fenced{
		Name: name, StreamID: config.Metadata[bus.StreamIDMetadataKey],
		FirstSeq: i.State.FirstSeq, LastSeq: i.State.LastSeq, Msgs: i.State.Msgs,
		Subjects: i.State.Subjects, Consumers: consumers,
	}, nil
}

// snapshot takes each recorded stream's snapshot into dir, after checking the stream still holds
// what the record says: a stream that took a write after its seal (it cannot, while sealed) or lost
// one shuts the gate before its snapshot is taken.
func snapshot(ctx context.Context, nc *nats.Conn, path, dir string, o out) (int, error) {
	r, err := readRecord(path)
	if err != nil {
		return exitUnknown, err
	}
	shut, unknown := 0, 0
	for _, f := range r.Streams {
		now, err := read(ctx, nc, f.Name)
		if err != nil {
			o.line("UNKNOWN %s: %v", f.Name, err)
			unknown++
			continue
		}
		if diff := differs(f, now, false); diff != "" {
			o.line("GATE SHUT %s: %s", f.Name, diff)
			shut++
			continue
		}
		state, size, err := takeSnapshot(ctx, nc, f.Name, dir)
		if err != nil {
			o.line("UNKNOWN %s: the snapshot failed: %v", f.Name, err)
			unknown++
			continue
		}
		if state.FirstSeq != f.FirstSeq || state.LastSeq != f.LastSeq || state.Msgs != f.Msgs {
			o.line("GATE SHUT %s: the snapshot holds first=%d last=%d messages=%d, the record first=%d last=%d messages=%d",
				f.Name, state.FirstSeq, state.LastSeq, state.Msgs, f.FirstSeq, f.LastSeq, f.Msgs)
			shut++
			continue
		}
		o.line("SNAPSHOT %s bytes=%d first=%d last=%d messages=%d", f.Name, size, state.FirstSeq, state.LastSeq, state.Msgs)
	}
	return o.verdict("SNAPSHOT TAKEN", shut, unknown), nil
}

// layout reads, from the account the streams move into, that the layout's apply took: JetStream
// answers there, and no recorded stream is there yet, so a restore would not land on a stream
// another write made.
func layout(ctx context.Context, nc *nats.Conn, path string, o out) (int, error) {
	r, err := readRecord(path)
	if err != nil {
		return exitUnknown, err
	}
	var account struct {
		Streams int `json:"streams"`
	}
	if err := request(ctx, nc, apiPrefix+"INFO", nil, &account); err != nil {
		o.line("UNKNOWN JetStream does not answer in this account: %v", err)
		return o.verdict("LAYOUT APPLIED", 0, 1), nil
	}
	o.line("JETSTREAM answers in this account, holding %d streams", account.Streams)
	shut, unknown := 0, 0
	for _, f := range r.Streams {
		err := request(ctx, nc, apiPrefix+"STREAM.INFO."+f.Name, nil, nil)
		var api *apiError
		switch {
		case err == nil:
			o.line("GATE SHUT %s is already in this account: a restore would not land on an empty name", f.Name)
			shut++
		case errors.As(err, &api) && api.ErrCode == errStreamNotFound:
			o.line("EMPTY %s is not in this account yet", f.Name)
		default:
			o.line("UNKNOWN %s: %v", f.Name, err)
			unknown++
		}
	}
	return o.verdict("LAYOUT APPLIED", shut, unknown), nil
}

// restore restores each recorded stream from its snapshot into the account nc is in, with the
// seal lifted, and verifies it against the record. As the rollback, it first finds the stream the
// merge base's render brought back from its store, which must still be sealed, and deletes it: a
// stream that is not sealed may hold a write the record does not, and is left as it is.
func restore(ctx context.Context, nc *nats.Conn, path, dir string, rollback bool, o out) (int, error) {
	r, err := readRecord(path)
	if err != nil {
		return exitUnknown, err
	}
	word := "MOVED"
	if rollback {
		word = "ROLLED BACK"
	}
	shut, unknown := 0, 0
	for _, f := range r.Streams {
		if rollback {
			back, err := info(ctx, nc, f.Name)
			var api *apiError
			switch {
			case errors.As(err, &api) && api.ErrCode == errStreamNotFound:
				o.line("ABSENT %s is not in this account; it is restored from the snapshot", f.Name)
			case err != nil:
				o.line("UNKNOWN %s: %v", f.Name, err)
				unknown++
				continue
			default:
				config, err := back.config()
				if err != nil {
					o.line("UNKNOWN %s: %v", f.Name, err)
					unknown++
					continue
				}
				if !config.Sealed {
					o.line("GATE SHUT %s is not sealed: it may hold a write the record does not, so it is left as it is", f.Name)
					shut++
					continue
				}
				if err := request(ctx, nc, apiPrefix+"STREAM.DELETE."+f.Name, nil, nil); err != nil {
					o.line("UNKNOWN %s: the sealed stream could not be deleted: %v", f.Name, err)
					unknown++
					continue
				}
				o.line("DELETED %s, sealed as the fence left it", f.Name)
			}
		}
		if err := restoreSnapshot(ctx, nc, f.Name, dir); err != nil {
			o.line("UNKNOWN %s: the restore failed: %v", f.Name, err)
			unknown++
			continue
		}
		s, u := verifyOne(ctx, nc, f, word, o)
		shut, unknown = shut+s, unknown+u
	}
	return o.verdict(word, shut, unknown), nil
}

// verifyStep verifies every recorded stream where nc reads it.
func verifyStep(ctx context.Context, nc *nats.Conn, path string, o out) (int, error) {
	r, err := readRecord(path)
	if err != nil {
		return exitUnknown, err
	}
	shut, unknown := 0, 0
	for _, f := range r.Streams {
		s, u := verifyOne(ctx, nc, f, "VERIFIED", o)
		shut, unknown = shut+s, unknown+u
	}
	return o.verdict("VERIFIED", shut, unknown), nil
}

// verifyOne reads the stream as restored, unsealed, and holds it to the record: word and the
// stream's figures on equality, GATE SHUT naming the first difference otherwise.
func verifyOne(ctx context.Context, nc *nats.Conn, f fenced, word string, o out) (shut, unknown int) {
	i, err := info(ctx, nc, f.Name)
	if err != nil {
		o.line("UNKNOWN %s: %v", f.Name, err)
		return 0, 1
	}
	config, err := i.config()
	if err != nil {
		o.line("UNKNOWN %s: %v", f.Name, err)
		return 0, 1
	}
	if config.Sealed {
		o.line("GATE SHUT %s: it is still sealed, so it takes no write", f.Name)
		return 1, 0
	}
	consumers, err := durables(ctx, nc, f.Name)
	if err != nil {
		o.line("UNKNOWN %s: %v", f.Name, err)
		return 0, 1
	}
	now := fenced{
		Name: f.Name, StreamID: config.Metadata[bus.StreamIDMetadataKey],
		FirstSeq: i.State.FirstSeq, LastSeq: i.State.LastSeq, Msgs: i.State.Msgs,
		Subjects: i.State.Subjects, Consumers: consumers,
	}
	if diff := differs(f, now, true); diff != "" {
		o.line("GATE SHUT %s: %s", f.Name, diff)
		return 1, 0
	}
	o.line("%s %s first=%d last=%d messages=%d subjects=%d durable_consumers=%d stream_id=%s", word, f.Name,
		now.FirstSeq, now.LastSeq, now.Msgs, len(now.Subjects), len(now.Consumers), now.StreamID)
	return 0, 0
}

// differs names the first way now differs from the record's want, or "": its identity, its first
// and last sequence, its message count, each subject's count, and each recorded durable consumer,
// present with an ack floor at or past the record's. A consumer's floor may move between the fence
// and the snapshot, since a sealed stream still takes its consumers' acknowledgements; with
// restored set, it may not be past the stream's last sequence either.
func differs(want, now fenced, restored bool) string {
	switch {
	case now.StreamID != want.StreamID:
		return fmt.Sprintf("its %s is %q, the record's %q", bus.StreamIDMetadataKey, now.StreamID, want.StreamID)
	case now.FirstSeq != want.FirstSeq:
		return fmt.Sprintf("its first sequence is %d, the record's %d", now.FirstSeq, want.FirstSeq)
	case now.LastSeq != want.LastSeq:
		return fmt.Sprintf("its last sequence is %d, the record's %d", now.LastSeq, want.LastSeq)
	case now.Msgs != want.Msgs:
		return fmt.Sprintf("it holds %d messages, the record %d", now.Msgs, want.Msgs)
	}
	for _, subject := range slices.Sorted(maps.Keys(want.Subjects)) {
		if now.Subjects[subject] != want.Subjects[subject] {
			return fmt.Sprintf("subject %s holds %d messages, the record %d", subject, now.Subjects[subject], want.Subjects[subject])
		}
	}
	for _, subject := range slices.Sorted(maps.Keys(now.Subjects)) {
		if _, ok := want.Subjects[subject]; !ok {
			return fmt.Sprintf("subject %s holds %d messages the record does not", subject, now.Subjects[subject])
		}
	}
	floors := map[string]uint64{}
	for _, c := range now.Consumers {
		floors[c.Name] = c.AckFloor
	}
	for _, c := range want.Consumers {
		floor, ok := floors[c.Name]
		switch {
		case !ok:
			return fmt.Sprintf("its durable consumer %s is gone", c.Name)
		case floor < c.AckFloor:
			return fmt.Sprintf("its durable consumer %s's ack floor is %d, the record's %d", c.Name, floor, c.AckFloor)
		case restored && floor > now.LastSeq:
			return fmt.Sprintf("its durable consumer %s's ack floor %d is past the stream's last sequence %d", c.Name, floor, now.LastSeq)
		}
	}
	return ""
}

// switched reads each recorded stream as a client of ENVOY does, through the account's imports,
// once the broker places every client there: the stream answers, unsealed, with the record's
// identity and first sequence and at least its last, since writes resume at the switch.
func switched(ctx context.Context, nc *nats.Conn, path string, o out) (int, error) {
	r, err := readRecord(path)
	if err != nil {
		return exitUnknown, err
	}
	shut, unknown := 0, 0
	for _, f := range r.Streams {
		var i streamInfo
		if err := request(ctx, nc, apiPrefix+"STREAM.INFO."+f.Name, nil, &i); err != nil {
			o.line("UNKNOWN %s: %v", f.Name, err)
			unknown++
			continue
		}
		config, err := i.config()
		if err != nil {
			o.line("UNKNOWN %s: %v", f.Name, err)
			unknown++
			continue
		}
		switch {
		case config.Sealed:
			o.line("GATE SHUT %s: it is sealed, so every write to it is refused", f.Name)
			shut++
		case config.Metadata[bus.StreamIDMetadataKey] != f.StreamID:
			o.line("GATE SHUT %s: its %s is %q, the record's %q", f.Name, bus.StreamIDMetadataKey, config.Metadata[bus.StreamIDMetadataKey], f.StreamID)
			shut++
		case i.State.FirstSeq < f.FirstSeq:
			o.line("GATE SHUT %s: its first sequence is %d, before the record's %d", f.Name, i.State.FirstSeq, f.FirstSeq)
			shut++
		case i.State.LastSeq < f.LastSeq:
			o.line("GATE SHUT %s: its last sequence is %d, before the record's %d", f.Name, i.State.LastSeq, f.LastSeq)
			shut++
		default:
			o.line("SWITCHED %s first=%d last=%d messages=%d", f.Name, i.State.FirstSeq, i.State.LastSeq, i.State.Msgs)
		}
	}
	return o.verdict("SWITCHED", shut, unknown), nil
}
