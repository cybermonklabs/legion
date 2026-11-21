package claude

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// The files of a session export after `writing-check dataset sessions` joined it to the cases.
const (
	rowsFile   = "session-rows.jsonl.gz"
	placesFile = "places.jsonl"
	countsFile = "sessions-counts.json"
)

// PlaceFound is the status of a case whose writer session has rows before its write.
const PlaceFound = "found"

// Place is where a case's write falls in its writer session (places.jsonl): the case's session
// is the first Rows rows of SessionID.
type Place struct {
	CaseID    string `json:"case_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Rows      int    `json:"rows"`
}

// ReadPlaces reads the places of every case of the export in dir.
func ReadPlaces(dir string) (map[string]Place, error) {
	data, err := os.ReadFile(filepath.Join(dir, placesFile))
	if err != nil {
		return nil, err
	}
	out := map[string]Place{}
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p Place
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", placesFile, i+1, err)
		}
		out[p.CaseID] = p
	}
	return out, nil
}

// The roles of the rows a session part keeps: what the people and agents said, what the
// writer did and saw, and a compaction's summary. Notices a plugin adds to a turn are left out.
var keptRoles = map[string]bool{
	"user": true, "assistant": true, "toolResult": true, "compaction": true,
	"custom:envoy-message": true, "custom:irc:incoming": true, "custom:async-result": true,
}

// rowLimit is the most bytes of one row a session part keeps, from the row's start.
const rowLimit = 2000

// cutMark stands where a part was cut.
const cutMark = "[…]"

// sessionRow is the part of one line of session-rows.jsonl.gz an excerpt reads.
type sessionRow struct {
	SessionID string `json:"session_id"`
	Index     int    `json:"index"`
	Row       struct {
		TS   string `json:"ts"`
		Role string `json:"role"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"row"`
}

// keep is whether a row goes into a session part: a kept role, and for the assistant its replies
// and not its thinking.
func (r *sessionRow) keep() bool {
	return keptRoles[r.Row.Role] && !(r.Row.Role == "assistant" && r.Row.Kind == "thinking") && strings.TrimSpace(r.Row.Text) != ""
}

// render is the row as a session part shows it.
func (r *sessionRow) render() string {
	return "[" + r.Row.TS + " " + r.Row.Role + "]\n" + headBytes(r.Row.Text, rowLimit) + "\n"
}

// headBytes is the first limit bytes of s, cut at a character boundary, and a mark where it cut.
func headBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + cutMark
}

// tailBytes is the last limit bytes of s, cut at a character boundary, with a mark where it cut.
func tailBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := len(s) - limit
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return cutMark + s[i:]
}

// tail is one session's newest kept rows while the export is read.
type tail struct {
	rows  []string
	bytes int
	last  string
	// wants are the cases of the session not yet cut, by their row count, fewest first.
	wants []Place
}

func (t *tail) add(row string, limit int) {
	if row == t.last {
		// The export holds a row once for each host that logged it.
		return
	}
	t.last = row
	t.rows = append(t.rows, row)
	t.bytes += len(row)
	for len(t.rows) > 1 && t.bytes-len(t.rows[0]) >= limit {
		t.bytes -= len(t.rows[0])
		t.rows = t.rows[1:]
	}
}

func (t *tail) text(limit int) string {
	return tailBytes(strings.Join(t.rows, ""), limit)
}

// Excerpts reads the session export in dir once and returns, for each case of cases whose place
// is found, the newest limit bytes of its session's kept rows before its place. A case with no
// place found has no entry.
func Excerpts(dir string, cases []string, limit int) (map[string]SessionText, error) {
	if limit <= 0 {
		return nil, errors.New("a session part's limit must be above 0")
	}
	places, err := ReadPlaces(dir)
	if err != nil {
		return nil, err
	}
	tails := map[string]*tail{}
	for _, id := range cases {
		p, ok := places[id]
		if !ok || p.Status != PlaceFound || p.Rows <= 0 {
			continue
		}
		t := tails[p.SessionID]
		if t == nil {
			t = &tail{}
			tails[p.SessionID] = t
		}
		t.wants = append(t.wants, p)
	}
	for _, t := range tails {
		slices.SortFunc(t.wants, func(a, b Place) int { return a.Rows - b.Rows })
	}
	out := make(map[string]SessionText, len(cases))
	if len(tails) == 0 {
		return out, nil
	}
	f, err := os.Open(filepath.Join(dir, rowsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(gz, 1<<20)
	prefix := []byte(`{"session_id":"`)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			// `dataset sessions` writes the session id first, so a row of a session no case
			// reads is skipped before it is decoded; a line in another order is decoded.
			if rest, ok := bytes.CutPrefix(line, prefix); ok {
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					if t := tails[string(rest[:end])]; t == nil || len(t.wants) == 0 {
						line = nil
					}
				}
			}
			if line != nil {
				var row sessionRow
				if err := json.Unmarshal(line, &row); err != nil {
					return nil, fmt.Errorf("%s: %w", rowsFile, err)
				}
				if t := tails[row.SessionID]; t != nil && len(t.wants) > 0 {
					if row.keep() {
						t.add(row.render(), limit)
					}
					for len(t.wants) > 0 && t.wants[0].Rows <= row.Index+1 {
						out[t.wants[0].CaseID] = SessionText{text: t.text(limit)}
						t.wants = t.wants[1:]
					}
					if len(t.wants) == 0 {
						t.rows, t.last = nil, ""
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	for _, t := range tails {
		if len(t.wants) > 0 {
			return nil, fmt.Errorf("%s holds fewer rows of session %s than case %s reads", rowsFile, t.wants[0].SessionID, t.wants[0].CaseID)
		}
	}
	return out, nil
}

// excerptHeader is the first line of an excerpt cache: what the excerpts were cut from, and
// every case they were cut for, with a session or without one.
type excerptHeader struct {
	Format int      `json:"format"`
	Limit  int      `json:"limit"`
	Rows   string   `json:"rows_sha256"`
	Places string   `json:"places_sha256"`
	Cases  []string `json:"cases"`
}

// covers is whether a cache under h serves a read under want: the same export and limit, and
// every case want asks for.
func (h excerptHeader) covers(want excerptHeader) bool {
	if h.Format != want.Format || h.Limit != want.Limit || h.Rows != want.Rows || h.Places != want.Places {
		return false
	}
	have := make(map[string]bool, len(h.Cases))
	for _, id := range h.Cases {
		have[id] = true
	}
	for _, id := range want.Cases {
		if !have[id] {
			return false
		}
	}
	return true
}

type excerptLine struct {
	CaseID string `json:"case_id"`
	Text   string `json:"text"`
}

// exportDigests are the SHA-256 of the export's rows and places, as sessions-counts.json
// records them.
func exportDigests(dir string) (rows, places string, err error) {
	data, err := os.ReadFile(filepath.Join(dir, countsFile))
	if err != nil {
		return "", "", err
	}
	var counts struct {
		SHA256 map[string]string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		return "", "", fmt.Errorf("%s: %w", countsFile, err)
	}
	return counts.SHA256[rowsFile], counts.SHA256[placesFile], nil
}

// CachedExcerpts is Excerpts, kept in cache (a gzipped JSONL file under the run's output
// directory): a cache cut from the same export at the same limit for every case asked for is
// read in place of the export, and otherwise the export is read and the cache written again.
func CachedExcerpts(dir string, cases []string, limit int, cache string) (map[string]SessionText, error) {
	rows, places, err := exportDigests(dir)
	if err != nil {
		return nil, err
	}
	header := excerptHeader{Format: 1, Limit: limit, Rows: rows, Places: places, Cases: slices.Sorted(slices.Values(cases))}
	if out, ok := readCache(cache, header); ok {
		return out, nil
	}
	out, err := Excerpts(dir, cases, limit)
	if err != nil {
		return nil, err
	}
	return out, writeCache(cache, header, out)
}

// readCache is the excerpts of the cache at path when its header covers want.
func readCache(path string, want excerptHeader) (map[string]SessionText, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(gz)
	var got excerptHeader
	if dec.Decode(&got) != nil || !got.covers(want) {
		return nil, false
	}
	asked := make(map[string]bool, len(want.Cases))
	for _, id := range want.Cases {
		asked[id] = true
	}
	out := map[string]SessionText{}
	for {
		var line excerptLine
		if err := dec.Decode(&line); err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, false
			}
			return out, true
		}
		if asked[line.CaseID] {
			out[line.CaseID] = SessionText{text: line.Text}
		}
	}
}

// writeCache writes excerpts to path under header, by a file beside it renamed over it.
func writeCache(path string, header excerptHeader, excerpts map[string]SessionText) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	err = enc.Encode(header)
	for _, id := range slices.Sorted(maps.Keys(excerpts)) {
		if err != nil {
			break
		}
		err = enc.Encode(excerptLine{CaseID: id, Text: excerpts[id].text})
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
