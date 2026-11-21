// Package kvrecords takes and reads a records-only copy of one KV bucket's stream (KV_<bucket>),
// format envoy-kv-records-v1: every stored message read one by one with STREAM.MSG.GET, so the
// copy needs no grant that writes. The operator takes it outside a hold under
// natsgrant.Operator(OperatorPhaseReleased), whose STREAM.INFO and STREAM.MSG.GET of
// KV_envoy_session_consumers are the only calls it makes; consumer-names create-bucket --after-loss
// --backup reads it.
//
// It is not a snapshot. Each record keeps the message's sequence, subject, key, time, headers and
// data, so the bucket's keys, values, delete markers and purge markers survive, but a restore
// writes each message again, and the restored stream's sequences, first_seq and last_seq start
// over: it cannot reproduce last_seq. A restore that needs the exact last_seq takes a STREAM.SNAPSHOT
// under a hold (envoy-infra buckets backup), since a snapshot delivers its chunks to a subject its
// requester chooses, which outside a hold could be any protected key.
//
// The copy is signed with the operator's hold key (signedrecord.RecordsManifest): the manifest
// names the records file's SHA-256, and the signature covers the manifest, so a copy that anyone
// who can write its directory rewrites, all three files agreeing with each other, does not read.
//
// Every stored message is in the copy, whatever its key: the sentinel envoy.bucket, each hold's
// hold.barrier.<hold id>, each listener durable's record durable.<name>, and any other key. A
// restore writes back only ForRestore's entries.
package kvrecords

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
)

// Format is the copy's format, which its manifest names.
const Format = "envoy-kv-records-v1"

// The four files a copy's directory holds.
const (
	RecordsFile   = "records.jsonl"
	ManifestFile  = "manifest.json"
	SumsFile      = "SHA256SUMS"
	SignatureFile = "manifest.sig"
)

// noMessageFound is the err_code nats-server answers a STREAM.MSG.GET of a sequence it no longer
// stores with: a message deleted, replaced under a bucket's history, or rolled up by a purge.
const noMessageFound = 10037

// The walk's bounds. A pass walks to the last_seq the stream's info last answered; a put to an
// existing key during it stores the key again above that last_seq and removes its old message, so
// the walk reads the info again and walks on, until a pass ends with the last_seq it began with.
// A bucket written faster than the walk reads it never gets there: the walk stops after
// maxPasses passes, or once its passes after the first have taken maxCatchUp, and fails naming the
// bound, writing nothing.
const (
	maxPasses  = 10
	maxCatchUp = 30 * time.Second
)

// The keys ForRestore knows by name. SentinelKey and HoldBarrierKeyPrefix are the bucket's system
// keys, which a restore skips: the bucket's re-creation writes its own sentinel, and a barrier key
// belongs to its hold alone. DurableKeyPrefix begins each listener durable's record, which a
// restore keeps: consumernames.DurableKey makes those keys ("durable." and the escaped name), and
// this is its prefix, named here since consumernames keeps its own unexported.
const (
	SentinelKey          = "envoy.bucket"
	HoldBarrierKeyPrefix = "hold.barrier."
	DurableKeyPrefix     = "durable."
)

// ForRestore is the records a restore writes back, in file order, each with only the headers a
// fresh stream takes (restorable). It skips every key skip names, which the restore's caller
// chooses, and the system keys, SentinelKey and each HoldBarrierKeyPrefix key, by name. It keeps
// each DurableKeyPrefix key and every key holding no '.', and refuses any other key holding a '.',
// naming the key and its sequence: such a key may be a system key of a later build, and a restore
// that guessed could drop a record or write a stale marker. Any client that may send a JetStream
// request can have the server store its answer under a key of its choosing, a dotted one among
// them, so the caller that finds one names it in skip rather than lose the whole restore. It
// decodes no value: each reader of the restored bucket decodes its values strictly and fails
// closed on one that is not an exact record of its key's kind.
func ForRestore(records []Record, skip []string) ([]Record, error) {
	var restore []Record
	for _, record := range records {
		switch {
		case slices.Contains(skip, record.Key):
			continue
		case record.Key == SentinelKey || strings.HasPrefix(record.Key, HoldBarrierKeyPrefix):
			continue
		case strings.HasPrefix(record.Key, DurableKeyPrefix) || !strings.Contains(record.Key, "."):
			record.Headers = restorable(record.Headers)
			restore = append(restore, record)
		default:
			return nil, fmt.Errorf("kvrecords: sequence %d holds the key %q, a dotted key this format does not know; a restore neither writes nor skips it unless its caller names it to skip",
				record.Seq, record.Key)
		}
	}
	return restore, nil
}

// restorable is headers without those that bind a message to the stream it was first written to,
// each matched by name whatever its case: every Nats-Expected-* header, which a write with a
// revision sends (kv.Create, kv.Update, a delete at a revision) and the server stores, and which a
// fresh stream refuses as "wrong last sequence"; Nats-Msg-Id, a duplicate window's id; and every
// Nats-Batch-* header, an atomic batch's. KV-Operation, Nats-Rollup and every other header are
// kept, in their order. headers is not changed.
func restorable(headers Headers) Headers {
	kept := Headers{}
	for _, header := range headers {
		name := strings.ToLower(header.Name)
		if strings.HasPrefix(name, "nats-expected-") || name == "nats-msg-id" || strings.HasPrefix(name, "nats-batch-") {
			continue
		}
		kept = append(kept, header)
	}
	return kept
}

// requestTimeout bounds each of Write's requests, inside its caller's context.
const requestTimeout = 10 * time.Second

// timeLayout is how a record writes its time: RFC 3339 in UTC with all nine digits of nanoseconds.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Manifest is a copy's facts, its manifest.json.
type Manifest struct {
	// Format is Format.
	Format string `json:"format"`
	// Bucket is the KV bucket and Stream its stream, KV_<bucket>.
	Bucket string `json:"bucket"`
	Stream string `json:"stream"`
	// TakenAt is when the stream's info first answered, in UTC, and FirstSeq its first_seq then.
	// LastSeq is the last_seq the walk ended at, the one the stream's info answered twice in a row:
	// the copy is the stream as of LastSeq, and a message stored after it is not in it.
	TakenAt  time.Time `json:"taken_at"`
	FirstSeq uint64    `json:"first_seq"`
	LastSeq  uint64    `json:"last_seq"`
	// Records is the number of lines of records.jsonl, and RecordsSHA256 its hex SHA-256, so the
	// manifest alone names the records file it describes, and its signature covers the records.
	Records       int    `json:"records"`
	RecordsSHA256 string `json:"records_sha256"`
	// Foreign is each stored message whose subject is no key of the bucket, by sequence and
	// subject, with no data: it has no record, and Read and ForRestore ignore it. A consumer's
	// delivery to a reply or deliver subject the bucket's stream captures is stored under the
	// delivered message's own subject (nats-server v2.15.1 stream.go:8620-8621, client.go:5358-5365),
	// and any grant that may pull from ENVOY_NOTIFICATIONS (Daemon's) can send one, so a walk that
	// stopped on it would let that grant block every copy. It is [] when there is none, and the
	// manifest's signature covers it.
	Foreign []ForeignMessage `json:"foreign"`
}

// ForeignMessage is a stored message of the bucket's stream whose subject is no key of the bucket.
type ForeignMessage struct {
	Seq     uint64 `json:"seq"`
	Subject string `json:"subject"`
}

// Header is one NATS header of a stored message: its name, as the server sent it, and its values
// in order.
type Header struct {
	Name   string
	Values []string
}

// Headers are a message's headers in the order its header block holds them. A record writes them
// as a JSON object of name to values in that order, {} for none.
type Headers []Header

// Record is one stored message of the bucket's stream, one line of records.jsonl.
type Record struct {
	// Seq is the message's stream sequence, strictly increasing down the file.
	Seq uint64
	// Subject is the stored subject, $KV.<bucket>.<key>, and Key that subject without its prefix.
	Subject, Key string
	// Time is when the server stored the message.
	Time time.Time
	// Headers are the message's NATS headers: a delete marker's KV-Operation: DEL, a purge marker's
	// KV-Operation: PURGE beside Nats-Rollup: sub.
	Headers Headers
	// Data is the payload.
	Data []byte
}

// line is a record's line of records.jsonl, both as Write writes it and as Read reads it:
// encoding/json writes its fields in this order, with no space between them, its time as
// timeLayout writes it and its data as standard base64 with padding.
type line struct {
	Seq     uint64  `json:"seq"`
	Subject string  `json:"subject"`
	Key     string  `json:"key"`
	Time    string  `json:"time"`
	Headers Headers `json:"headers"`
	Data    string  `json:"data"`
}

// encodeLine is record's line, with no line feed.
func encodeLine(record Record) ([]byte, error) {
	headers := record.Headers
	if headers == nil {
		headers = Headers{}
	}
	return json.Marshal(line{
		Seq: record.Seq, Subject: record.Subject, Key: record.Key, Time: record.Time.UTC().Format(timeLayout),
		Headers: headers, Data: base64.StdEncoding.EncodeToString(record.Data),
	})
}

// lineFields are the names of line's six fields, each of which a line must hold, and hold as other
// than null.
var lineFields = []string{"seq", "subject", "key", "time", "headers", "data"}

// decodeLine is the record raw, one line of records.jsonl, holds, its time and data decoded and
// nothing else checked. It refuses a line that leaves out one of lineFields, or gives it as null,
// naming the field: decoded into line alone, such a line would read as the field's zero, a line
// with no data as an empty value.
func decodeLine(raw []byte) (Record, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Record{}, err
	}
	for _, name := range lineFields {
		if value, ok := fields[name]; !ok || string(value) == "null" {
			return Record{}, fmt.Errorf("it holds no %q", name)
		}
	}
	var entry line
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Record{}, err
	}
	stored, err := time.Parse(time.RFC3339Nano, entry.Time)
	if err != nil {
		return Record{}, fmt.Errorf("its time: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(entry.Data)
	if err != nil {
		return Record{}, fmt.Errorf("its data is not base64: %w", err)
	}
	return Record{Seq: entry.Seq, Subject: entry.Subject, Key: entry.Key, Time: stored, Headers: entry.Headers, Data: payload}, nil
}

// MarshalJSON writes the headers as an object of name to values, in their order.
func (h Headers) MarshalJSON() ([]byte, error) {
	out := []byte{'{'}
	for index, header := range h {
		name, err := json.Marshal(header.Name)
		if err != nil {
			return nil, err
		}
		values, err := json.Marshal(header.Values)
		if err != nil {
			return nil, err
		}
		if index > 0 {
			out = append(out, ',')
		}
		out = append(append(append(out, name...), ':'), values...)
	}
	return append(out, '}'), nil
}

// UnmarshalJSON reads the headers object, keeping its order, and refuses a name given twice or a
// value list that is not a list of strings.
func (h *Headers) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("the headers are not a JSON object")
	}
	headers := Headers{}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, _ := token.(string)
		if seen[name] {
			return fmt.Errorf("the header %q is given twice", name)
		}
		seen[name] = true
		var values []string
		if err := decoder.Decode(&values); err != nil {
			return fmt.Errorf("the header %q's values are not a list of strings: %w", name, err)
		}
		headers = append(headers, Header{Name: name, Values: values})
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	*h = headers
	return nil
}

// Block is the headers as a NATS header block, NATS/1.0 and each "name: value" line, or nil for
// none: the bytes a message carrying them is stored with.
func (h Headers) Block() []byte {
	if len(h) == 0 {
		return nil
	}
	var out bytes.Buffer
	out.WriteString("NATS/1.0\r\n")
	for _, header := range h {
		for _, value := range header.Values {
			fmt.Fprintf(&out, "%s: %s\r\n", header.Name, value)
		}
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

// parseBlock reads a stored message's header block: its version line, then each "name: value"
// line, its names kept as written and grouped in the order each first appears.
func parseBlock(block []byte) (Headers, error) {
	headers := Headers{}
	if len(block) == 0 {
		return headers, nil
	}
	lines := strings.Split(string(block), "\r\n")
	if !strings.HasPrefix(lines[0], "NATS/1.0") {
		return nil, fmt.Errorf("the header block opens %q, not NATS/1.0", lines[0])
	}
	index := map[string]int{}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("the header line %q holds no name", line)
		}
		value = strings.TrimSpace(value)
		if at, seen := index[name]; seen {
			headers[at].Values = append(headers[at].Values, value)
			continue
		}
		index[name] = len(headers)
		headers = append(headers, Header{Name: name, Values: []string{value}})
	}
	return headers, nil
}

// apiError is a JetStream API answer's error.
type apiError struct {
	Code        int    `json:"code"`
	ErrCode     int    `json:"err_code"`
	Description string `json:"description"`
}

// request asks subject with body through nc within requestTimeout, and decodes the answer into out,
// returning its error when it carries one.
func request(ctx context.Context, nc *nats.Conn, subject string, body []byte, out any) (*apiError, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, subject, body)
	if err != nil {
		return nil, fmt.Errorf("kvrecords: %s: %w", subject, err)
	}
	var answer struct {
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &answer); err != nil {
		return nil, fmt.Errorf("kvrecords: %s answered no JSON: %w", subject, err)
	}
	if answer.Error != nil {
		return answer.Error, nil
	}
	if err := json.Unmarshal(msg.Data, out); err != nil {
		return nil, fmt.Errorf("kvrecords: %s: %w", subject, err)
	}
	return nil, nil
}

// streamState is the first_seq and last_seq the stream's info answers now.
func streamState(ctx context.Context, nc *nats.Conn, stream string) (first, last uint64, err error) {
	var info struct {
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
	}
	refused, err := request(ctx, nc, "$JS.API.STREAM.INFO."+stream, nil, &info)
	if err != nil {
		return 0, 0, err
	}
	if refused != nil {
		return 0, 0, fmt.Errorf("kvrecords: the info of %s: %d %s", stream, refused.ErrCode, refused.Description)
	}
	return info.State.FirstSeq, info.State.LastSeq, nil
}

// Write takes the copy of bucket's stream through nc into dir, which must not exist yet, and signs
// it with key, the operator's hold signing key. It reads the stream's info, asks STREAM.MSG.GET for
// every sequence from first_seq to last_seq in order, then reads the info again and walks on to
// the new last_seq, until a pass ends at the last_seq it began with, within the walk's bounds. A
// sequence answered 10037, no message found, is left out, and a message whose subject is no key of
// the bucket is listed in the manifest's Foreign and has no record; any other error, or a walk past
// its bounds, stops the walk, and nothing is written. The files are written into a directory of
// their own beside dir and moved to dir whole.
func Write(ctx context.Context, nc *nats.Conn, bucket, dir string, key ed25519.PrivateKey) (Manifest, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Manifest{}, fmt.Errorf("kvrecords: the signing key holds %d bytes, not an Ed25519 private key's %d", len(key), ed25519.PrivateKeySize)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("kvrecords: %s exists already, or cannot be read (%v); a copy is written into a new directory", dir, err)
	}
	stream := "KV_" + bucket
	first, last, err := streamState(ctx, nc, stream)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{Format: Format, Bucket: bucket, Stream: stream, TakenAt: time.Now().UTC(), FirstSeq: first, LastSeq: last,
		Foreign: []ForeignMessage{}}
	records, err := walk(ctx, nc, &manifest)
	if err != nil {
		return Manifest{}, err
	}
	recordsSum := sha256.Sum256(records)
	manifest.RecordsSHA256 = hex.EncodeToString(recordsSum[:])
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifestJSON = append(manifestJSON, '\n')
	manifestSum := sha256.Sum256(manifestJSON)
	sums := fmt.Sprintf("%s  %s\n%s  %s\n", hex.EncodeToString(manifestSum[:]), ManifestFile, manifest.RecordsSHA256, RecordsFile)
	sig, err := signedrecord.Sign(key, signedrecord.RecordsManifest(manifestJSON))
	if err != nil {
		return Manifest{}, fmt.Errorf("kvrecords: sign the manifest: %w", err)
	}

	staging, err := os.MkdirTemp(filepath.Dir(dir), ".kvrecords-")
	if err != nil {
		return Manifest{}, fmt.Errorf("kvrecords: stage the copy beside %s: %w", dir, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	for _, file := range []struct {
		name string
		data []byte
	}{{RecordsFile, records}, {ManifestFile, manifestJSON}, {SumsFile, []byte(sums)}, {SignatureFile, []byte(sig + "\n")}} {
		if err := os.WriteFile(filepath.Join(staging, file.name), file.data, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("kvrecords: write %s: %w", file.name, err)
		}
	}
	// A rename replaces an empty directory, so dir is checked once more just before it.
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("kvrecords: %s appeared while the copy was taken", dir)
	}
	if err := os.Rename(staging, dir); err != nil {
		return Manifest{}, fmt.Errorf("kvrecords: move the copy to %s: %w", dir, err)
	}
	return manifest, nil
}

// walk is records.jsonl's bytes for manifest's stream, walked from its FirstSeq to the last_seq
// the walk ends at, which it writes into manifest.LastSeq with the number of records.
func walk(ctx context.Context, nc *nats.Conn, manifest *Manifest) ([]byte, error) {
	stream := manifest.Stream
	prefix := "$KV." + manifest.Bucket + "."
	var records bytes.Buffer
	// A stream that never stored a message has last_seq 0, and sequences start at 1.
	next := max(manifest.FirstSeq, 1)
	var caughtUp time.Time
	for pass := 1; ; pass++ {
		for ; next <= manifest.LastSeq; next++ {
			body, _ := json.Marshal(map[string]uint64{"seq": next})
			var got struct {
				Message struct {
					Subject string    `json:"subject"`
					Seq     uint64    `json:"seq"`
					Header  []byte    `json:"hdrs"`
					Data    []byte    `json:"data"`
					Time    time.Time `json:"time"`
				} `json:"message"`
			}
			refused, err := request(ctx, nc, "$JS.API.STREAM.MSG.GET."+stream, body, &got)
			if err != nil {
				return nil, err
			}
			if refused != nil {
				if refused.ErrCode == noMessageFound {
					continue
				}
				return nil, fmt.Errorf("kvrecords: %s sequence %d: %d %s", stream, next, refused.ErrCode, refused.Description)
			}
			message := got.Message
			if message.Seq != next {
				return nil, fmt.Errorf("kvrecords: %s sequence %d answered sequence %d", stream, next, message.Seq)
			}
			key, ok := strings.CutPrefix(message.Subject, prefix)
			if !ok || key == "" {
				manifest.Foreign = append(manifest.Foreign, ForeignMessage{Seq: next, Subject: message.Subject})
				continue
			}
			headers, err := parseBlock(message.Header)
			if err != nil {
				return nil, fmt.Errorf("kvrecords: %s sequence %d: %w", stream, next, err)
			}
			encoded, err := encodeLine(Record{Seq: next, Subject: message.Subject, Key: key, Time: message.Time, Headers: headers, Data: message.Data})
			if err != nil {
				return nil, fmt.Errorf("kvrecords: %s sequence %d: %w", stream, next, err)
			}
			records.Write(encoded)
			records.WriteByte('\n')
			manifest.Records++
		}
		_, last, err := streamState(ctx, nc, stream)
		if err != nil {
			return nil, err
		}
		if last <= manifest.LastSeq {
			return records.Bytes(), nil
		}
		if pass == 1 {
			caughtUp = time.Now()
		}
		if pass >= maxPasses {
			return nil, fmt.Errorf("kvrecords: %s's last_seq still moved after %d passes, the walk's bound (from %d to %d): the bucket is written faster than it is walked",
				stream, maxPasses, manifest.LastSeq, last)
		}
		if spent := time.Since(caughtUp); spent > maxCatchUp {
			return nil, fmt.Errorf("kvrecords: %s's last_seq still moved after %s of passes past the first, the walk's bound of %s (from %d to %d): the bucket is written faster than it is walked",
				stream, spent.Round(time.Second), maxCatchUp, manifest.LastSeq, last)
		}
		manifest.LastSeq = last
	}
}

// Read reads and checks the copy in dir against keys, the operator's public hold keys, refusing,
// naming what failed: a missing file; a SHA256SUMS line that does not match its file; a signature
// no key of keys verifies over the manifest; a format other than Format; a stream other than the
// bucket's, KV_<bucket>; a record count or records sum the manifest gives that disagrees with
// records.jsonl; a sequence that does not increase or falls outside first_seq..last_seq; a subject
// that is not one of the manifest's bucket's keys, or whose key disagrees with the record's key; an
// empty key; and data that is not base64.
func Read(dir string, keys []ed25519.PublicKey) (Manifest, []Record, error) {
	files := map[string][]byte{}
	for _, name := range []string{SumsFile, ManifestFile, RecordsFile, SignatureFile} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("kvrecords: read %s: %w", name, err)
		}
		files[name] = data
	}
	if err := checkSums(files); err != nil {
		return Manifest{}, nil, err
	}
	sig := strings.TrimSuffix(string(files[SignatureFile]), "\n")
	if err := signedrecord.Verify(keys, sig, signedrecord.RecordsManifest(files[ManifestFile])); err != nil {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s does not verify %s with any of the %d operator keys: %w", SignatureFile, ManifestFile, len(keys), err)
	}
	var manifest Manifest
	if err := json.Unmarshal(files[ManifestFile], &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s: %w", ManifestFile, err)
	}
	if manifest.Format != Format {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s names the format %q, not %q", ManifestFile, manifest.Format, Format)
	}
	if manifest.Bucket == "" || manifest.Stream != "KV_"+manifest.Bucket {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s names the stream %q, not %q, the stream of the bucket %q", ManifestFile, manifest.Stream, "KV_"+manifest.Bucket, manifest.Bucket)
	}
	recordsSum := sha256.Sum256(files[RecordsFile])
	if got := hex.EncodeToString(recordsSum[:]); got != manifest.RecordsSHA256 {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s gives records_sha256 %s, and %s's sum is %s", ManifestFile, manifest.RecordsSHA256, RecordsFile, got)
	}
	records, err := readRecords(files[RecordsFile], manifest)
	if err != nil {
		return Manifest{}, nil, err
	}
	if len(records) != manifest.Records {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s gives %d records, and %s holds %d", ManifestFile, manifest.Records, RecordsFile, len(records))
	}
	return manifest, records, nil
}

// checkSums holds SHA256SUMS to its two lines, manifest.json's then records.jsonl's, each naming
// its file's sum.
func checkSums(files map[string][]byte) error {
	lines := strings.Split(strings.TrimSuffix(string(files[SumsFile]), "\n"), "\n")
	want := []string{ManifestFile, RecordsFile}
	if len(lines) != len(want) {
		return fmt.Errorf("kvrecords: %s holds %d lines, not one for each of %s", SumsFile, len(lines), strings.Join(want, " and "))
	}
	for index, line := range lines {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || name != want[index] {
			return fmt.Errorf("kvrecords: %s line %d is %q, not the sum of %s", SumsFile, index+1, line, want[index])
		}
		got := sha256.Sum256(files[name])
		if hex.EncodeToString(got[:]) != sum {
			return fmt.Errorf("kvrecords: %s names %s for %s, whose sum is %s", SumsFile, sum, name, hex.EncodeToString(got[:]))
		}
	}
	return nil
}

// readRecords reads records.jsonl's lines, each ending in a line feed, holding each record to the
// manifest's sequences and bucket.
func readRecords(data []byte, manifest Manifest) ([]Record, error) {
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("kvrecords: %s's last line ends in no line feed", RecordsFile)
	}
	prefix := "$KV." + manifest.Bucket + "."
	var records []Record
	var last uint64
	reader := bufio.NewReader(bytes.NewReader(data))
	for number := 1; ; number++ {
		raw, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		record, err := decodeLine(raw)
		if err != nil {
			return nil, fmt.Errorf("kvrecords: %s line %d: %w", RecordsFile, number, err)
		}
		if record.Seq <= last || record.Seq < manifest.FirstSeq || record.Seq > manifest.LastSeq {
			return nil, fmt.Errorf("kvrecords: %s line %d holds sequence %d, after %d, outside %d..%d or not increasing",
				RecordsFile, number, record.Seq, last, manifest.FirstSeq, manifest.LastSeq)
		}
		last = record.Seq
		if record.Key == "" {
			return nil, fmt.Errorf("kvrecords: %s line %d holds an empty key, which no stored message has", RecordsFile, number)
		}
		key, ok := strings.CutPrefix(record.Subject, prefix)
		if !ok || key != record.Key {
			return nil, fmt.Errorf("kvrecords: %s line %d's subject %q is not the key %q of %s", RecordsFile, number, record.Subject, record.Key, manifest.Bucket)
		}
		records = append(records, record)
	}
}
