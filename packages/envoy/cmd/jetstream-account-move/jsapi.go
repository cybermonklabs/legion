package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// The JetStream API calls this command makes, by their subjects, so a grant that names each call
// reads as the list of what the step does. Every answer is read for its own error first.
const apiPrefix = "$JS.API."

// apiError is the error a JetStream API answer carries.
type apiError struct {
	Code        int    `json:"code"`
	ErrCode     int    `json:"err_code"`
	Description string `json:"description"`
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (%d)", e.Description, e.ErrCode) }

// errStreamNotFound is nats-server's error code for a stream the account does not hold
// (JSStreamNotFoundErr).
const errStreamNotFound = 10059

// request sends body to the API subject and decodes the answer into into, returning the answer's
// own error when it carries one.
func request(ctx context.Context, nc *nats.Conn, subject string, body any, into any) error {
	return requestWithin(ctx, nc, subject, body, into, requestTimeout)
}

func requestWithin(ctx context.Context, nc *nats.Conn, subject string, body any, into any, timeout time.Duration) error {
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("%s: %w", subject, err)
	}
	return decodeAnswer(subject, msg.Data, into)
}

// decodeAnswer decodes data, a JetStream API answer, into into, or returns the error it carries.
func decodeAnswer(subject string, data []byte, into any) error {
	var answer struct {
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return fmt.Errorf("%s answered no JSON: %w", subject, err)
	}
	if answer.Error != nil {
		return fmt.Errorf("%s: %w", subject, answer.Error)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}

// streamState is the part of a stream's state this command reads.
type streamState struct {
	Msgs          uint64            `json:"messages"`
	FirstSeq      uint64            `json:"first_seq"`
	LastSeq       uint64            `json:"last_seq"`
	ConsumerCount int               `json:"consumer_count"`
	Subjects      map[string]uint64 `json:"subjects"`
}

// streamInfo is a STREAM.INFO answer: the configuration kept as the server wrote it, so a restore
// sends it back whole, and its state.
type streamInfo struct {
	Config json.RawMessage `json:"config"`
	State  streamState     `json:"state"`
	Total  int             `json:"total"`
	Offset int             `json:"offset"`
}

// streamConfig is the part of a configuration this command reads.
type streamConfig struct {
	Name     string            `json:"name"`
	Sealed   bool              `json:"sealed"`
	Metadata map[string]string `json:"metadata"`
}

func (i streamInfo) config() (streamConfig, error) {
	var c streamConfig
	err := json.Unmarshal(i.Config, &c)
	return c, err
}

// streamNames lists every stream of the account, page by page.
func streamNames(ctx context.Context, nc *nats.Conn) ([]string, error) {
	var names []string
	for {
		var page struct {
			Streams []string `json:"streams"`
			Total   int      `json:"total"`
		}
		if err := request(ctx, nc, apiPrefix+"STREAM.NAMES", map[string]int{"offset": len(names)}, &page); err != nil {
			return nil, err
		}
		names = append(names, page.Streams...)
		if len(page.Streams) == 0 || len(names) >= page.Total {
			return names, nil
		}
	}
}

// info reads the stream and every subject it holds with its count, page by page.
func info(ctx context.Context, nc *nats.Conn, stream string) (streamInfo, error) {
	var whole streamInfo
	subjects := map[string]uint64{}
	for {
		var page streamInfo
		if err := request(ctx, nc, apiPrefix+"STREAM.INFO."+stream, map[string]any{"subjects_filter": ">", "offset": len(subjects)}, &page); err != nil {
			return streamInfo{}, err
		}
		for subject, count := range page.State.Subjects {
			subjects[subject] = count
		}
		whole = page
		if len(page.State.Subjects) == 0 || len(subjects) >= page.Total {
			break
		}
	}
	whole.State.Subjects = subjects
	return whole, nil
}

// consumer is the part of a durable consumer's state the record keeps.
type consumer struct {
	Name     string `json:"name"`
	AckFloor uint64 `json:"ack_floor_stream_seq"`
}

// durables lists the stream's durable consumers with their ack floors, page by page. An ephemeral
// consumer (a KV watch's, an ordered consumer) is no reader's cursor, and is left out.
func durables(ctx context.Context, nc *nats.Conn, stream string) ([]consumer, error) {
	var found []consumer
	seen := 0
	for {
		var page struct {
			Consumers []struct {
				Name   string `json:"name"`
				Config struct {
					Durable string `json:"durable_name"`
				} `json:"config"`
				AckFloor struct {
					Stream uint64 `json:"stream_seq"`
				} `json:"ack_floor"`
			} `json:"consumers"`
			Total int `json:"total"`
		}
		if err := request(ctx, nc, apiPrefix+"CONSUMER.LIST."+stream, map[string]int{"offset": seen}, &page); err != nil {
			return nil, err
		}
		seen += len(page.Consumers)
		for _, c := range page.Consumers {
			if c.Config.Durable != "" {
				found = append(found, consumer{Name: c.Name, AckFloor: c.AckFloor.Stream})
			}
		}
		if len(page.Consumers) == 0 || seen >= page.Total {
			return found, nil
		}
	}
}

// seal sets the stream's configuration sealed, after which nats-server refuses every write to it.
func seal(ctx context.Context, nc *nats.Conn, stream string, config json.RawMessage) error {
	sealed, err := withSealed(config, true)
	if err != nil {
		return err
	}
	return request(ctx, nc, apiPrefix+"STREAM.UPDATE."+stream, sealed, nil)
}

// withSealed is config with its sealed field set to sealed, every other field as it was.
func withSealed(config json.RawMessage, sealed bool) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return nil, err
	}
	fields["sealed"] = json.RawMessage(fmt.Sprint(sealed))
	return json.Marshal(fields)
}

// The files a snapshot of one stream is kept in, under the snapshot directory's subdirectory named
// for the stream: the snapshot's bytes as the server sent them, and the configuration and state
// its answer named.
const (
	snapshotData = "stream.snapshot"
	snapshotMeta = "meta.json"
)

// snapshotMetaFile is what a snapshot's answer named: the configuration a restore sends back and
// the state it carried.
type snapshotMetaFile struct {
	Config json.RawMessage `json:"config"`
	State  json.RawMessage `json:"state"`
}

// takeSnapshot writes the stream's snapshot, its durable consumers included, into dir/<stream>:
// the server sends it in chunks to an inbox of this connection, each acknowledged at once, and
// ends it with an empty message, whose status says whether it ended whole (204). It returns the
// state the snapshot was taken at.
func takeSnapshot(ctx context.Context, nc *nats.Conn, stream, dir string) (streamState, int64, error) {
	into := filepath.Join(dir, stream)
	if err := os.MkdirAll(into, 0o700); err != nil {
		return streamState{}, 0, err
	}
	data, err := os.OpenFile(filepath.Join(into, snapshotData), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return streamState{}, 0, err
	}
	defer data.Close()
	inbox := nc.NewRespInbox()
	done := make(chan error, 1)
	var written int64
	sub, err := nc.Subscribe(inbox, func(m *nats.Msg) {
		if len(m.Data) == 0 {
			status := m.Header.Get("Status")
			if status != "" && !strings.HasPrefix(status, "204") {
				done <- fmt.Errorf("the snapshot ended %s %s", status, m.Header.Get("Description"))
				return
			}
			done <- nil
			return
		}
		if m.Reply != "" {
			_ = m.Respond(nil)
		}
		n, err := data.Write(m.Data)
		written += int64(n)
		if err != nil {
			done <- err
		}
	})
	if err != nil {
		return streamState{}, 0, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := sub.SetPendingLimits(-1, -1); err != nil {
		return streamState{}, 0, err
	}
	var answer struct {
		Config json.RawMessage `json:"config"`
		State  json.RawMessage `json:"state"`
	}
	request := map[string]any{"deliver_subject": inbox, "chunk_size": 128 * 1024, "jsck": true}
	if err := requestWithin(ctx, nc, apiPrefix+"STREAM.SNAPSHOT."+stream, request, &answer, requestTimeout); err != nil {
		return streamState{}, 0, err
	}
	select {
	case err := <-done:
		if err != nil {
			return streamState{}, 0, err
		}
	case <-time.After(restoreTimeout):
		return streamState{}, 0, errors.New("the snapshot did not end within " + restoreTimeout.String())
	case <-ctx.Done():
		return streamState{}, 0, ctx.Err()
	}
	if err := data.Sync(); err != nil {
		return streamState{}, 0, err
	}
	meta, err := json.Marshal(snapshotMetaFile{Config: answer.Config, State: answer.State})
	if err != nil {
		return streamState{}, 0, err
	}
	if err := os.WriteFile(filepath.Join(into, snapshotMeta), meta, 0o600); err != nil {
		return streamState{}, 0, err
	}
	var state streamState
	if err := json.Unmarshal(answer.State, &state); err != nil {
		return streamState{}, 0, err
	}
	return state, written, nil
}

// restoreSnapshot restores the stream from dir/<stream> into the account nc is in, with the seal
// lifted: the server answers the restore request with the subject the chunks go to, takes each
// chunk as a request it answers, and answers the empty one that ends them once it has written the
// stream.
func restoreSnapshot(ctx context.Context, nc *nats.Conn, stream, dir string) error {
	from := filepath.Join(dir, stream)
	raw, err := os.ReadFile(filepath.Join(from, snapshotMeta))
	if err != nil {
		return err
	}
	var meta snapshotMetaFile
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(from, snapshotMeta), err)
	}
	config, err := withSealed(meta.Config, false)
	if err != nil {
		return err
	}
	var answer struct {
		DeliverSubject string `json:"deliver_subject"`
	}
	body := map[string]json.RawMessage{"config": config, "state": meta.State}
	if err := request(ctx, nc, apiPrefix+"STREAM.RESTORE."+stream, body, &answer); err != nil {
		return err
	}
	data, err := os.Open(filepath.Join(from, snapshotData))
	if err != nil {
		return err
	}
	defer data.Close()
	chunk := make([]byte, min(int64(128*1024), nc.MaxPayload()))
	for {
		n, err := io.ReadFull(data, chunk)
		if n > 0 {
			reply, rerr := nc.Request(answer.DeliverSubject, bytes.Clone(chunk[:n]), requestTimeout)
			if rerr != nil {
				return fmt.Errorf("send a chunk of %s: %w", stream, rerr)
			}
			if len(reply.Data) > 0 {
				if derr := decodeAnswer(answer.DeliverSubject, reply.Data, nil); derr != nil {
					return derr
				}
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	final, err := nc.RequestWithContext(ctx, answer.DeliverSubject, nil)
	if err != nil {
		return fmt.Errorf("end the restore of %s: %w", stream, err)
	}
	return decodeAnswer(answer.DeliverSubject, final.Data, nil)
}
