package delivery

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// citationReach is the most of a body before an issue path that the reading hands the host check
// of that path (attribution_keys): 507 bytes, 2 x 253 + 1, the bound the Go reading of the default
// branch gives its own host check.
const citationReach = 2*253 + 1

// readingPath finds each issue path of a body as the path alternative of the reading's one pass
// finds it, so a row can say what the reading should have handed the check of each.
var readingPath = regexp.MustCompile(`/([\w.-]+)/([\w.-]+)/issues/([1-9][0-9]*)\b`)

// readingSlice is the slice of a body the reading hands each issue path's host check: the
// citationReach bytes before the path, or the bytes before it where the path stands nearer the
// start. readingSliceFromTheStart is the body from its start to the path, the slice a check that
// scans back from each path to the nearest space would need. readingCheckInput is the call the
// slice is handed to, which drops the trailing bytes of a character the slice cuts at its start,
// and readingContinuationBytes the second argument of that call, which ends it.
const (
	readingSlice             = "pg_catalog.substring(b.bytes, greatest(path_at - 507, 0) + 1, least(path_at, 507))"
	readingSliceFromTheStart = "pg_catalog.substring(b.bytes, 1, path_at)"
	readingCheckInput        = "pg_catalog.ltrim("
	readingContinuationBytes = `, '\x808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf'::bytea)`
)

// readingSpans is what a reading handed the host checks of one body: Spans slices, Handed bytes in
// all, the Longest of them.
type readingSpans struct{ Spans, Handed, Longest int }

// createCountedReading creates name, a copy of the reading in which whatever the reading hands
// each issue path's host check, the first argument of its one readingCheckInput call, passes
// through reading_span first; edits change the copy before that. reading_span counts each slice it
// is given in the session's settings reading.spans, reading.handed and reading.longest, and
// readingSpansOf reads them for one body. A reading with no such call fails here rather than
// counting nothing; a reading that hands the call more than the reach is counted as it is.
func createCountedReading(t *testing.T, ctx context.Context, q readingSession, name string, edits ...readingEdit) {
	t.Helper()
	schema := readingSchema(t, ctx, q)
	if _, err := q.Exec(ctx, `create or replace function reading_span(span bytea) returns bytea language plpgsql as $$
		declare
		  n bigint := pg_catalog.octet_length(span);
		begin
		  perform pg_catalog.set_config('reading.spans', (pg_catalog.current_setting('reading.spans')::bigint + 1)::text, false);
		  perform pg_catalog.set_config('reading.handed', (pg_catalog.current_setting('reading.handed')::bigint + n)::text, false);
		  perform pg_catalog.set_config('reading.longest', greatest(pg_catalog.current_setting('reading.longest')::bigint, n)::text, false);
		  return span;
		end $$`); err != nil {
		t.Fatalf("create reading_span: %v", err)
	}
	if _, err := q.Exec(ctx, `create or replace function reading_spans_of(reading text, repo text, body text,
		  out cited text[], out spans bigint, out handed bigint, out longest bigint) language plpgsql as $$
		begin
		  perform pg_catalog.set_config('reading.spans', '0', false), pg_catalog.set_config('reading.handed', '0', false),
		    pg_catalog.set_config('reading.longest', '0', false);
		  execute pg_catalog.format('select k.cited_issues from %s($1, '''', '''', $2, null) k', reading) into cited using repo, body;
		  spans := pg_catalog.current_setting('reading.spans')::bigint;
		  handed := pg_catalog.current_setting('reading.handed')::bigint;
		  longest := pg_catalog.current_setting('reading.longest')::bigint;
		end $$`); err != nil {
		t.Fatalf("create reading_spans_of: %v", err)
	}
	createReadingCopy(t, ctx, q, name, append(edits,
		readingEdit{readingCheckInput, readingCheckInput + schema + ".reading_span(", 1},
		readingEdit{readingContinuationBytes, ")" + readingContinuationBytes, 1})...)
}

// readingSpansOf is what reading, a copy createCountedReading made, cites in body and what it
// handed the host checks of that body.
func readingSpansOf(t *testing.T, ctx context.Context, q Querier, reading, repo, body string) ([]string, readingSpans) {
	t.Helper()
	var cited []string
	var spans readingSpans
	if err := q.QueryRow(ctx, `select cited, spans, handed, longest from reading_spans_of($1, $2, $3)`, reading, repo, body).Scan(
		&cited, &spans.Spans, &spans.Handed, &spans.Longest); err != nil {
		t.Fatalf("%s over %d bytes: %v", reading, len(body), err)
	}
	return cited, spans
}

// holdToTheReading is what the cost rows hold of spans, what a reading handed the host checks of
// body: one slice for each issue path, since each path's decision needs one and a bare reference's
// none, and no slice longer than citationReach. Together they bound the bytes the checks read by
// citationReach times the paths, whatever the body holds before each. It answers each one spans
// breaks.
func holdToTheReading(what, body string, spans readingSpans) []string {
	paths := len(readingPath.FindAllStringIndex(body, -1))
	var problems []string
	if spans.Spans != paths {
		problems = append(problems, fmt.Sprintf("%s: %d slices for %d issue paths: a path was decided from no counted slice, or from more than one", what, spans.Spans, paths))
	}
	if spans.Longest > citationReach {
		problems = append(problems, fmt.Sprintf("%s: one slice handed %d bytes, want at most %d (citationReach): a path is read back to the nearest space rather than a bounded run", what, spans.Longest, citationReach))
	}
	return problems
}

// The reading reads a body in proportion to its length. It reaches the bytes before an issue path
// only through the one slice it hands that path's host check, and the row counts those slices
// (createCountedReading, holdToTheReading): one for each path, none longer than citationReach, so
// how busy the machine is decides nothing. The body is 1 MiB, holds no whitespace, and holds an
// issue path every thirteen bytes, which is the shape a reading that walks back to the nearest
// space pays the square of: such a reading needs the body back to its start at every path.
func TestTheReadingCostsNoMoreThanTheBodyIsLong(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const cited = "https://github.com/acme/widgets/issues/7"
	createCountedReading(t, ctx, pool, "attribution_keys_counted")
	body := strings.Repeat("/a/b/issues/1", 1<<20/13) + ",github.com/acme/widgets/issues/7"
	got, spans := readingSpansOf(t, ctx, pool, "attribution_keys_counted", "acme/widgets", body)
	t.Logf("a body of %d bytes: %+v", len(body), spans)
	for _, problem := range holdToTheReading(fmt.Sprintf("a body of %d bytes", len(body)), body, spans) {
		t.Error(problem)
	}
	if !slices.Equal(got, []string{cited}) {
		t.Fatalf("a body of %d bytes cites %v, want %s and nothing else", len(body), got, cited)
	}
	whole := cited + strings.Repeat("x", 4096)
	if got := readingOf(t, ctx, pool, readingText{Repo: "acme/widgets", Body: whole}).CitedIssues; len(got) != 0 {
		t.Fatalf("a path running past the issue number cites %v, want nothing", got)
	}
}

// Two bodies of one length carry the same 200 citations, separated by spaces in one and by commas
// in the other. A reading that walks back to the nearest space needs the 65,536 bytes of prose for
// every path of the comma body; a reading of a bounded run needs the same bytes for both, since
// neither a space nor a comma is a host byte. The row holds each body's counts (holdToTheReading)
// and the two bodies to the same counts: a count has no noise to allow for, so the bound is the
// claim itself rather than a ratio.
func TestTheReadingCostsNoMoreForACitationFarFromASpace(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	createCountedReading(t, ctx, pool, "attribution_keys_counted")
	bySpace, byComma := citationsFarFromASpace()
	_, space := readingSpansOf(t, ctx, pool, "attribution_keys_counted", "acme/widgets", bySpace)
	_, comma := readingSpansOf(t, ctx, pool, "attribution_keys_counted", "acme/widgets", byComma)
	t.Logf("separated by spaces %+v, by commas %+v", space, comma)
	for _, problem := range append(holdToTheReading("the citations separated by spaces", bySpace, space), holdToTheReading("the citations separated by commas", byComma, comma)...) {
		t.Error(problem)
	}
	if comma != space {
		t.Errorf("citing %d bytes separated by commas handed %+v, and separated by spaces %+v, want the same: a path is read back to the nearest space rather than a bounded run", len(byComma), comma, space)
	}
}

// citationsFarFromASpace is the cost row's two bodies: 65,536 bytes of prose and 200 citations,
// separated by spaces and by commas.
func citationsFarFromASpace() (bySpace, byComma string) {
	one := "github.com/acme/widgets/issues/7"
	prose := strings.Repeat("a", 65536)
	return prose + " " + strings.TrimSuffix(strings.Repeat(one+" ", 200), " "),
		prose + "," + strings.TrimSuffix(strings.Repeat(one+",", 200), ",")
}

// TestTheReadingCountsAScanBackFromEachPath is the cost rows' control. A copy of the reading whose
// host checks get the body from its start to each path, the slice a check that scans back to the
// nearest space needs, hands each check that slice, so holdToTheReading names it: here over the
// comma body, where the scan needs the body back to its start at every path. It cites what the
// reading cites, so only the count can tell the two apart.
func TestTheReadingCountsAScanBackFromEachPath(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	createCountedReading(t, ctx, pool, "attribution_keys_scan_back", readingEdit{readingSlice, readingSliceFromTheStart, 1})
	_, byComma := citationsFarFromASpace()
	cited, spans := readingSpansOf(t, ctx, pool, "attribution_keys_scan_back", "acme/widgets", byComma)
	problems := holdToTheReading("the comma body read back from each path", byComma, spans)
	t.Logf("%+v: %q", spans, problems)
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "one slice handed") }) {
		t.Errorf("a scan back from each of 200 paths over %d bytes handed %+v, and the cost rows' check named no slice longer than citationReach: it cannot see the scan", len(byComma), spans)
	}
	if want := []string{"https://github.com/acme/widgets/issues/7"}; !slices.Equal(cited, want) {
		t.Errorf("the scan back cites %v, want %v as the reading does", cited, want)
	}
}

// hostCheckReach is how far before an issue path the reading's host check reads: the byte before
// the host, github.com, the colon and the longest port it credits (attribution_keys).
const hostCheckReach = 1 + len("github.com") + 1 + 253

// wholeBodyLimit is the longest body the row reads with the whole-body copy. That copy hands each
// path's check the body from its start, so it costs the square of a body's length: over the cost
// row's 1 MiB body, with a path every thirteen bytes, about 40 GB. A longer body is held to the
// copy whose check gets hostCheckReach bytes, which cites as the whole-body copy does over every
// shorter body of the corpus; the same shape at 64 KiB is read whole.
const wholeBodyLimit = 1 << 17

// TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody is the differential of
// citationReach, on the reading in the database (attribution_keys). The reading hands each issue
// path's host check the citationReach bytes before the path (readingSlice); before, its one
// pattern decided each path over the whole body. A copy of it whose check gets the body from its
// start (readingSliceFromTheStart) is that whole-body reading. Over a generated corpus
// (reachCorpus) the row holds, for every body:
//
//   - the reading cites what the whole-body copy cites (up to wholeBodyLimit);
//   - a copy whose check gets hostCheckReach bytes cites the same, and one whose check gets a byte
//     fewer cites otherwise for some body of the corpus, so the corpus reaches the edge of what the
//     check reads, which citationReach holds;
//   - the reading hands the checks exactly one slice for each issue path the finder answers, the
//     citationReach bytes before the path or the body before it where the path stands nearer the
//     start (counted through reading_span, createCountedReading).
//
// The Go row also compared authorityBefore and citesGitHubIssue over the reach and the body, and
// counted the passes over the body and the spans of each credited citation's parts. The reading
// has neither function, its check being one pattern over the slice, and no counted body: the one
// pass's captures give a citation's parts. So the first two holds above stand for them.
func TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const repo = "acme/widgets"
	reach := func(n int) string {
		return fmt.Sprintf("pg_catalog.substring(b.bytes, greatest(path_at - %d, 0) + 1, least(path_at, %d))", n, n)
	}
	if got := reach(citationReach); got != readingSlice {
		t.Fatalf("the slice of a reach of %d is %q, want the reading's own %q", citationReach, got, readingSlice)
	}
	createReadingCopy(t, ctx, pool, "attribution_keys_whole", readingEdit{readingSlice, readingSliceFromTheStart, 1})
	createReadingCopy(t, ctx, pool, "attribution_keys_check_reach", readingEdit{readingSlice, reach(hostCheckReach), 1})
	createReadingCopy(t, ctx, pool, "attribution_keys_check_reach_short", readingEdit{readingSlice, reach(hostCheckReach - 1), 1})
	createCountedReading(t, ctx, pool, "attribution_keys_counted")
	var bodies, paths, cut, shorterDiffers, credited, refused, failures int
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
		if failures++; failures == 20 {
			t.Fatalf("stopped after %d differences", failures)
		}
	}
	// One statement reads every body, so the row's time is the database's reading and not a round
	// trip for each body: reading_spans_of sets its counts back to 0 at each call.
	corpus := reachCorpus()
	rows, err := pool.Query(ctx, `select (attribution_keys($1, '', '', u.body, null)).cited_issues,
		pg_catalog.octet_length(u.body) <= $3,
		case when pg_catalog.octet_length(u.body) <= $3 then (attribution_keys_whole($1, '', '', u.body, null)).cited_issues end,
		(attribution_keys_check_reach($1, '', '', u.body, null)).cited_issues,
		(attribution_keys_check_reach_short($1, '', '', u.body, null)).cited_issues,
		s.cited, s.spans, s.handed, s.longest
		from unnest($2::text[]) with ordinality u(body, i), lateral reading_spans_of('attribution_keys_counted', $1, u.body) s
		order by u.i`, repo, corpus, wholeBodyLimit)
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	type answer struct {
		reading, whole, atReach, short, counted []string
		wholeRead                               bool
		spans                                   readingSpans
	}
	var answers []answer
	for rows.Next() {
		var a answer
		if err := rows.Scan(&a.reading, &a.wholeRead, &a.whole, &a.atReach, &a.short, &a.counted, &a.spans.Spans, &a.spans.Handed, &a.spans.Longest); err != nil {
			t.Fatalf("read the answer of body %d: %v", len(answers), err)
		}
		answers = append(answers, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	if len(answers) != len(corpus) {
		t.Fatalf("the statement answered %d bodies of %d", len(answers), len(corpus))
	}
	var wholeBodies int
	for i, body := range corpus {
		bodies++
		reading, whole, atReach, short, counted, spans := answers[i].reading, answers[i].whole, answers[i].atReach, answers[i].short, answers[i].counted, answers[i].spans
		if answers[i].wholeRead {
			wholeBodies++
			if !slices.Equal(reading, whole) {
				fail("body %d (%d bytes) cites %v within the reach and %v over the whole body: %.300q", i, len(body), reading, whole, body)
			}
			if !slices.Equal(atReach, whole) {
				fail("body %d (%d bytes) cites %v within the check's reach of %d and %v over the whole body: %.300q", i, len(body), atReach, hostCheckReach, whole, body)
			}
		} else if !slices.Equal(reading, atReach) {
			fail("body %d (%d bytes, longer than the whole-body copy reads) cites %v within the reach and %v within the check's reach of %d", i, len(body), reading, atReach, hostCheckReach)
		}
		if !slices.Equal(short, atReach) {
			shorterDiffers++
		}
		if !slices.Equal(counted, reading) {
			fail("body %d: the counted copy cites %v and the reading %v", i, counted, reading)
		}
		var want readingSpans
		found := readingPath.FindAllStringIndex(body, -1)
		for _, path := range found {
			paths++
			if path[0] > citationReach {
				cut++
			}
			n := min(path[0], citationReach)
			want.Spans++
			want.Handed += n
			want.Longest = max(want.Longest, n)
		}
		if spans != want {
			fail("body %d: the reading handed %+v, want %+v: the reach before each path or the body before it where that is shorter, one slice for each path", i, spans, want)
		}
		if len(found) > 0 && len(reading) == 0 {
			refused++
		}
		credited += len(reading)
	}
	t.Logf("%d bodies, %d of them read whole, %d paths: %d decided over a reach that cut the body; %d bodies cite otherwise over a reach a byte shorter than the check's; %d citations, %d bodies whose paths cite nothing", bodies, wholeBodies, paths, cut, shorterDiffers, credited, refused)
	if wholeBodies < bodies-1 || cut == 0 || shorterDiffers == 0 || credited == 0 || refused == 0 {
		t.Errorf("the corpus has %d bodies read whole of %d (want all but the 1 MiB one), %d paths the reach cuts the body before, %d bodies a reach a byte shorter than the check's changes, %d citations and %d bodies whose paths cite nothing, want some of each: without them the row compares nothing at the reach's edge", wholeBodies, bodies, cut, shorterDiffers, credited, refused)
	}
}

// reachCorpus is the differential row's bodies, the default branch's corpus of the same row:
//
//   - a grid of the shapes at the reach's edges: a port of up to and past 253 digits, an empty one
//     and none, after a run of host bytes up to and past 253 ending in github.com, with a host
//     byte, a space, a comma, an `@` or a two-byte letter before the run, each far from the body's
//     start (the reach cuts the body) and at it;
//   - the bodies of the cost rows, the shape of the 1 MiB one at 64 KiB, the host-name length cases
//     and the default branch's corpus (citationCorpus);
//   - 2,000 bodies drawn with a fixed seed, each of one to five citations with prose before them
//     of lengths at, near and far from the reach, in ASCII and not, any separator from a space to
//     none, schemes, userinfo, hosts that are github.com and hosts that only resemble it, ports,
//     and paths the finder answers and paths it does not, some with a bare #N after them.
func reachCorpus() []string {
	const path = "/acme/widgets/issues/7"
	var bodies []string
	for _, before := range []string{"", "a", " ", ",", "@", "é"} {
		for _, run := range []int{1, 10, 11, 252, 253, 254, 300} {
			host := strings.Repeat("a", max(0, run-10)) + "github.com"[max(0, 10-run):]
			for _, digits := range []int{-1, 0, 1, 252, 253, 254, 300} {
				port := ""
				if digits >= 0 {
					port = ":" + strings.Repeat("4", digits)
				}
				citation := before + host + port + path
				bodies = append(bodies, citation, strings.Repeat("~", 600)+citation, strings.Repeat("a", 600)+citation)
			}
		}
	}
	bySpace, byComma := citationsFarFromASpace()
	bodies = append(bodies,
		strings.Repeat("/a/b/issues/1", 1<<20/13)+",github.com/acme/widgets/issues/7",
		strings.Repeat("/a/b/issues/1", 1<<16/13)+",github.com/acme/widgets/issues/7",
		bySpace,
		byComma,
		strings.Repeat("a", 65533)+" https://github.com/acme/widgets/issues/7",
		strings.Repeat("a", 65533)+"github.com/acme/widgets/issues/7",
		"See https://github.com:"+strings.Repeat("4", 300)+"/acme/widgets/issues/7 here.",
	)
	for _, row := range citationCorpus {
		bodies = append(bodies, row.body)
	}
	alphabets := []string{"a", "1", "-", ".", "~", " ", "é", "中", "😀", "a b", "x,"}
	lengths := []int{0, 1, 5, 252, 253, 254, 505, 506, 507, 508, 700}
	separators := []string{"", " ", ",", "\n", "\t", "(", "<", "[", "é", "—", "\u00a0", "@", ":", "/", "#", "%"}
	schemes := []string{"", "https://", "http://", "HTTPS://", "//", "git+ssh://"}
	userinfos := []string{"", "", "", "user@", "a:b@"}
	hosts := []string{
		"github.com", "github.com", "GitHub.COM", "www.github.com", "evilgithub.com", "github.com.evil.test",
		"gıthub.com", "github.com.", "xn--github.com", "127.0.0.1",
		strings.Repeat("a", 242) + "github.com", strings.Repeat("a", 243) + "github.com",
	}
	ports := []string{"", "", ":443", ":", ":" + strings.Repeat("9", 252), ":" + strings.Repeat("9", 253), ":" + strings.Repeat("9", 254)}
	paths := []string{
		"/acme/widgets/issues/7", "/a/b/issues/12345", "/o.r-g/re_po/issues/9", "/acme/widgets/issues/07",
		"/acme/widgets/issues/7x", "/acme/widgets/issues/7#c", "/acme/widgets/issues/7?q=1", "/acme/widgets/pull/7",
	}
	bares := []string{"", "", " #12", " (#4567)", "#89"}
	draw := rand.New(rand.NewPCG(4904, 2026))
	pick := func(from []string) string { return from[draw.IntN(len(from))] }
	for range 2000 {
		var body strings.Builder
		for range 1 + draw.IntN(5) {
			// n copies of the alphabet: n bytes of an ASCII letter, and whole characters of the
			// others, so every body stays valid UTF-8.
			body.WriteString(strings.Repeat(pick(alphabets), lengths[draw.IntN(len(lengths))]))
			body.WriteString(pick(separators) + pick(schemes) + pick(userinfos) + pick(hosts) + pick(ports) + pick(paths) + pick(bares))
		}
		bodies = append(bodies, body.String())
	}
	return bodies
}
