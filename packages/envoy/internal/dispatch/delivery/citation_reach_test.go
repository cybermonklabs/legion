package delivery

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/citebody"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody is the differential of
// citationReach. citedIssues hands each path's decision the body from citationReach bytes
// before the path; before, it handed the whole body up to the path's end. Over a generated corpus
// (reachCorpus) the row holds, for every issue path the finder answers:
//
//   - authorityBefore finds the same authority over the reach as over the whole body, and over a
//     reach one byte shorter it finds another for some path of the corpus, so citationReach is the
//     least reach that keeps it;
//   - citesGitHubIssue answers the same over the reach as over the whole body;
//
// and, for every body, that citedIssues answers what the whole-body reading answers (the citations
// the paths' whole-body decisions credit, and the bare references), and reads exactly: one pass for
// each finder, the reach before each path (or the body before it, where the path stands nearer the
// start) and the path, the owner, repository and number of each path it credits, and the number of
// each bare reference.
//
// Every body is read, and in subtests of the corpus group that run beside each other: the bodies
// in shards of consecutive bodies (reachShards), and the 1 MiB body's paths in shards of
// consecutive paths (reachPathShard), beside one subtest that reads its bare references and has
// citedIssues read it whole. Once the group has ended, the 1 MiB body's whole-body reading is the
// citations its path shards credited and its bare references, and the counts the last check reads
// are summed over every shard. The corpus, the 1 MiB body's paths and citedIssues' reading of it are
// built once per test process (reachCorpusBodies, citationBigBodyPaths, citationBigBodyReading).
func TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody(t *testing.T) {
	t.Parallel()
	const repo = "acme/widgets"
	var counts reachCounts
	var failures atomic.Int32
	bodies := reachCorpusBodies()
	big := slices.Index(bodies, citationBigBody())
	if big < 0 {
		t.Fatal("the corpus holds no 1 MiB body")
	}
	bigPaths := citationBigBodyPaths()
	pathShards := (len(bigPaths) + reachPathShard - 1) / reachPathShard
	bigParts := make([]reachPart, pathShards)
	var bigBare []wholeBodyCitation
	var bigBareReads citebody.Reads
	t.Run("corpus", func(t *testing.T) {
		failFor := func(t *testing.T) func(format string, args ...any) {
			return func(format string, args ...any) {
				t.Helper()
				t.Errorf(format, args...)
				if failures.Add(1) >= 20 {
					t.Fatalf("stopped after %d differences", 20)
				}
			}
		}
		for _, shard := range reachShards(bodies) {
			if shard.first == big {
				continue // read in the subtests below, a shard of its paths at a time
			}
			t.Run(fmt.Sprintf("bodies %d to %d", shard.first, shard.last), func(t *testing.T) {
				t.Parallel()
				for i := shard.first; i <= shard.last; i++ {
					checkReachOfBody(i, repo, bodies[i], &counts, failFor(t))
				}
			})
		}
		for k := range pathShards {
			first, last := k*reachPathShard, min((k+1)*reachPathShard, len(bigPaths))-1
			t.Run(fmt.Sprintf("the 1 MiB body's paths %d to %d", first, last), func(t *testing.T) {
				t.Parallel()
				bigParts[k] = checkReachOfPaths(big, bodies[big], bigPaths[first:last+1], &counts, failFor(t))
			})
		}
		t.Run("the 1 MiB body's bare references and citedIssues' reading", func(t *testing.T) {
			t.Parallel()
			bigBare, bigBareReads = bareOverTheWholeBody(repo, bodies[big])
			citationBigBodyReading()
		})
	})
	// The 1 MiB body, from its shards: citedIssues answers what the whole-body reading answers, and
	// reads exactly the spans its paths and bare references call for.
	counts.bodies.Add(1)
	wantReads := citebody.Reads{Passes: 2}
	var credited []wholeBodyCitation
	for _, part := range append(bigParts, reachPart{reads: bigBareReads}) {
		addReads(&wantReads, part.reads)
		credited = append(credited, part.credited...)
	}
	reading := citationBigBodyReading()
	if want := wholeBodyURLs(append(credited, bigBare...)); !slices.Equal(reading.urls, want) {
		t.Errorf("body %d (%d bytes) cites %v within the reach and %v over the whole body: %.300q", big, len(bodies[big]), reading.urls, want, bodies[big])
	}
	if reading.reads != wantReads {
		t.Errorf("body %d: citedIssues read %+v, want %+v: the reach before each path or the body before it where that is shorter, the path, and each credited citation's parts", big, reading.reads, wantReads)
	}
	bodiesRead, paths, cut, shorterDiffers, creditedPaths, refused := counts.bodies.Load(), counts.paths.Load(), counts.cut.Load(), counts.shorterDiffers.Load(), counts.credited.Load(), counts.refused.Load()
	t.Logf("%d bodies, %d paths: %d decided over a reach that cut the body, %d of those found another authority over a reach one byte shorter; %d credited, %d refused", bodiesRead, paths, cut, shorterDiffers, creditedPaths, refused)
	if bodiesRead != int64(len(bodies)) {
		t.Errorf("the shards read %d of the corpus's %d bodies", bodiesRead, len(bodies))
	}
	if cut == 0 || shorterDiffers == 0 || creditedPaths == 0 || refused == 0 {
		t.Errorf("the corpus has %d paths the reach cuts the body before, %d whose authority a reach one byte shorter changes, %d credited and %d refused, want some of each: without them the row compares nothing at the reach's edge", cut, shorterDiffers, creditedPaths, refused)
	}
}

// reachCounts is what the differential row counts over the corpus, summed over its shards.
type reachCounts struct {
	bodies, paths, cut, shorterDiffers, credited, refused atomic.Int64
}

// reachShard is the bodies first to last of the corpus, which one subtest reads.
type reachShard struct{ first, last int }

// reachShardBytes is about how many bytes of bodies one shard of the corpus reads, so each shard
// reads in a fraction of a second: a body is read a few times over, and once more for each of its
// paths over the reach before it.
const reachShardBytes = 32 << 10

// reachPathShard is how many of the 1 MiB body's paths one shard reads, so each shard reads in a
// fraction of a second: each path is decided four times over the reach before it or the body before
// it (authorityBefore, citesGitHubIssue).
const reachPathShard = 4096

// reachShards cuts bodies into shards of consecutive bodies of about reachShardBytes together. A
// body longer than that is a shard of its own.
func reachShards(bodies []string) []reachShard {
	var shards []reachShard
	first, size := 0, 0
	for i, body := range bodies {
		if i > first && size+len(body) > reachShardBytes {
			shards = append(shards, reachShard{first, i - 1})
			first, size = i, 0
		}
		size += len(body)
	}
	return append(shards, reachShard{first, len(bodies) - 1})
}

// reachPart is what the differential reads of some issue paths of a body (checkReachOfPaths): the
// spans citedIssues should read for them, and the citations the whole-body reading credits among
// them.
type reachPart struct {
	reads    citebody.Reads
	credited []wholeBodyCitation
}

// checkReachOfBody holds the differential for body i of the corpus in repo, adding what it counts
// to counts and handing each difference to fail.
func checkReachOfBody(i int, repo, body string, counts *reachCounts, fail func(format string, args ...any)) {
	counts.bodies.Add(1)
	var reads citebody.Reads
	got := citedIssues(refs.Renames{}, repo, citebody.New(body, &reads))
	part := checkReachOfPaths(i, body, githubIssuePath.FindAllStringSubmatchIndex(body, -1), counts, fail)
	bare, bareReads := bareOverTheWholeBody(repo, body)
	if want := wholeBodyURLs(append(part.credited, bare...)); !slices.Equal(got, want) {
		fail("body %d (%d bytes) cites %v within the reach and %v over the whole body: %.300q", i, len(body), got, want, body)
	}
	wantReads := citebody.Reads{Passes: 2}
	addReads(&wantReads, part.reads)
	addReads(&wantReads, bareReads)
	if reads != wantReads {
		fail("body %d: citedIssues read %+v, want %+v: the reach before each path or the body before it where that is shorter, the path, and each credited citation's parts", i, reads, wantReads)
	}
}

// checkReachOfPaths holds the differential for the issue paths matches of body i: authorityBefore
// and citesGitHubIssue answer over the reach as over the whole body. It adds what it counts to
// counts, hands each difference to fail, and answers the spans citedIssues should read for those
// paths, the reach before each (or the body before it) and the path, and the owner, repository and
// number of each it credits, and the citations the whole-body reading credits among them.
func checkReachOfPaths(i int, body string, matches [][]int, counts *reachCounts, fail func(format string, args ...any)) reachPart {
	var part reachPart
	for _, match := range matches {
		counts.paths.Add(1)
		pathAt, end := match[0], match[1]
		from := max(0, pathAt-citationReach)
		addSpan(&part.reads, end-from)
		whole := authorityBefore(body[:end], pathAt)
		if within := offsetAuthority(authorityBefore(body[from:end], pathAt-from), from); within != whole {
			fail("body %d, the path at %d: authorityBefore finds %d within the reach and %d over the whole body: %.300q", i, pathAt, within, whole, body[from:end])
		}
		if from > 0 {
			counts.cut.Add(1)
			if offsetAuthority(authorityBefore(body[from+1:end], pathAt-from-1), from+1) != whole {
				counts.shorterDiffers.Add(1)
			}
		}
		citation, cites := creditedOverTheWholeBody(body, match)
		if within := citesGitHubIssue(body[from:end], pathAt-from); within != cites {
			fail("body %d, the path at %d: citesGitHubIssue answers %t within the reach and %t over the whole body: %.300q", i, pathAt, within, cites, body[from:end])
		}
		if cites {
			counts.credited.Add(1)
			addSpan(&part.reads, match[3]-match[2])
			addSpan(&part.reads, match[5]-match[4])
			addSpan(&part.reads, match[7]-match[6])
			part.credited = append(part.credited, citation)
		} else {
			counts.refused.Add(1)
		}
	}
	return part
}

// citationBigBody is the 1 MiB body both citation rows read (TestCitedIssuesCostsNoMoreThanTheBodyIsLong
// and the differential's corpus, reachCorpus): no whitespace in it and an issue path every thirteen
// bytes, then one citation of acme/widgets#7. It is built once per test process.
var citationBigBody = sync.OnceValue(func() string {
	return strings.Repeat("/a/b/issues/1", 1<<20/13) + ",github.com/acme/widgets/issues/7"
})

// citationBigBodyPaths is the issue paths of citationBigBody, as the differential reads a body's
// (githubIssuePath): found once per test process.
var citationBigBodyPaths = sync.OnceValue(func() [][]int {
	return githubIssuePath.FindAllStringSubmatchIndex(citationBigBody(), -1)
})

// citationReading is citedIssues' answer for a body and the reads it made of it (citebody.Reads).
type citationReading struct {
	urls  []string
	reads citebody.Reads
}

// citationBigBodyReading is citedIssues' reading of citationBigBody in acme/widgets, with the reads
// it made, which both citation rows hold to what they require: made once per test process, since
// citedIssues is a pure function of the body.
var citationBigBodyReading = sync.OnceValue(func() citationReading {
	var reads citebody.Reads
	urls := citedIssues(refs.Renames{}, "acme/widgets", citebody.New(citationBigBody(), &reads))
	return citationReading{urls: urls, reads: reads}
})

// reachCorpusBodies is the differential's corpus (reachCorpus), built once per test process.
var reachCorpusBodies = sync.OnceValue(reachCorpus)

// offsetAuthority is an authority authorityBefore found in a part of a body that starts at
// from, as an index of the whole body; -1, no authority, stays -1.
func offsetAuthority(at, from int) int {
	if at < 0 {
		return at
	}
	return at + from
}

// wholeBodyCitation is a citation the whole-body reading credits: where it stands in the body, and
// the issue it names.
type wholeBodyCitation struct {
	at  int
	url string
}

// creditedOverTheWholeBody is the whole-body reading's decision on the issue path match of body:
// citesGitHubIssue over the body from its start to the path's end, and the citation it credits.
func creditedOverTheWholeBody(body string, match []int) (wholeBodyCitation, bool) {
	if !citesGitHubIssue(body[:match[1]], match[0]) {
		return wholeBodyCitation{}, false
	}
	return wholeBodyCitation{match[0], "https://github.com/" + body[match[2]:match[3]] + "/" + body[match[4]:match[5]] + "/issues/" + body[match[6]:match[7]]}, true
}

// bareOverTheWholeBody is the bare references of body, each an issue of repo, and the span
// citedIssues should read of each: its number.
func bareOverTheWholeBody(repo, body string) ([]wholeBodyCitation, citebody.Reads) {
	var citations []wholeBodyCitation
	var reads citebody.Reads
	for _, match := range githubBareReference.FindAllStringSubmatchIndex(body, -1) {
		citations = append(citations, wholeBodyCitation{match[2], "https://github.com/" + repo + "/issues/" + body[match[2]:match[3]]})
		addSpan(&reads, match[3]-match[2])
	}
	return citations, reads
}

// wholeBodyURLs is the whole-body reading's answer from its citations: their issues in the order
// they stand in the body, each once.
func wholeBodyURLs(citations []wholeBodyCitation) []string {
	slices.SortStableFunc(citations, func(a, b wholeBodyCitation) int { return cmp.Compare(a.at, b.at) })
	urls := []string{}
	for _, c := range citations {
		urls = appendUnique(urls, c.url)
	}
	return urls
}

// addSpan adds to reads one span of n bytes that citedIssues hands a finder.
func addSpan(reads *citebody.Reads, n int) {
	reads.Spans++
	reads.Handed += n
	reads.Longest = max(reads.Longest, n)
}

// addReads adds to reads the spans part counts.
func addReads(reads *citebody.Reads, part citebody.Reads) {
	reads.Spans += part.Spans
	reads.Handed += part.Handed
	reads.Longest = max(reads.Longest, part.Longest)
}

// reachCorpus is the differential row's bodies:
//
//   - a grid of the shapes at the reach's edges: a port of up to and past maxHostName digits, an
//     empty one and none, after a run of host bytes up to and past maxHostName ending in
//     github.com, with a host byte, a space, a comma, an `@` or a two-byte letter before the run,
//     each far from the body's start (the reach cuts the body) and at it;
//   - the bodies of the cost rows, of TestCitedIssuesReadsAHostNameOfAHostNamesLength and of the
//     default branch's corpus (citationCorpus);
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
	one := "github.com/acme/widgets/issues/7"
	bodies = append(bodies,
		citationBigBody(),
		strings.Repeat("a", 65536)+" "+strings.TrimSuffix(strings.Repeat(one+" ", 200), " "),
		strings.Repeat("a", 65536)+","+strings.TrimSuffix(strings.Repeat(one+",", 200), ","),
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
