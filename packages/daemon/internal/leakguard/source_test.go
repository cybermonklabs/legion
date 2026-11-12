package leakguard

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// sourceToken is the read-only token the tests' Source mints, and the bearer the fake GitHub
// requires of every read.
const sourceToken = "ghs_leak_check_read"

// tarballRoute is the path GitHub serves the check's repository's tarball under, a tag after it.
const tarballRoute = "/repos/" + CheckSlug + "/tarball/"

// checkGitHub is a fake GitHub serving the tarball route of the check's repository, with a count
// of the reads made at each tag. answer serves a read at tag; any other route fails the test.
type checkGitHub struct {
	url string

	mu    sync.Mutex
	reads map[string]int
}

func serveCheck(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, tag string)) *checkGitHub {
	t.Helper()
	fake := &checkGitHub{reads: map[string]int{}}
	fake.url = githubtest.Server(t, githubtest.Spec{Token: sourceToken, Handle: func(w http.ResponseWriter, r *http.Request) {
		tag, routed := strings.CutPrefix(r.URL.Path, tarballRoute)
		if !routed || r.Method != http.MethodGet {
			githubtest.Unexpected(t, w, r)
			return
		}
		fake.mu.Lock()
		fake.reads[tag]++
		fake.mu.Unlock()
		answer(w, r, tag)
	}})
	return fake
}

// readsOf is how many reads the fake was asked at tag.
func (f *checkGitHub) readsOf(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[tag]
}

// total is a fake's whole read count, every tag together.
func (f *checkGitHub) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.reads {
		total += n
	}
	return total
}

// sourceOver is a Source reading url, whose every mint is counted in mints.
func sourceOver(url string, mints *atomic.Int32) *Source {
	return &Source{API: url, Token: func(context.Context) (string, error) {
		mints.Add(1)
		return sourceToken, nil
	}}
}

// answerCheck answers every read with a tarball of a check that runs, its launcher marked with
// the tag read, so two tags serve two bodies.
func answerCheck(t *testing.T) func(w http.ResponseWriter, r *http.Request, tag string) {
	return func(w http.ResponseWriter, _ *http.Request, tag string) {
		_, _ = w.Write(wholeCheck(t, marked(tag)))
	}
}

// Launchers that ask at once share one read: eight goroutines released together from a barrier
// are served one body from one request. A second ask at that tag reads nothing, an ask at a
// second tag reads that tag once, and a fresh Source — a daemon that restarted — reads again.
func TestOneReadServesEveryLauncherThatAsksForATag(t *testing.T) {
	fake := serveCheck(t, answerCheck(t))
	var mints atomic.Int32
	source := sourceOver(fake.url, &mints)

	const launchers = 8
	ctx := bound(t)
	var ready, finished sync.WaitGroup
	ready.Add(launchers)
	start := make(chan struct{})
	served := make([]Served, launchers)
	errs := make([]error, launchers)
	for i := range launchers {
		finished.Go(func() {
			ready.Done()
			<-start
			served[i], errs[i] = source.Get(ctx, theTag)
		})
	}
	ready.Wait()
	close(start)
	finished.Wait()
	for i := range launchers {
		if errs[i] != nil {
			t.Fatalf("launcher %d was refused: %v", i, errs[i])
		}
		if served[i].SHA256 != served[0].SHA256 || string(served[i].Body) != string(served[0].Body) {
			t.Fatalf("launcher %d was served %s, launcher 0 %s: want one body", i, served[i].SHA256, served[0].SHA256)
		}
	}
	if reads, minted := fake.readsOf(theTag), mints.Load(); reads != 1 || minted != 1 {
		t.Fatalf("%d launchers asking together made %d reads and %d mints, want one of each", launchers, reads, minted)
	}

	if _, err := source.Get(bound(t), theTag); err != nil {
		t.Fatalf("a second ask at %s: %v", theTag, err)
	}
	if reads, minted := fake.readsOf(theTag), mints.Load(); reads != 1 || minted != 1 {
		t.Fatalf("a second ask at %s made the reads %d and the mints %d, want the kept copy and no read", theTag, reads, minted)
	}

	const second = "leak-check-v16"
	other, err := source.Get(bound(t), second)
	if err != nil {
		t.Fatalf("an ask at %s: %v", second, err)
	}
	if other.Tag != second || other.SHA256 == served[0].SHA256 {
		t.Fatalf("the ask at %s was served %s at %s, want its own body", second, other.SHA256, other.Tag)
	}
	if reads := fake.readsOf(second); reads != 1 || fake.total() != 2 {
		t.Fatalf("an ask at %s made %d reads of it and %d in all, want one more read", second, reads, fake.total())
	}

	restarted := sourceOver(fake.url, &mints)
	again, err := restarted.Get(bound(t), theTag)
	if err != nil {
		t.Fatalf("a fresh Source's ask at %s: %v", theTag, err)
	}
	if again.SHA256 != served[0].SHA256 {
		t.Fatalf("a fresh Source was served %s, want the body at %s, %s", again.SHA256, theTag, served[0].SHA256)
	}
	if reads := fake.readsOf(theTag); reads != 2 {
		t.Fatalf("a fresh Source's ask made %d reads at %s, want it to read again: 2", reads, theTag)
	}
}

// The body the source serves is the leak-check directory alone, the paths its own with no
// repository prefix, and its digest is the body's SHA-256.
func TestTheServedBodyIsTheDirectoryAloneWithItsDigest(t *testing.T) {
	files := theCheck(answersItsHelp)
	raw := tarball(t, files)
	fake := serveCheck(t, func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write(raw) })
	var mints atomic.Int32
	got, err := sourceOver(fake.url, &mints).Get(bound(t), theTag)
	if err != nil {
		t.Fatalf("the read: %v", err)
	}
	sum := sha256.Sum256(got.Body)
	if got.Tag != theTag || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("served %s at %s, want %s at the body's SHA-256 %x", got.SHA256, got.Tag, theTag, sum)
	}
	members := membersOf(t, got.Body)
	names := slices.Sorted(maps.Keys(members))
	if want := slices.Sorted(maps.Keys(files)); !slices.Equal(names, want) {
		t.Fatalf("the served body holds %q, want the directory's own files alone, %q", names, want)
	}
	for name, m := range files {
		if member := members[name]; member.typeflag != tar.TypeReg || member.body != m.body || member.mode != m.mode {
			t.Errorf("the served body holds %s as (type %q, mode %o, %q), want a regular file of mode %o holding %q",
				name, member.typeflag, member.mode, member.body, m.mode, m.body)
		}
	}
}

// A tag not of the published shape is refused before anything is minted or asked of GitHub: it
// would otherwise become a path of a GitHub API endpoint.
func TestABadTagIsRefusedBeforeAnyMintOrRequest(t *testing.T) {
	fake := serveCheck(t, func(w http.ResponseWriter, r *http.Request, _ string) { githubtest.Unexpected(t, w, r) })
	var mints atomic.Int32
	source := sourceOver(fake.url, &mints)
	for _, tag := range []string{"", "v15", "leak-check-v0", "leak-check-v015", "leak-check-v15/../../../user", "leak-check-v15\n", "LEAK-CHECK-V15"} {
		_, err := source.Get(bound(t), tag)
		want := RefusalPrefix + fmt.Sprintf("the pod asked for the tag %q, which is not of the published shape leak-check-v<n>", tag)
		if err == nil || err.Error() != want {
			t.Errorf("the tag %q = %v, want %q", tag, err, want)
		}
	}
	if minted := mints.Load(); minted != 0 || fake.total() != 0 {
		t.Fatalf("bad tags made %d mints and %d reads, want none", minted, fake.total())
	}
}

// GitHub refusing the read is refused with its status and the first line of its answer, and a
// refused read is not kept: the next ask reads again.
func TestARefusedReadIsRefusedWithGitHubsAnswerAndNotKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer string
	}{
		{"a tag the repository does not hold", http.StatusNotFound,
			`{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/contents#download-a-repository-archive-tar","status":"404"}`},
		{"a token the repository does not admit", http.StatusForbidden,
			`{"message":"Resource not accessible by integration","status":"403"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refuse atomic.Bool
			refuse.Store(true)
			fake := serveCheck(t, func(w http.ResponseWriter, _ *http.Request, tag string) {
				if refuse.Load() {
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprintln(w, tc.answer)
					_, _ = fmt.Fprintln(w, "a second line the cause does not carry")
					return
				}
				_, _ = w.Write(wholeCheck(t, marked(tag)))
			})
			var mints atomic.Int32
			source := sourceOver(fake.url, &mints)
			_, err := source.Get(bound(t), theTag)
			want := fmt.Sprintf("%sGitHub answered the read of theorem-labs/agent-infra at %s with HTTP %d: %s", RefusalPrefix, theTag, tc.status, tc.answer)
			if err == nil || err.Error() != want {
				t.Fatalf("the refused read = %v, want %q", err, want)
			}

			refuse.Store(false)
			if _, err := source.Get(bound(t), theTag); err != nil {
				t.Fatalf("the ask after a refused read: %v", err)
			}
			if reads, minted := fake.readsOf(theTag), mints.Load(); reads != 2 || minted != 2 {
				t.Fatalf("the ask after a refused read made the reads %d and the mints %d, want it read again: 2 and 2", reads, minted)
			}
		})
	}
}

// A tarball with no leak-check directory is refused naming the tarball and the tag.
func TestATarballWithoutTheDirectoryIsRefused(t *testing.T) {
	raw := gzipTar(t, func(archive *tar.Writer) {
		writeFile(t, archive, repositoryPrefix+"README.md", member{body: "the rest of the repository\n", mode: 0o644})
	})
	fake := serveCheck(t, func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write(raw) })
	var mints atomic.Int32
	_, err := sourceOver(fake.url, &mints).Get(bound(t), theTag)
	want := RefusalPrefix + "the tarball of theorem-labs/agent-infra at " + theTag + " holds no leak-check directory"
	if err == nil || err.Error() != want {
		t.Fatalf("a tarball without the directory = %v, want %q", err, want)
	}
}

// A mint that fails is refused with the sentence the token function wrote, asks GitHub nothing,
// and is not kept.
func TestAFailedMintIsRefusedWithItsSentenceAndNotKept(t *testing.T) {
	fake := serveCheck(t, answerCheck(t))
	var mints atomic.Int32
	const sentence = "the daemon could not mint its read-only token for theorem-labs/agent-infra: the App has no installation on theorem-labs"
	source := &Source{API: fake.url, Token: func(context.Context) (string, error) {
		if mints.Add(1) == 1 {
			return "", errors.New(sentence)
		}
		return sourceToken, nil
	}}
	if _, err := source.Get(bound(t), theTag); err == nil || err.Error() != RefusalPrefix+sentence {
		t.Fatalf("a failed mint = %v, want %q", err, RefusalPrefix+sentence)
	}
	if fake.total() != 0 {
		t.Fatalf("a failed mint made %d reads, want none", fake.total())
	}
	if _, err := source.Get(bound(t), theTag); err != nil {
		t.Fatalf("the ask after a failed mint: %v", err)
	}
	if minted, reads := mints.Load(), fake.readsOf(theTag); minted != 2 || reads != 1 {
		t.Fatalf("the ask after a failed mint made %d mints and %d reads, want the mint again and the read: 2 and 1", minted, reads)
	}
}

// A read GitHub never answers ends at the read's bound, with a sentence naming the bound.
func TestAReadThatDoesNotAnswerEndsAtItsBound(t *testing.T) {
	fake := serveCheck(t, func(_ http.ResponseWriter, r *http.Request, _ string) { <-r.Context().Done() })
	var mints atomic.Int32
	source := sourceOver(fake.url, &mints)
	source.ReadBound = 50 * time.Millisecond
	_, err := source.Get(bound(t), theTag)
	want := RefusalPrefix + "the daemon's read of theorem-labs/agent-infra at " + theTag + " did not finish within 50ms"
	if err == nil || err.Error() != want {
		t.Fatalf("a read that does not answer = %v, want %q", err, want)
	}
}

// A caller that stops waiting is refused alone: the read it started goes on, and another caller
// waiting on it is served the body, from that one read.
func TestACallerThatGivesUpDoesNotEndTheReadOthersWaitOn(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fake := serveCheck(t, func(w http.ResponseWriter, r *http.Request, tag string) {
		once.Do(func() { close(arrived) })
		<-release
		if err := r.Context().Err(); err != nil {
			t.Errorf("the read ended with its first caller: %v", err)
		}
		_, _ = w.Write(wholeCheck(t, marked(tag)))
	})
	var mints atomic.Int32
	source := sourceOver(fake.url, &mints)

	waiting := bound(t)
	gaveUp, giveUp := context.WithCancel(waiting)
	first := make(chan error, 1)
	go func() {
		_, err := source.Get(gaveUp, theTag)
		first <- err
	}()
	<-arrived
	type answer struct {
		served Served
		err    error
	}
	second := make(chan answer, 1)
	go func() {
		served, err := source.Get(waiting, theTag)
		second <- answer{served, err}
	}()
	giveUp()
	err := <-first
	want := RefusalPrefix + "the start stopped waiting for the daemon's read of theorem-labs/agent-infra at " + theTag + ": context canceled"
	if err == nil || err.Error() != want {
		t.Fatalf("the caller that gave up = %v, want %q", err, want)
	}
	close(release)
	got := <-second
	if got.err != nil || len(got.served.Body) == 0 {
		t.Fatalf("the caller still waiting = %d bytes, %v; want the body", len(got.served.Body), got.err)
	}
	if reads := fake.readsOf(theTag); reads != 1 {
		t.Fatalf("%d reads, want the one both callers waited on", reads)
	}
}

// GitHub answers the tarball route with a redirect to its download host. The read asks with the
// token and GitHub's headers, follows the redirect, and hands the download host no token.
func TestTheReadFollowsGitHubsRedirectAndHandsTheDownloadHostNoToken(t *testing.T) {
	const downloads = "codeload.leak-check.test"
	raw := tarball(t, theCheck(answersItsHelp))
	var downloadAuth atomic.Value
	download := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		downloadAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(raw)
	}})
	var asked atomic.Value
	api := githubtest.Server(t, githubtest.Spec{Token: sourceToken,
		Record: func(r *http.Request) { asked.Store(r.Header.Clone()) },
		Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tarballRoute+theTag {
				githubtest.Unexpected(t, w, r)
				return
			}
			http.Redirect(w, r, "http://"+downloads+"/theorem-labs/agent-infra/legacy.tar.gz/refs/tags/"+theTag, http.StatusFound)
		}})
	dialer := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == downloads+":80" {
			addr = download.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, addr)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	var mints atomic.Int32
	source := sourceOver(api, &mints)
	source.Client = &http.Client{Transport: transport}

	got, err := source.Get(bound(t), theTag)
	if err != nil {
		t.Fatalf("the read through the redirect: %v", err)
	}
	if string(got.Body) != string(served(t, raw)) {
		t.Fatalf("the read served %d bytes, want the download host's tarball packed", len(got.Body))
	}
	headers, _ := asked.Load().(http.Header)
	for name, want := range map[string]string{
		"Authorization": "Bearer " + sourceToken, "Accept": "application/vnd.github+json", "X-Github-Api-Version": "2022-11-28",
	} {
		if headers.Get(name) != want {
			t.Errorf("the read asked GitHub with %s %q, want %q", name, headers.Get(name), want)
		}
	}
	if auth, _ := downloadAuth.Load().(string); auth != "" {
		t.Fatalf("the download host was handed Authorization %q, want none", auth)
	}
}
