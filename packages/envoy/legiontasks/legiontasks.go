// Package legiontasks names the key-value bucket that carries each Legion claim's current task,
// and what is written in it, for the programs that share it: the Legion daemon, which writes it,
// and the Envoy listener, which reads it and relays each value to the session holding the claim's
// role. The NATS grants that let each of them reach the bucket (daemonpermissions and
// envoy-infra's core) are built from these names too, so the bucket, its keys and the relay's
// headers are spelled in one place.
//
// The bucket is the delivered copy of the daemon's record of each claim's current task, not a
// second record: the daemon's store stays the truth, the daemon is the bucket's one writer, and it
// writes a key again from its record whenever the two differ. One bucket serves every project. A
// key is a claim's token (`legion-<project>-<issue>-<n>-<role>`), so a daemon tells its own
// project's keys by their prefix (OwnedBy). A value is the number of the claim's current task
// (Value): a worker reads the number from its value, then asks its daemon for the task's text.
//
// A revision of the bucket restarts from 1 when the bucket is created again, so a revision is
// read with the bucket's epoch, the creation time of its stream (Epoch).
package legiontasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Bucket is the key-value bucket of every claim's current task.
const Bucket = "legion_tasks"

// Stream is the JetStream stream behind Bucket, as NATS names a bucket's stream.
const Stream = "KV_" + Bucket

// The headers of a relay frame, the core message the listener publishes on a session's relay
// subject for each put and delete of the key of the claim whose role that session holds. The
// frame's data is the value's bytes as the bucket holds them, empty for a delete.
const (
	// HeaderClaim is the key the frame relays, the claim's token.
	HeaderClaim = "Envoy-Task-Claim"
	// HeaderEpoch is the bucket's epoch the revision belongs to (Epoch).
	HeaderEpoch = "Envoy-Task-Epoch"
	// HeaderRevision is the key's revision in the bucket, in decimal.
	HeaderRevision = "Envoy-Task-Revision"
	// HeaderOp is what happened to the key at that revision: OpPut or OpDelete.
	HeaderOp = "Envoy-Task-Op"
)

// The values of HeaderOp.
const (
	// OpPut is a write of the key's value.
	OpPut = "put"
	// OpDelete is a delete of the key: the claim has no current task.
	OpDelete = "delete"
)

// Epoch is the bucket's epoch as every program spells it: created, the creation time of Stream
// (its stream info's Created), in UTC and RFC 3339 with nanoseconds. A key's revision names one
// write only within one epoch, since the bucket created again counts its revisions from 1, so a
// revision is carried, reported and compared with its epoch.
func Epoch(created time.Time) string {
	return created.UTC().Format(time.RFC3339Nano)
}

// OwnedBy reports whether key is a key of the project whose project token is projectToken: it has
// the prefix `legion-<projectToken>-`. A project token is lowercase letters and digits alone
// (ProjectToken in packages/daemon/internal/claim, legionProjectToken in packages/contracts), so
// it holds no hyphen, and the hyphen after it ends the project's part of every claim token: the
// prefix of project `ab` begins no key of project `abc`, and the prefix of `abc` begins no key of
// `ab`. A projectToken that is not a project token, empty or holding any other character, owns no
// key, since such a prefix could reach into another project's keys: a token `a-b` would own
// project `a`'s claims on issues `B-<n>`.
func OwnedBy(key, projectToken string) bool {
	if projectToken == "" {
		return false
	}
	for _, r := range projectToken {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return strings.HasPrefix(key, "legion-"+projectToken+"-")
}

// Value is what the bucket holds for a claim: the number of its current task, which the daemon's
// record assigned and which is never 0. Its encoding is exactly `{"task":<number>}`.
type Value struct {
	Task uint64
}

// Encode is v as the bucket holds it, `{"task":<number>}`. A Value whose Task is 0 is refused: no
// task is numbered 0.
func (v Value) Encode() ([]byte, error) {
	if v.Task == 0 {
		return nil, errors.New("legiontasks: a task value's number is 0")
	}
	return append(strconv.AppendUint([]byte(`{"task":`), v.Task, 10), '}'), nil
}

// Decode reads a value the bucket holds. It accepts exactly one JSON object holding one member,
// "task", whose value is a positive integer that fits a uint64, with nothing after the object,
// and refuses anything else: a value that is not JSON, another member beside "task" or in its
// place (a member name is matched exactly, case included), a repeated "task", a number that is 0,
// negative, fractional or written with an exponent, and a "task" that is a string, null or any
// other JSON type. JSON's insignificant whitespace is allowed anywhere it is.
func Decode(data []byte) (Value, error) {
	refuse := func(why string) (Value, error) {
		return Value{}, fmt.Errorf("legiontasks: %q is not a task value: %s", data, why)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	// next is the decoder's next token, or the error that ended the read, which is no token the
	// checks below accept.
	next := func() json.Token {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		return token
	}
	if next() != json.Delim('{') {
		return refuse(`want an object, {"task":<number>}`)
	}
	if name := next(); name != "task" {
		return refuse(`want the one member "task"`)
	}
	number, ok := next().(json.Number)
	if !ok {
		return refuse("want a positive integer")
	}
	task, err := strconv.ParseUint(number.String(), 10, 64)
	if err != nil || task == 0 {
		return refuse("want a positive integer")
	}
	if next() != json.Delim('}') {
		return refuse(`want the one member "task"`)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return refuse("want nothing after the object")
	}
	return Value{Task: task}, nil
}
