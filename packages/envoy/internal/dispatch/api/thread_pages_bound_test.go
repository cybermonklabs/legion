package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// R7's measurement: the seven first pages Threads reads, on the issue that holds production's
// largest counts together, and All's three on the issue that holds production's heaviest first
// pages. The seed is api/thread_pages_bound_seed_test.go's.
const (
	boundWarmups     = 5
	boundRuns        = 30
	boundPageRows    = 20
	boundPageBound   = 200 * time.Millisecond
	boundSecondTotal = 630_000
	// boundSecondOver is the share above its weight a fitted row set may weigh: 1 %.
	boundSecondOver = 100
)

// boundView is one read of R7: the view it serves, the route, the filters past order=activity,
// and every thread the filters hold, newest first.
type boundView struct {
	view, route, filters string
	threads              []string
}

// TestThreadPagesHoldTheirBoundOnTheLargestIssues is c-bound. It times each of R7's seven first
// pages through the handler, 5 warm-up reads and then 30, holds the nearest-rank 90th percentile
// to 200 ms, and holds each page to 20 threads and to the bytes of those rows and its own two
// members. It then reads All's three first pages of the second issue and holds their sum to
// 630,000 bytes.
//
// It is a measurement, so it runs only on a database DISPATCH_BENCH_DATABASE_URL names, which it
// migrates and seeds itself; CI leaves the variable unset (scripts/database-skips.sh). It opens no
// benchtime.Runner, which binds the subjects of a cmd/dispatch-seed database this one does not hold.
func TestThreadPagesHoldTheirBoundOnTheLargestIssues(t *testing.T) {
	t.Parallel()
	databaseURL := os.Getenv(benchtime.DatabaseURLEnv)
	if databaseURL == "" {
		t.Skip(benchtime.DatabaseURLEnv + " must name a database of this measurement's own, which it migrates and seeds; CI leaves it unset")
	}
	database := openBoundDatabase(t, databaseURL)
	handler, _, _ := newTestServer(t, testServerOptions{store: database})
	shape := seedBound(t, handler, database)
	logBoundSettings(t, database)

	views := []boundView{
		{"All", "comments", "&person=in", shape.personComments},
		{"All", "messages", "&person=in", shape.personMessages},
		{"All", "asks", "&thread=1", append(slices.Clone(shape.agentAsks), shape.personAsks...)},
		{"Mine", "comments", "&person=in&author=" + boundPerson, shape.personComments},
		{"Mine", "messages", "&person=in&author=" + boundPerson, shape.personMessages},
		{"Mine", "asks", "&thread=1&author=" + boundPerson, shape.personAsks},
		{"Agents", "messages", "&person=none", shape.agentMessages},
	}
	for _, view := range views {
		target := boundTarget(shape.large, view.route, view.filters)
		for range boundWarmups {
			boundRead(t, shape, shape.large, view.route, view.filters)
		}
		taken := make([]time.Duration, 0, boundRuns)
		var last boundReading
		for range boundRuns {
			last = boundRead(t, shape, shape.large, view.route, view.filters)
			taken = append(taken, last.took)
		}
		slices.Sort(taken)
		p90 := percentileOfDismissedScale(taken, 0.9)
		page := readBoundPage(t, target, last.body)
		t.Logf("%s %s: p50 %.1f ms, p90 %.1f ms, max %.1f ms over %d reads; the first page is %d bytes, its %d rows %d bytes",
			view.view, view.route, msOf(percentileOfDismissedScale(taken, 0.5)), msOf(p90), msOf(taken[len(taken)-1]),
			boundRuns, len(last.body), len(page.rows), page.rowBytes())
		if p90 > boundPageBound {
			t.Errorf("%s: the 90th percentile of %d reads is %.1f ms, want at most %.0f ms", target, boundRuns, msOf(p90), msOf(boundPageBound))
		}
		page.requireOnly(t, target)
		if ids := page.threadIDs(t, view.route); !slices.Equal(ids, view.threads[:boundPageRows]) {
			t.Errorf("%s: the first page holds %d threads %v, want the %d newest of the seed's %d: %v",
				target, len(ids), ids, boundPageRows, len(view.threads), view.threads[:boundPageRows])
		}
		if want := len(view.threads) - boundPageRows; string(page.remaining) != strconv.Itoa(want) {
			t.Errorf("%s: remaining = %s, want %d", target, page.remaining, want)
		}
	}

	// The second issue: All's three first pages, which R7 bounds together.
	total := 0
	for _, second := range []struct {
		route, filters string
		threads, rows  int
	}{
		{"comments", "&person=in", boundSecondComments, boundSecondCommentBytes},
		{"messages", "&person=in", 0, 0},
		{"asks", "&thread=1", boundSecondAsks, boundSecondAskBytes},
	} {
		target := boundTarget(shape.second, second.route, second.filters)
		read := boundRead(t, shape, shape.second, second.route, second.filters)
		page := readBoundPage(t, target, read.body)
		total += len(read.body)
		t.Logf("%s %s: the first page is %d bytes, its %d rows %d bytes", shape.second, second.route, len(read.body), len(page.rows), page.rowBytes())
		page.requireOnly(t, target)
		if len(page.rows) != second.threads || page.rowBytes() < second.rows || page.rowBytes() > second.rows+second.rows/boundSecondOver {
			t.Errorf("%s: %d rows in %d bytes, want %d rows in %d bytes and at most 1 %% more", target, len(page.rows), page.rowBytes(), second.threads, second.rows)
		}
	}
	t.Logf("%s: All's three first pages are %d bytes together", shape.second, total)
	if total > boundSecondTotal {
		t.Errorf("%s: All's three first pages are %d bytes together, want at most %d", shape.second, total, boundSecondTotal)
	}
}

// openBoundDatabase opens and migrates the database the measurement seeds. One that already holds
// the seed's project is refused: the seed writes fixed keys, so a second run would fail halfway.
func openBoundDatabase(t *testing.T, databaseURL string) *store.Store {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the measurement database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the measurement database: %v", err)
	}
	var held bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from projects where key = $1)`, boundProject).Scan(&held); err != nil {
		t.Fatalf("read the measurement database's projects: %v", err)
	}
	if held {
		t.Fatalf("the measurement database already holds project %s: give the measurement a database of its own (createdb), as c-bound's run does", boundProject)
	}
	return database
}

// logBoundSettings prints the server settings every reading is taken beside, as cmd/dispatch's
// README asks of a measurement ("A large test database").
func logBoundSettings(t *testing.T, database *store.Store) {
	t.Helper()
	settings := []string{}
	for _, name := range []string{"fsync", "synchronous_commit", "full_page_writes", "shared_buffers"} {
		var value string
		if err := database.Pool.QueryRow(context.Background(), "show "+name).Scan(&value); err != nil {
			t.Fatalf("show %s: %v", name, err)
		}
		settings = append(settings, name+"="+value)
	}
	t.Logf("measurement server: %s", strings.Join(settings, " "))
}

// boundTarget is a first page of one of the issue's thread routes, under the filters given.
func boundTarget(issue, route, filters string) string {
	return "/api/v1/issues/" + issue + "/" + route + "?order=activity" + filters
}

// boundReading is one read's time at the handler and the bytes it answered.
type boundReading struct {
	took time.Duration
	body []byte
}

// boundRead reads a first page as the person with display trees, timing the handler alone. A
// comment page first moves the issue's comment watermark, as any comment write moves it, so the
// page is built rather than handed back from the answer the server kept (list_answers.go): a
// kept answer would time a copy, not the read R7 bounds.
func boundRead(t *testing.T, shape *boundShape, issue, route, filters string) boundReading {
	t.Helper()
	target := boundTarget(issue, route, filters)
	if route == "comments" {
		if _, err := shape.database.Pool.Exec(context.Background(), `select comment_watermark_bump($1, null)`, issue); err != nil {
			t.Fatalf("move the comment watermark of %s: %v", issue, err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", boundPerson)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	started := time.Now()
	shape.handler.ServeHTTP(response, request)
	took := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%.240s", target, response.Code, response.Body.String())
	}
	return boundReading{took: took, body: response.Body.Bytes()}
}

// boundPage is one first page as it went on the wire: the whole answer, each row's own bytes, and
// its two members.
type boundPage struct {
	body              []byte
	members           int
	rows              []json.RawMessage
	before, remaining json.RawMessage
}

func readBoundPage(t *testing.T, target string, body []byte) boundPage {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("GET %s: %v: %.240s", target, err, body)
	}
	page := boundPage{body: body, members: len(members), before: members["before"], remaining: members["remaining"]}
	if err := json.Unmarshal(members["threads"], &page.rows); err != nil {
		t.Fatalf("GET %s: threads: %v: %.240s", target, err, body)
	}
	return page
}

func (p boundPage) rowBytes() int {
	sum := 0
	for _, row := range p.rows {
		sum += len(row)
	}
	return sum
}

// requireOnly holds the answer to its rows and its own two members: rebuilt from them as the
// encoder writes the page, it must be the answer byte for byte, so anything more is bytes the
// reader receives that are not its page.
func (p boundPage) requireOnly(t *testing.T, target string) {
	t.Helper()
	var rebuilt bytes.Buffer
	rebuilt.WriteString(`{"threads":[`)
	for i, row := range p.rows {
		if i > 0 {
			rebuilt.WriteByte(',')
		}
		rebuilt.Write(row)
	}
	rebuilt.WriteString(`],"before":`)
	rebuilt.Write(p.before)
	rebuilt.WriteString(`,"remaining":`)
	rebuilt.Write(p.remaining)
	rebuilt.WriteString("}\n")
	if p.members != 3 || !bytes.Equal(rebuilt.Bytes(), p.body) {
		t.Errorf("%s: the answer is %d bytes in %d members, and its %d rows with before and remaining are %d: want the rows and those two members and no more",
			target, len(p.body), p.members, len(p.rows), rebuilt.Len())
	}
}

// threadIDs are the page's threads in order, each by its root's or its ask's id.
func (p boundPage) threadIDs(t *testing.T, route string) []string {
	t.Helper()
	ids := make([]string, 0, len(p.rows))
	for _, raw := range p.rows {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("a %s row: %v", route, err)
		}
		ids = append(ids, pageThreadID(route, row))
	}
	return ids
}
