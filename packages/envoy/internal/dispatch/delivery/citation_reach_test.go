package delivery

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody is the differential of
// citationReach. readCitedIssues hands each path's decision the body from citationReach bytes
// before the path; before, it handed the whole body up to the path's end. Over a generated corpus
// (reachCorpus) the row holds, for every issue path the finder answers:
//
//   - authorityBefore finds the same authority over the reach as over the whole body, and over a
//     reach one byte shorter it finds another for some path of the corpus, so citationReach is the
//     least reach that keeps it;
//   - citesGitHubIssue answers the same over the reach as over the whole body;
//
// and, for every body, that citedIssues answers what the whole-body reading answers
// (citedIssuesOverTheWholeBody), with each decision handed exactly the reach, or the whole body
// before its path where the path stands nearer the start than that.
func TestEveryCitationIsDecidedWithinItsReachAsOverTheWholeBody(t *testing.T) {
	const repo = "acme/widgets"
	var bodies, paths, cut, shorterDiffers, credited, refused, failures int
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
		if failures++; failures == 20 {
			t.Fatalf("stopped after %d differences", failures)
		}
	}
	for i, body := range reachCorpus() {
		bodies++
		var work citationWork
		got := readCitedIssues(repo, body, &work)
		if want := citedIssuesOverTheWholeBody(repo, body); !slices.Equal(got, want) {
			fail("body %d (%d bytes) cites %v within the reach and %v over the whole body: %.300q", i, len(body), got, want, body)
		}
		matches := githubIssuePath.FindAllStringIndex(body, -1)
		handed := 0
		for _, match := range matches {
			paths++
			pathAt, end := match[0], match[1]
			from := max(0, pathAt-citationReach)
			handed += pathAt - from
			whole := authorityBefore(body[:end], pathAt)
			if within := offsetAuthority(authorityBefore(body[from:end], pathAt-from), from); within != whole {
				fail("body %d, the path at %d: authorityBefore finds %d within the reach and %d over the whole body: %.300q", i, pathAt, within, whole, body[from:end])
			}
			if from > 0 {
				cut++
				if offsetAuthority(authorityBefore(body[from+1:end], pathAt-from-1), from+1) != whole {
					shorterDiffers++
				}
			}
			cites := citesGitHubIssue(body[:end], pathAt, nil)
			if within := citesGitHubIssue(body[from:end], pathAt-from, nil); within != cites {
				fail("body %d, the path at %d: citesGitHubIssue answers %t within the reach and %t over the whole body: %.300q", i, pathAt, within, cites, body[from:end])
			}
			if cites {
				credited++
			} else {
				refused++
			}
		}
		if work.decisions != len(matches) || work.before != handed {
			fail("body %d: readCitedIssues made %d decisions, handed %d bytes before their paths; want %d decisions and %d bytes, the reach before each path or the body before it where that is shorter", i, work.decisions, work.before, len(matches), handed)
		}
	}
	t.Logf("%d bodies, %d paths: %d decided over a reach that cut the body, %d of those found another authority over a reach one byte shorter; %d credited, %d refused", bodies, paths, cut, shorterDiffers, credited, refused)
	if cut == 0 || shorterDiffers == 0 || credited == 0 || refused == 0 {
		t.Errorf("the corpus has %d paths the reach cuts the body before, %d whose authority a reach one byte shorter changes, %d credited and %d refused, want some of each: without them the row compares nothing at the reach's edge", cut, shorterDiffers, credited, refused)
	}
}

// offsetAuthority is an authority authorityBefore found in a part of a body that starts at
// from, as an index of the whole body; -1, no authority, stays -1.
func offsetAuthority(at, from int) int {
	if at < 0 {
		return at
	}
	return at + from
}

// citedIssuesOverTheWholeBody is citedIssues as it read before each decision was handed only
// citationReach bytes before its path: every path is decided over the body from its start. The
// differential row holds citedIssues to its answers.
func citedIssuesOverTheWholeBody(repo, body string) []string {
	type citation struct {
		at  int
		url string
	}
	citations := []citation{}
	for _, match := range githubIssuePath.FindAllStringSubmatchIndex(body, -1) {
		if !citesGitHubIssue(body[:match[1]], match[0], nil) {
			continue
		}
		citations = append(citations, citation{match[0], "https://github.com/" + body[match[2]:match[3]] + "/" + body[match[4]:match[5]] + "/issues/" + body[match[6]:match[7]]})
	}
	for _, match := range githubBareReference.FindAllStringSubmatchIndex(body, -1) {
		citations = append(citations, citation{match[2], "https://github.com/" + repo + "/issues/" + body[match[2]:match[3]]})
	}
	slices.SortStableFunc(citations, func(a, b citation) int { return cmp.Compare(a.at, b.at) })
	urls := []string{}
	for _, c := range citations {
		urls = appendUnique(urls, c.url)
	}
	return urls
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
		strings.Repeat("/a/b/issues/1", 1<<20/13)+",github.com/acme/widgets/issues/7",
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
