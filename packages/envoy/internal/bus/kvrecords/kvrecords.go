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
// Every stored message is in the copy, whatever its key: the sentinel envoy.bucket, each hold's
// hold.barrier.<hold id>, each listener durable's record durable.<name>, and any other key. A
// restore writes back only ForRestore's entries.
package kvrecords

import (
	"bufio"
	"bytes"
	"context"
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
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Format is the copy's format, which its manifest names.
const Format = "envoy-kv-records-v1"

// The three files a copy's directory holds.
const (
	RecordsFile  = "records.jsonl"
	ManifestFile = "manifest.json"
	SumsFile     = "SHA256SUMS"
)

// noMessageFound is the err_code nats-server answers a STREAM.MSG.GET of a sequence it no longer
// stores with: a message deleted, replaced under a bucket's history, or rolled up by a purge.
const noMessageFound = 10037

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

// ForRestore is the records a restore writes back, in file order: every record but the system
// keys, SentinelKey and each HoldBarrierKeyPrefix key, which it skips by name. It keeps each
// DurableKeyPrefix key and every key holding no '.', and refuses any other key holding a '.',
// naming the key and its sequence: such a key may be a system key of a later build, and a restore
// that guessed could drop a record or write a stale marker.
func ForRestore(records []Record) ([]Record, error) {
	var restore []Record
	for _, record := range records {
		switch {
		case record.Key == SentinelKey || strings.HasPrefix(record.Key, HoldBarrierKeyPrefix):
			continue
		case strings.HasPrefix(record.Key, DurableKeyPrefix) || !strings.Contains(record.Key, "."):
			restore = append(restore, record)
		default:
			return nil, fmt.Errorf("kvrecords: sequence %d holds the key %q, a dotted key this format does not know; a restore neither writes nor skips it unasked",
				record.Seq, record.Key)
		}
	}
	return restore, nil
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
	// TakenAt is when the stream's info answered, in UTC. FirstSeq and LastSeq are its state then:
	// the copy is the stream as of LastSeq, and a message stored after the read is not in it.
	TakenAt  time.Time `json:"taken_at"`
	FirstSeq uint64    `json:"first_seq"`
	LastSeq  uint64    `json:"last_seq"`
	// Records is the number of lines of records.jsonl, and RecordsSHA256 its hex SHA-256, so the
	// manifest alone names the records file it describes.
	Records       int    `json:"records"`
	RecordsSHA256 string `json:"records_sha256"`
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

// recordJSON is a record's line as it is read.
type recordJSON struct {
	Seq     uint64          `json:"seq"`
	Subject string          `json:"subject"`
	Key     string          `json:"key"`
	Time    string          `json:"time"`
	Headers json.RawMessage `json:"headers"`
	Data    *string         `json:"data"`
}

// MarshalJSON writes the record's line: seq, subject, key, time, headers and data, in that order.
func (r Record) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	field := func(name string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%q:", name)
		out.Write(raw)
		return nil
	}
	for _, f := range []struct {
		name  string
		value any
	}{
		{"seq", r.Seq}, {"subject", r.Subject}, {"key", r.Key}, {"time", r.Time.UTC().Format(timeLayout)},
		{"headers", r.Headers}, {"data", base64.StdEncoding.EncodeToString(r.Data)},
	} {
		if err := field(f.name, f.value); err != nil {
			return nil, err
		}
	}
	return append(append([]byte("{"), out.Bytes()...), '}'), nil
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

// Write takes the copy of bucket's stream through nc into dir, which must not exist yet. It reads
// the stream's info once, then asks STREAM.MSG.GET for every sequence from first_seq to last_seq
// in order. A sequence answered 10037, no message found, is left out; any other error, or a
// message whose subject is not one of the bucket's keys, stops the walk, and nothing is written. The
// files are written into a directory of their own beside dir and moved to dir whole.
func Write(ctx context.Context, nc *nats.Conn, bucket, dir string) (Manifest, error) {
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("kvrecords: %s exists already, or cannot be read (%v); a copy is written into a new directory", dir, err)
	}
	stream := "KV_" + bucket
	prefix := "$KV." + bucket + "."
	var info struct {
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
	}
	refused, err := request(ctx, nc, "$JS.API.STREAM.INFO."+stream, nil, &info)
	if err != nil {
		return Manifest{}, err
	}
	if refused != nil {
		return Manifest{}, fmt.Errorf("kvrecords: the info of %s: %d %s", stream, refused.ErrCode, refused.Description)
	}
	manifest := Manifest{Format: Format, Bucket: bucket, Stream: stream, TakenAt: time.Now().UTC(),
		FirstSeq: info.State.FirstSeq, LastSeq: info.State.LastSeq}
	var records bytes.Buffer
	// A stream that never stored a message has last_seq 0, and sequences start at 1.
	for seq := max(manifest.FirstSeq, 1); seq <= manifest.LastSeq; seq++ {
		body, _ := json.Marshal(map[string]uint64{"seq": seq})
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
			return Manifest{}, err
		}
		if refused != nil {
			if refused.ErrCode == noMessageFound {
				continue
			}
			return Manifest{}, fmt.Errorf("kvrecords: %s sequence %d: %d %s", stream, seq, refused.ErrCode, refused.Description)
		}
		message := got.Message
		if message.Seq != seq {
			return Manifest{}, fmt.Errorf("kvrecords: %s sequence %d answered sequence %d", stream, seq, message.Seq)
		}
		key, ok := strings.CutPrefix(message.Subject, prefix)
		if !ok || key == "" {
			return Manifest{}, fmt.Errorf("kvrecords: %s sequence %d is stored under %q, which is no key of %s", stream, seq, message.Subject, bucket)
		}
		headers, err := parseBlock(message.Header)
		if err != nil {
			return Manifest{}, fmt.Errorf("kvrecords: %s sequence %d: %w", stream, seq, err)
		}
		line, err := json.Marshal(Record{Seq: seq, Subject: message.Subject, Key: key, Time: message.Time, Headers: headers, Data: message.Data})
		if err != nil {
			return Manifest{}, fmt.Errorf("kvrecords: %s sequence %d: %w", stream, seq, err)
		}
		records.Write(line)
		records.WriteByte('\n')
		manifest.Records++
	}
	recordsSum := sha256.Sum256(records.Bytes())
	manifest.RecordsSHA256 = hex.EncodeToString(recordsSum[:])
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifestJSON = append(manifestJSON, '\n')
	manifestSum := sha256.Sum256(manifestJSON)
	sums := fmt.Sprintf("%s  %s\n%s  %s\n", hex.EncodeToString(manifestSum[:]), ManifestFile, manifest.RecordsSHA256, RecordsFile)

	staging, err := os.MkdirTemp(filepath.Dir(dir), ".kvrecords-")
	if err != nil {
		return Manifest{}, fmt.Errorf("kvrecords: stage the copy beside %s: %w", dir, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	for _, file := range []struct {
		name string
		data []byte
	}{{RecordsFile, records.Bytes()}, {ManifestFile, manifestJSON}, {SumsFile, []byte(sums)}} {
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

// Read reads and checks the copy in dir, refusing, naming what failed: a missing file; a
// SHA256SUMS line that does not match its file; a format other than Format; a record count or
// records sum the manifest gives that disagrees with records.jsonl; a sequence that does not
// increase or falls outside first_seq..last_seq; a subject that is not one of the manifest's
// bucket's keys, or whose key disagrees with the record's key; and data that is not base64.
func Read(dir string) (Manifest, []Record, error) {
	files := map[string][]byte{}
	for _, name := range []string{SumsFile, ManifestFile, RecordsFile} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("kvrecords: read %s: %w", name, err)
		}
		files[name] = data
	}
	if err := checkSums(files); err != nil {
		return Manifest{}, nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(files[ManifestFile], &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s: %w", ManifestFile, err)
	}
	if manifest.Format != Format {
		return Manifest{}, nil, fmt.Errorf("kvrecords: %s names the format %q, not %q", ManifestFile, manifest.Format, Format)
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
	for line := 1; ; line++ {
		raw, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		var entry recordJSON
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("kvrecords: %s line %d: %w", RecordsFile, line, err)
		}
		if entry.Seq <= last || entry.Seq < manifest.FirstSeq || entry.Seq > manifest.LastSeq {
			return nil, fmt.Errorf("kvrecords: %s line %d holds sequence %d, after %d, outside %d..%d or not increasing",
				RecordsFile, line, entry.Seq, last, manifest.FirstSeq, manifest.LastSeq)
		}
		last = entry.Seq
		key, ok := strings.CutPrefix(entry.Subject, prefix)
		if !ok || key != entry.Key {
			return nil, fmt.Errorf("kvrecords: %s line %d's subject %q is not the key %q of %s", RecordsFile, line, entry.Subject, entry.Key, manifest.Bucket)
		}
		stored, err := time.Parse(time.RFC3339Nano, entry.Time)
		if err != nil {
			return nil, fmt.Errorf("kvrecords: %s line %d's time: %w", RecordsFile, line, err)
		}
		var headers Headers
		if err := headers.UnmarshalJSON(entry.Headers); err != nil {
			return nil, fmt.Errorf("kvrecords: %s line %d: %w", RecordsFile, line, err)
		}
		if entry.Data == nil {
			return nil, fmt.Errorf("kvrecords: %s line %d holds no data", RecordsFile, line)
		}
		payload, err := base64.StdEncoding.DecodeString(*entry.Data)
		if err != nil {
			return nil, fmt.Errorf("kvrecords: %s line %d's data is not base64: %w", RecordsFile, line, err)
		}
		records = append(records, Record{Seq: entry.Seq, Subject: entry.Subject, Key: key, Time: stored, Headers: headers, Data: payload})
	}
}
