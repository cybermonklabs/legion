// Command timelinepages reads one project's merge timeline a page at a time
// (GET /api/v1/projects/{key}/timeline, dispatch://INFRA-2918, D8) and, with -compare, compares
// the union of its pages with the base head's unpaged answer for the same window
// (GET /api/v1/delivery/timeline at legion/INFRA-978 82380570f8, which evalbench's job.sh base
// captures over the one-project fixture as base-timeline.json), every field but measures, which
// the project timeline does not carry (spec R16). It is check k22's command:
//
//	go run ./internal/dispatch/evals/evalbench/timelinepages --url <server> --token-file <file> \
//	  --project <KEY> --window 84d --limit 200 --compare base-timeline.json
//
// The window is the --window days ending at --to (RFC 3339), by default the window the capture
// is of, and with no capture the current UTC midnight. It reads offset 0, limit, 2×limit and on
// until the pages hold the window's total, and judges:
//
//   - the pages: each answers the same total, each row once, newest first in the route's one
//     total order (time, then merge, deploy, failed_run, then id) across every page, and at least
//     three pages, since fewer exercise no paging;
//   - with --compare, the rows: the union holds every row of the base's prs[] (the merges, by id)
//     and runs[] (the deploys and failed runs, by id) and no other, each row with every field the
//     base's row has, equal, and beyond them only its kind (a merge's "merge", a run's "deploy" or
//     "failed_run") and a run's repo and created_at;
//   - with --compare, the members beside the rows: the window, the facet and colour counts and
//     the freshness equal the base's on every page; the issue titles, session titles, session
//     pull-request counts, components and session groups equal the base's on the first page and
//     are null on every other; waiting equals the base's on the last page and is null on every
//     other; and no other member of the base's answer but measures goes unread.
//
// It prints a RESULT line for each page and one for the whole read, a FAIL line for each
// difference (the first 50 named, the rest counted), and exits 1 on any.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The members of a page beside its rows, by where they are carried (projectTimelineResponse).
var (
	everyPage    = []string{"window", "facet_counts", "color_counts", "freshness"}
	firstPage    = []string{"issue_titles", "session_titles", "session_pr_counts", "components", "session_groups"}
	lastPage     = []string{"waiting"}
	envelope     = []string{"items", "total", "limit", "offset"}
	notCompared  = []string{"measures"}
	baseRowLists = []string{"prs", "runs"}
)

// The kinds of row, in the order the rows of one moment are listed, and the fields a head row
// carries beyond its base row.
var (
	kinds     = []string{"merge", "deploy", "failed_run"}
	rowExtras = map[string][]string{"merge": {"kind"}, "run": {"kind", "repo", "created_at"}}
)

// maxNamed is how many differences are printed whole.
const maxNamed = 50

type options struct {
	url, tokenFile, project, window, compare, to string
	limit                                        int
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "", "the server's base URL (required)")
	flag.StringVar(&o.tokenFile, "token-file", "", "a file holding the bearer token the reads send (required)")
	flag.StringVar(&o.project, "project", "", "the project whose timeline is read (required)")
	flag.StringVar(&o.window, "window", "84d", "the window's length in whole days, 1d to 84d")
	flag.IntVar(&o.limit, "limit", 200, "the rows a page asks for, 1 to 500")
	flag.StringVar(&o.compare, "compare", "", "the base's GET /api/v1/delivery/timeline answer for the window, to compare the union with")
	flag.StringVar(&o.to, "to", "", "the window's end, RFC 3339; the capture's window end, else the current UTC midnight")
	flag.Parse()
	if o.url == "" || o.tokenFile == "" || o.project == "" {
		fmt.Fprintln(os.Stderr, "usage: timelinepages --url <server> --token-file <file> --project <KEY> [--window 84d] [--limit 200] [--compare base-timeline.json] [--to <RFC 3339>]")
		os.Exit(2)
	}
	r := &report{}
	if err := run(context.Background(), r, o); err != nil {
		r.fail("%v", err)
	}
	if r.failed > maxNamed {
		fmt.Printf("FAIL k22 %d more differences not named\n", r.failed-maxNamed)
	}
	if r.failed > 0 {
		os.Exit(1)
	}
}

// report prints the run's lines and counts its failures.
type report struct{ failed int }

func (r *report) result(format string, args ...any) {
	fmt.Printf("RESULT k22 "+format+"\n", args...)
}

func (r *report) fail(format string, args ...any) {
	r.failed++
	if r.failed <= maxNamed {
		fmt.Printf("FAIL k22 "+format+"\n", args...)
	}
}

// page is one page as the route answered it: its members by name, and its rows.
type page struct {
	members map[string]json.RawMessage
	items   []map[string]json.RawMessage
	total   int
}

func run(ctx context.Context, r *report, o options) error {
	token, err := os.ReadFile(o.tokenFile)
	if err != nil {
		return fmt.Errorf("read the token file: %w", err)
	}
	days, err := strconv.Atoi(strings.TrimSuffix(o.window, "d"))
	if err != nil || !strings.HasSuffix(o.window, "d") || days < 1 || days > 84 {
		return fmt.Errorf("--window is a whole number of days from 1d to 84d, not %q", o.window)
	}
	if o.limit < 1 || o.limit > 500 {
		return fmt.Errorf("--limit is 1 to 500, not %d", o.limit)
	}
	var base map[string]json.RawMessage
	if o.compare != "" {
		if base, err = readJSON(o.compare); err != nil {
			return fmt.Errorf("read the capture %s: %w", o.compare, err)
		}
	}
	to := time.Now().UTC().Truncate(24 * time.Hour)
	switch {
	case o.to != "":
		if to, err = time.Parse(time.RFC3339, o.to); err != nil {
			return fmt.Errorf("read --to %q as RFC 3339: %w", o.to, err)
		}
	case base != nil:
		var window struct{ From, To time.Time }
		if err := json.Unmarshal(base["window"], &window); err != nil || window.To.IsZero() {
			return fmt.Errorf("the capture %s names no window (%v), so name --to", o.compare, err)
		}
		if span := window.To.Sub(window.From); span != time.Duration(days)*24*time.Hour {
			return fmt.Errorf("the capture %s is of %s..%s, %s, not %s: name --to and a --window that read what it holds", o.compare,
				window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), span, o.window)
		}
		to = window.To
	}
	from := to.AddDate(0, 0, -days)
	client := &http.Client{Timeout: 5 * time.Minute}
	var pages []page
	for offset := 0; ; offset += o.limit {
		query := url.Values{
			"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)},
			"limit": {strconv.Itoa(o.limit)}, "offset": {strconv.Itoa(offset)},
		}
		target := strings.TrimSuffix(o.url, "/") + "/api/v1/projects/" + url.PathEscape(o.project) + "/timeline?" + query.Encode()
		started := time.Now()
		one, size, err := readPage(ctx, client, target, strings.TrimSpace(string(token)))
		if err != nil {
			return fmt.Errorf("read the page at offset %d: %w", offset, err)
		}
		merges, runs := countKinds(one.items)
		r.result("page=%d offset=%d rows=%d merges=%d runs=%d total=%d bytes=%d ms=%d", len(pages)+1, offset, len(one.items), merges, runs, one.total,
			size, time.Since(started).Milliseconds())
		pages = append(pages, one)
		if len(pages) > 1 && one.total != pages[0].total {
			r.fail("page %d answers a total of %d, page 1 %d: the window moved while it was read", len(pages), one.total, pages[0].total)
		}
		if offset+o.limit >= one.total || len(one.items) == 0 {
			break
		}
	}
	rows := judgePages(r, o, pages)
	merges, runs := countKinds(rows)
	compared := 0
	if base != nil {
		compared = compareWithBase(r, base, pages, rows)
	}
	r.result("project=%s window=%s..%s limit=%d pages=%d rows=%d merges=%d runs=%d total=%d compared_with=%q fields_compared=%d differences=%d",
		o.project, from.Format(time.RFC3339), to.Format(time.RFC3339), o.limit, len(pages), len(rows), merges, runs, pages[0].total,
		o.compare, compared, r.failed)
	return nil
}

// readJSON reads a file of one JSON object, its members by name.
func readJSON(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// readPage is one GET of a page and the bytes its body held.
func readPage(ctx context.Context, client *http.Client, target, token string) (page, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return page{}, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return page{}, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return page{}, 0, err
	}
	if response.StatusCode != http.StatusOK {
		return page{}, len(body), fmt.Errorf("%s answered %d: %.300s", target, response.StatusCode, body)
	}
	var one page
	if err := json.Unmarshal(body, &one.members); err != nil {
		return page{}, len(body), fmt.Errorf("read the page: %w", err)
	}
	if err := json.Unmarshal(one.members["items"], &one.items); err != nil {
		return page{}, len(body), fmt.Errorf("read the page's items: %w", err)
	}
	if err := json.Unmarshal(one.members["total"], &one.total); err != nil {
		return page{}, len(body), fmt.Errorf("read the page's total: %w", err)
	}
	return one, len(body), nil
}

// kindOf is a row's kind, "" where it names none the route lists.
func kindOf(row map[string]json.RawMessage) string {
	var kind string
	if json.Unmarshal(row["kind"], &kind) != nil || !slices.Contains(kinds, kind) {
		return ""
	}
	return kind
}

// countKinds is how many of rows are merges and how many runs.
func countKinds(rows []map[string]json.RawMessage) (merges, runs int) {
	for _, row := range rows {
		if kindOf(row) == "merge" {
			merges++
		} else {
			runs++
		}
	}
	return merges, runs
}

// rowKey is a row's place in the route's one total order: its time (a merge's merged_at, a run's
// created_at), its kind's place, and its id, a merge's as it is and a run's as a number.
type rowKey struct {
	at    time.Time
	kind  int
	merge string
	run   int64
}

func keyOf(row map[string]json.RawMessage) (rowKey, error) {
	kind := kindOf(row)
	if kind == "" {
		return rowKey{}, fmt.Errorf("a row names no kind the route lists: %.200s", row["kind"])
	}
	key := rowKey{kind: slices.Index(kinds, kind)}
	at := "created_at"
	if kind == "merge" {
		at = "merged_at"
		if err := json.Unmarshal(row["id"], &key.merge); err != nil {
			return rowKey{}, fmt.Errorf("a merge's id: %w", err)
		}
	} else if err := json.Unmarshal(row["id"], &key.run); err != nil {
		return rowKey{}, fmt.Errorf("a run's id: %w", err)
	}
	if err := json.Unmarshal(row[at], &key.at); err != nil {
		return rowKey{}, fmt.Errorf("a %s's %s: %w", kind, at, err)
	}
	return key, nil
}

// compareKeys is the route's order: newest first, then by kind, then by id.
func compareKeys(a, b rowKey) int {
	return cmp.Or(b.at.Compare(a.at), cmp.Compare(a.kind, b.kind), strings.Compare(a.merge, b.merge), cmp.Compare(a.run, b.run))
}

// id is a row's id as the base's lists key it: a merge's string, a run's number in decimal.
func id(row map[string]json.RawMessage) string {
	var text string
	if json.Unmarshal(row["id"], &text) == nil {
		return text
	}
	return string(row["id"])
}

// judgePages judges the pages as pages (the package comment) and answers their rows in order.
func judgePages(r *report, o options, pages []page) []map[string]json.RawMessage {
	var rows []map[string]json.RawMessage
	seen := map[string]int{}
	var previous *rowKey
	for number, one := range pages {
		for _, row := range one.items {
			key, err := keyOf(row)
			if err != nil {
				r.fail("page %d: %v", number+1, err)
				continue
			}
			if previous != nil && compareKeys(*previous, key) >= 0 {
				r.fail("page %d holds the %s %s at %s after a row at %s: the rows are not newest first in the route's one order", number+1,
					kinds[key.kind], id(row), key.at.Format(time.RFC3339Nano), previous.at.Format(time.RFC3339Nano))
			}
			previous = &key
			name := kinds[key.kind] + " " + id(row)
			if first, held := seen[name]; held {
				r.fail("the %s is on page %d and again on page %d", name, first, number+1)
				continue
			}
			seen[name] = number + 1
			rows = append(rows, row)
		}
	}
	if len(rows) != pages[0].total {
		r.fail("the pages hold %d distinct rows, and the route answers a total of %d", len(rows), pages[0].total)
	}
	if len(pages) < 3 {
		r.fail("the window's %d rows fill %d pages of %d, and fewer than three pages exercise no paging", pages[0].total, len(pages), o.limit)
	}
	return rows
}

// canonical is value written again with its objects' keys in order, so two encodings of one value
// compare equal as text.
func canonical(value json.RawMessage) string {
	if len(value) == 0 {
		return "<absent>"
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	written, err := json.Marshal(decoded)
	if err != nil {
		return "<unwritable: " + err.Error() + ">"
	}
	return string(written)
}

// clip is text cut to a readable length for a FAIL line.
func clip(text string) string {
	if len(text) > 240 {
		return text[:240] + "…"
	}
	return text
}

// compareWithBase compares the pages' rows and members with the base's answer (the package
// comment), and answers how many fields it compared.
func compareWithBase(r *report, base map[string]json.RawMessage, pages []page, rows []map[string]json.RawMessage) int {
	compared := 0
	head := map[string]map[string]json.RawMessage{}
	for _, row := range rows {
		list := "runs"
		if kindOf(row) == "merge" {
			list = "prs"
		}
		head[list+" "+id(row)] = row
	}
	matched := map[string]bool{}
	for _, list := range baseRowLists {
		var baseRows []map[string]json.RawMessage
		if err := json.Unmarshal(base[list], &baseRows); err != nil {
			r.fail("the capture's %s is no list of rows: %v", list, err)
			continue
		}
		extras := rowExtras["run"]
		if list == "prs" {
			extras = rowExtras["merge"]
		}
		for _, baseRow := range baseRows {
			name := list + " " + id(baseRow)
			row, held := head[name]
			if !held {
				r.fail("the base's %s %s is on no page", list, id(baseRow))
				continue
			}
			matched[name] = true
			for field, value := range baseRow {
				compared++
				if got, want := canonical(row[field]), canonical(value); got != want {
					r.fail("%s %s: %s is %s, and the base's %s", list, id(baseRow), field, clip(got), clip(want))
				}
			}
			for field := range row {
				if _, inBase := baseRow[field]; !inBase && !slices.Contains(extras, field) {
					r.fail("%s %s carries %s, which the base's row does not", list, id(baseRow), field)
				}
			}
			if kind := kindOf(row); (list == "prs") != (kind == "merge") {
				r.fail("%s %s is listed as a %s", list, id(baseRow), kind)
			}
		}
	}
	for name := range head {
		if !matched[name] {
			r.fail("the head's %s is in no list of the base's answer", name)
		}
	}
	member := func(number int, name string) json.RawMessage {
		if value, held := pages[number].members[name]; held {
			return value
		}
		return nil
	}
	last := len(pages) - 1
	for _, name := range everyPage {
		for number := range pages {
			compared++
			if got, want := canonical(member(number, name)), canonical(base[name]); got != want {
				r.fail("page %d's %s is %s, and the base's %s", number+1, name, clip(got), clip(want))
			}
		}
	}
	for _, carried := range []struct {
		names []string
		on    int
	}{{firstPage, 0}, {lastPage, last}} {
		for _, name := range carried.names {
			for number := range pages {
				compared++
				want := "null"
				if number == carried.on {
					want = canonical(base[name])
				}
				if got := canonical(member(number, name)); got != want {
					r.fail("page %d's %s is %s, and %s is due there", number+1, name, clip(got), clip(want))
				}
			}
		}
	}
	read := slices.Concat(everyPage, firstPage, lastPage, notCompared, baseRowLists)
	for name := range base {
		if !slices.Contains(read, name) {
			r.fail("the base's answer holds %s, which no page member is compared with", name)
		}
	}
	for name := range pages[0].members {
		if !slices.Contains(read, name) && !slices.Contains(envelope, name) {
			r.fail("the pages carry %s, which the base's answer does not", name)
		}
	}
	return compared
}
