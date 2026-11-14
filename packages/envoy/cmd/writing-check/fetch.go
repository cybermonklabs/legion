package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"
)

// pageSize is the events one read of the log asks for.
const pageSize = 500

func runFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check fetch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	after := flags.Int64("after", 0, "read the events after this event id")
	through := flags.Int64("through", 0, "read the events up to and including this event id (required)")
	out := flags.String("out", "", "the cases file, JSONL (required; keep it under $TMPDIR)")
	resume := flags.Bool("resume", false, "continue the cases file after the last event it holds, rather than start it again")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "writing-check fetch: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	base, tokenFile := os.Getenv("DISPATCH_URL"), os.Getenv("DISPATCH_TOKEN_FILE")
	switch {
	case *out == "":
		fmt.Fprintln(stderr, "writing-check fetch: --out is required")
		return 2
	case *through <= *after:
		fmt.Fprintln(stderr, "writing-check fetch: --through must be above --after")
		return 2
	case base == "" || tokenFile == "":
		fmt.Fprintln(stderr, "writing-check fetch: DISPATCH_URL and DISPATCH_TOKEN_FILE must be set")
		return 2
	}
	c := newClient(base, tokenFile)
	x := newExtractor(newLiveSource(c))
	start := *after
	var file *os.File
	var err error
	if *resume {
		file, start, err = resumeCases(*out, *after, x)
	} else {
		file, err = os.Create(*out)
	}
	if err != nil {
		fmt.Fprintf(stderr, "writing-check fetch: %v\n", err)
		return 1
	}
	defer file.Close()
	stats, err := fetchCases(ctx, c, x, start, *through, file, stderr)
	writeFetchStats(stdout, stderr, stats, x.unread, c.requests)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check fetch: %v\n", err)
		fmt.Fprintf(stderr, "writing-check fetch: rerun with --resume to continue after event %d\n", stats.last)
		return 1
	}
	return 0
}

// fetchStats counts what one fetch read and wrote.
type fetchStats struct {
	events   int
	last     int64
	surfaces map[string]int
	exempt   map[string]int
	exemptBy map[string]int
}

// fetchCases pages the log from after through through and writes each event's cases to w as
// one write, so an interrupted run leaves whole events behind it.
func fetchCases(ctx context.Context, c *client, x *extractor, after, through int64, w io.Writer, progress io.Writer) (fetchStats, error) {
	stats := fetchStats{last: after, surfaces: map[string]int{}, exempt: map[string]int{}, exemptBy: map[string]int{}}
	cursor := after
	lastReport := time.Now()
	for cursor < through {
		limit := min(int64(pageSize), through-cursor)
		var page []event
		query := url.Values{"after": {strconv.FormatInt(cursor, 10)}, "limit": {strconv.FormatInt(limit, 10)}}
		if err := c.get(ctx, "/api/v1/events/log", query, &page); err != nil {
			return stats, fmt.Errorf("reading the log after %d: %w", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			if ev.ID > through {
				return stats, nil
			}
			cases, exemption, err := x.cases(ctx, ev)
			if err != nil {
				return stats, err
			}
			if exemption != "" {
				stats.exempt[exemption]++
				stats.exemptBy[ev.Type+" "+exemption]++
			}
			var buf bytes.Buffer
			for _, cs := range cases {
				line, err := json.Marshal(cs)
				if err != nil {
					return stats, err
				}
				buf.Write(line)
				buf.WriteByte('\n')
				stats.surfaces[cs.Surface]++
			}
			if buf.Len() > 0 {
				if _, err := w.Write(buf.Bytes()); err != nil {
					return stats, err
				}
			}
			stats.events++
			stats.last = ev.ID
			cursor = ev.ID
		}
		if time.Since(lastReport) > 10*time.Second {
			fmt.Fprintf(progress, "writing-check fetch: through event %d, %d events read\n", cursor, stats.events)
			lastReport = time.Now()
		}
	}
	return stats, nil
}

// writeFetchStats prints the cases by surface on stdout, and on stderr the writes the actor
// rule exempted and the fields Dispatch would not serve whole (unread, by surface).
func writeFetchStats(stdout, stderr io.Writer, stats fetchStats, unread map[string]int, requests int) {
	total := 0
	fmt.Fprintf(stdout, "events read: %d (through event %d, %d requests)\n", stats.events, stats.last, requests)
	fmt.Fprintln(stdout, "cases by surface:")
	for _, s := range sortedKeys(stats.surfaces) {
		fmt.Fprintf(stdout, "  %s: %d\n", s, stats.surfaces[s])
		total += stats.surfaces[s]
	}
	fmt.Fprintf(stdout, "cases: %d\n", total)
	fmt.Fprintln(stderr, "exempted writes:")
	for _, e := range sortedKeys(stats.exempt) {
		fmt.Fprintf(stderr, "  %s: %d\n", e, stats.exempt[e])
	}
	fmt.Fprintln(stderr, "exempted writes by event type:")
	for _, e := range sortedKeys(stats.exemptBy) {
		fmt.Fprintf(stderr, "  %s: %d\n", e, stats.exemptBy[e])
	}
	fmt.Fprintln(stderr, "fields not served whole (kept as the event carried them, or dropped when it carried none):")
	for _, s := range sortedKeys(unread) {
		fmt.Fprintf(stderr, "  %s: %d\n", s, unread[s])
	}
}

// resumeCases opens the cases file at path to continue it. A line a crash cut is dropped, and so
// are the cases of the last event the file holds, which may be only some of them: the fetch
// reads that event again. It answers the file, open for appending, and the event id to read
// after. The broadcasts the file already took are marked taken in x.
func resumeCases(path string, after int64, x *extractor) (*os.File, int64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		f, err := os.Create(path)
		return f, after, err
	}
	if err != nil {
		return nil, 0, err
	}
	if cut := bytes.LastIndexByte(data, '\n'); cut+1 != len(data) {
		data = data[:cut+1]
	}
	type line struct {
		end       int
		event     int64
		broadcast string
	}
	var lines []line
	var last int64
	offset := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for scanner.Scan() {
		offset += len(scanner.Bytes()) + 1
		var cs Case
		if err := json.Unmarshal(scanner.Bytes(), &cs); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
		l := line{end: offset, event: cs.EventID}
		if cs.Surface == surfaceBroadcast {
			l.broadcast = cs.SourceID
		}
		lines = append(lines, l)
		last = max(last, cs.EventID)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	keep := 0
	for _, l := range lines {
		if l.event == last {
			continue
		}
		keep = max(keep, l.end)
		if l.broadcast != "" {
			x.broadcasts[l.broadcast] = true
		}
	}
	if len(lines) == 0 {
		last = after + 1
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, err
	}
	if err := f.Truncate(int64(keep)); err != nil {
		f.Close()
		return nil, 0, err
	}
	if _, err := f.Seek(int64(keep), io.SeekStart); err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, max(last-1, after), nil
}

// liveSource is the reads extraction makes of the live Dispatch API.
type liveSource struct {
	c       *client
	results map[string]map[string]checkResult
}

func newLiveSource(c *client) *liveSource {
	return &liveSource{c: c, results: map[string]map[string]checkResult{}}
}

// commentBody is a comment's whole body: GET /api/v1/comments/{id}.
func (s *liveSource) commentBody(ctx context.Context, id string) (string, error) {
	var read struct {
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
	}
	err := s.c.get(ctx, "/api/v1/comments/"+url.PathEscape(id), nil, &read)
	return read.Comment.Body, err
}

// messageBody is a message's whole body: GET /api/v1/messages/{id}, which answers its whole
// conversation. A credential without the person scope reads a direct conversation only naming
// a session in it, so each session given is tried in turn after the plain read.
func (s *liveSource) messageBody(ctx context.Context, id string, sessions []string) (string, error) {
	type message struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	var read struct {
		Message message   `json:"message"`
		Replies []message `json:"replies"`
	}
	queries := []url.Values{nil}
	for _, session := range sessions {
		if session != "" {
			queries = append(queries, url.Values{"session": {session}})
		}
	}
	var err error
	for _, q := range queries {
		if err = s.c.get(ctx, "/api/v1/messages/"+url.PathEscape(id), q, &read); err == nil {
			break
		}
		var status *statusError
		if !errors.As(err, &status) || status.status != 403 {
			return "", err
		}
	}
	if err != nil {
		return "", err
	}
	for _, m := range append([]message{read.Message}, read.Replies...) {
		if m.ID == id {
			return m.Body, nil
		}
	}
	return "", fmt.Errorf("message %s is not in the conversation its read answered: %w", id, errNotFound)
}

// issueParent is an issue's parent key, "" for a root: GET /api/v1/issues/{key}.
func (s *liveSource) issueParent(ctx context.Context, key string) (string, error) {
	var issue struct {
		Parent *string `json:"parent"`
	}
	err := s.c.get(ctx, "/api/v1/issues/"+url.PathEscape(key), nil, &issue)
	return deref(issue.Parent), err
}

// documentVersion is a document version's Markdown: GET /api/v1/artifacts/{id}/versions/{n}.
func (s *liveSource) documentVersion(ctx context.Context, artifactID string, number int) (string, error) {
	var version struct {
		Markdown string `json:"markdown"`
	}
	err := s.c.get(ctx, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/versions/"+strconv.Itoa(number), nil, &version)
	return version.Markdown, err
}

// result is one recorded result's prose. Dispatch serves results by the checks they answer,
// GET /api/v1/issues/{key}/results on the check's issue, so the read is kept per issue and made
// again only for a result it does not hold yet.
func (s *liveSource) result(ctx context.Context, checkIssue, resultID string) (checkResult, error) {
	if r, ok := s.results[checkIssue][resultID]; ok {
		return r, nil
	}
	var read struct {
		Checks []struct {
			Results []struct {
				ID string `json:"id"`
				checkResult
			} `json:"results"`
		} `json:"checks"`
	}
	if err := s.c.get(ctx, "/api/v1/issues/"+url.PathEscape(checkIssue)+"/results", nil, &read); err != nil {
		return checkResult{}, err
	}
	byID := map[string]checkResult{}
	for _, check := range read.Checks {
		for _, r := range check.Results {
			byID[r.ID] = r.checkResult
		}
	}
	s.results[checkIssue] = byID
	r, ok := byID[resultID]
	if !ok {
		return checkResult{}, fmt.Errorf("result %s is not among %s's results: %w", resultID, checkIssue, errNotFound)
	}
	return r, nil
}
