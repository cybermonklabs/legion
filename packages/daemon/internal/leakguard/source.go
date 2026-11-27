package leakguard

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/oauth2call"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/urljoin"
)

// leakCheckReadBound is how long one read of the check's repository at a tag has: the mint of its
// token, GitHub's answer with the redirect it follows, and the pack of the tarball together.
// Measured from a pod on 2026-11-09, the whole fetch took 3.3 s, so two minutes is a slow GitHub
// many times over and still ends a read that hangs rather than every start that waits on it.
const leakCheckReadBound = 2 * time.Minute

// refusalBodyBytes is how much of GitHub's answer a refused read keeps: its first line is what
// the cause sentence quotes, and GitHub's refusals are a line of JSON.
const refusalBodyBytes = 64 << 10

// leakCheckCaller is one of the daemon's caller kinds (githubrate.WithCaller, internal/daemon's
// rate reading): the read of the shared leak check, its token's mint included, which the reading
// counts its calls on the installation the token is of by.
const leakCheckCaller = "leak_check"

// Source is the daemon's reader of the shared public leak check: it reads the check's repository
// (Repository, the deployment's Setting.Repository) at the tag a pod's launcher names, with a
// read-only token of the daemon's own, and answers the leak-check directory alone, packed
// (PackTarballManifest), for the daemon to serve to that launcher. It runs no gh and reads no gh
// configuration. The daemon builds one only for a deployment that names a check
// (Setting.Configured).
//
// It keeps one copy per tag. Launchers that ask at once wait on the one read the first of them
// started; a read that succeeded is kept for the life of the process, so every later ask at that
// tag is answered from memory, and a daemon that restarts reads again. A read that failed is not
// kept: the next ask at that tag reads again. A tag is trusted not to move. No ruleset protects a
// tag of the check's repository, so a tag force-moved there is served from the copy read before
// the move until the daemon restarts.
//
// A Source is used through a pointer and never copied once it has been asked.
type Source struct {
	// Repository is the check's repository, whose tarball a read fetches, with a token minted on
	// its owner's installation (Token); every cause sentence names it.
	Repository ghrepo.Repository
	// Token mints the read-only token the read is made with. Its error's text is what happened,
	// in the words a cause sentence takes after RefusalPrefix (Cause): the daemon, which mints
	// through its GitHub App and tells a mint GitHub refused from a narrowing it refused, writes
	// that sentence, so this package imports nothing of the App's.
	Token func(context.Context) (string, error)
	// API is GitHub's REST root; empty is https://api.github.com.
	API string
	// Client makes the read; nil is the daemon's REST client (githubrest.HTTPClient), whose calls
	// the daemon's rate reading counts. Unlike the daemon's other clients (redirects.Refused), the
	// read follows redirects, ten at most as Go's own policy counts them: GitHub answers the
	// tarball route with a redirect to its download host, whose URL carries the download's own
	// token, and a renamed repository's route with a 301 to /repositories/<id> on the API's own
	// origin. The read sends its token on only to a redirect on the origin it asked (originToken,
	// which sets the CheckRedirect of a copy of Client, so Client's own is never asked).
	Client *http.Client
	// ReadBound is how long one read has; zero is leakCheckReadBound.
	ReadBound time.Duration
	// Forget, when set, drops what Token holds of the token it minted, so the next mint is a new
	// token: the read calls it where GitHub answered the tarball read 401, a token GitHub no longer
	// takes. Nil is a Token that holds nothing.
	Forget func()

	mu    sync.Mutex
	reads map[string]*tagRead
}

// tagRead is one read of the check at a tag: done is closed once served or err is set, and
// neither changes after.
type tagRead struct {
	done   chan struct{}
	served Served
	err    error
}

// Served is the check at Tag as the daemon serves it: Body is the packed leak-check directory
// (PackTarballManifest), ManifestSHA256 is the manifest of the files in that exact body, and
// Frames the launcher_leak_check sequence that carries it, each frame declaring the body's digest.
// Body and Frames are shared by every caller served the same read, so no caller writes to them.
type Served struct {
	Tag            string
	Body           []byte
	ManifestSHA256 string
	Frames         []shimwire.LauncherLeakCheck
}

// Get answers the check at tag, read once for every caller (Source). Every error is one cause
// sentence (Cause).
//
// A tag not of the published shape (shimwire.ValidLeakCheckTag) is refused before any token is
// minted or any request made, since a tag becomes a path of a GitHub API endpoint. Otherwise Get
// waits for the read at tag, starting one where none is kept or running. The read runs detached
// from ctx, under its own bound (ReadBound): a caller whose ctx ends stops waiting and is refused,
// and the read goes on for every other caller waiting on it.
func (s *Source) Get(ctx context.Context, tag string) (Served, error) {
	if !shimwire.ValidLeakCheckTag(tag) {
		return Served{}, errors.New(Cause(fmt.Sprintf("the pod asked for the tag %q, which is not of the published shape leak-check-v<n>", tag), nil))
	}
	r := s.reading(ctx, tag)
	select {
	case <-r.done:
		return r.served, r.err
	case <-ctx.Done():
		return Served{}, errors.New(Cause(fmt.Sprintf("the start stopped waiting for the daemon's read of %s at %s", s.Repository, tag), ctx.Err()))
	}
}

// reading is the read at tag: the one kept or running, or a new one, started on a context
// detached from ctx.
func (s *Source) reading(ctx context.Context, tag string) *tagRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, kept := s.reads[tag]; kept {
		return r
	}
	if s.reads == nil {
		s.reads = map[string]*tagRead{}
	}
	r := &tagRead{done: make(chan struct{})}
	s.reads[tag] = r
	go s.fill(context.WithoutCancel(ctx), tag, r)
	return r
}

// fill makes the read r of tag under the read bound. A failed read is removed before its waiters
// are released, so an ask that follows a failure reads again.
func (s *Source) fill(ctx context.Context, tag string, r *tagRead) {
	bound := s.ReadBound
	if bound <= 0 {
		bound = leakCheckReadBound
	}
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	r.served, r.err = s.read(bounded, tag, bound)
	if r.err != nil {
		s.mu.Lock()
		delete(s.reads, tag)
		s.mu.Unlock()
	}
	close(r.done)
}

// read mints the token, fetches GitHub's tarball of the check's repository at tag, packs its
// leak-check directory and slices it into the sequence that carries it. ctx carries the read's
// bound, which a refusal names where it ended the read. A 401 drops the token (Forget) before the
// read is refused. Its calls, the mint's among them, count as leakCheckCaller's.
func (s *Source) read(ctx context.Context, tag string, bound time.Duration) (Served, error) {
	ctx = githubrate.WithCaller(ctx, leakCheckCaller)
	token, err := s.Token(ctx)
	if err != nil {
		return Served{}, errors.New(Cause(err.Error(), nil))
	}
	// GitHub answers the tarball route with a redirect to its download host
	// (https://docs.github.com/en/rest/repos/contents#download-a-repository-archive-tar), which
	// the read follows with no Authorization, and a renamed repository's with a 301 on the API's
	// own origin, which it follows with its token (originToken).
	//
	// The tarball's path is joined onto the repository's REST base (urljoin.Join), so an API
	// address configured with a path and a query asks that path with the tarball's under it and
	// keeps the query. A base that cannot be read as an absolute URL with a host is refused in the
	// join's fixed words, which print no URL.
	target, err := urljoin.Join(githubrest.RepositoryAPI(s.API, s.Repository), "/tarball/"+tag)
	if err != nil {
		return Served{}, errors.New(Cause(fmt.Sprintf("the daemon could not ask GitHub for %s at %s", s.Repository, tag), err))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Served{}, errors.New(Cause(fmt.Sprintf("the daemon could not ask GitHub for %s at %s", s.Repository, tag), oauth2call.Unanswered(err, nil, "")))
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := originToken(cmp.Or(s.Client, githubrest.HTTPClient())).Do(request)
	if err != nil {
		return Served{}, s.readFailure(ctx, tag, bound, oauth2call.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized && s.Forget != nil {
			s.Forget()
		}
		// GitHub's message, or a stand-in's words, print as the rule reads them, with the token the
		// read carried redacted (oauth2call.Answer.Decode into oauth2call.GitHubRefusal, Words): the
		// message alone where the answer is GitHub's JSON refusal, whose documentation URL in compact
		// JSON the rule cannot read as clean, and the answer's words otherwise. A read its bound cut,
		// or that ended on an error, can stop inside a URL before its "@", which redact.Read drops
		// rather than quote.
		refused, readErr := oauth2call.ReadAnswer(response, refusalBodyBytes)
		var github oauth2call.GitHubRefusal
		said := refused.Words()
		if refused.Decode(&github) == nil && !github.Message.IsEmpty() {
			said = github.Message
		}
		body := redact.Read(said.String(), readErr != nil)
		what := fmt.Sprintf("GitHub answered the read of %s at %s with HTTP %d", s.Repository, tag, response.StatusCode)
		if strings.TrimSpace(body) != "" {
			what += ": " + firstLine(body)
		}
		return Served{}, errors.New(Cause(what, nil))
	}
	// The tarball is data the daemon unpacks and serves, streamed from the answer, whose record an
	// error naming a member is read with (PackTarballManifest); the pack bounds what it unpacks.
	body, manifest, err := PackTarballManifest(response)
	if err != nil {
		if ctx.Err() != nil {
			return Served{}, s.readFailure(ctx, tag, bound, err)
		}
		return Served{}, errors.New(Cause(fmt.Sprintf("the tarball of %s at %s %s", s.Repository, tag, err), nil))
	}
	frames, err := shimwire.SplitLeakCheck(tag, body)
	if err != nil {
		return Served{}, errors.New(Cause(fmt.Sprintf("the daemon could not slice the check it read at %s", tag), err))
	}
	return Served{Tag: tag, Body: body, ManifestSHA256: manifest, Frames: frames}, nil
}

// readFailure is the cause of a read that did not arrive: the bound's own sentence where the
// read's bound ended it, and the transport's error otherwise.
func (s *Source) readFailure(ctx context.Context, tag string, bound time.Duration, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New(Cause(fmt.Sprintf("the daemon's read of %s at %s did not finish within %s", s.Repository, tag, bound), nil))
	}
	return errors.New(Cause(fmt.Sprintf("the daemon could not read %s at %s", s.Repository, tag), err))
}

// originToken is client following redirects as Go's own policy does, with the read's token sent
// on only to a redirect on the read's own origin (keepTokenOnOrigin). net/http copies the read's
// Authorization onto a redirect's request wherever its host name is the API's or a name under it,
// whatever its scheme and port (shouldCopyHeaderOnRedirect, net/http/client.go of go1.27.3, lines
// 700-705 and 1017-1039). GitHub's redirect to its download host needs none: its URL carries the
// download's own token. A renamed repository's 301 to /repositories/<id> on the API's origin
// needs it to read a private repository. A copy, as redirects.Refused makes: the caller's client
// is its own.
func originToken(client *http.Client) *http.Client {
	copied := *client
	copied.CheckRedirect = keepTokenOnOrigin
	return &copied
}

// keepTokenOnOrigin is the CheckRedirect of a client originToken made: next, a redirect's
// request, keeps the Authorization net/http copied onto it from the read, the one credential the
// read sets, only where its origin is the read's (via[0]'s scheme, and its host and port as
// redirects.SameOrigin compares them), and is sent without it otherwise; and only within ten
// requests, as Go's own policy (net/http's defaultCheckRedirect) counts them.
func keepTokenOnOrigin(next *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if next.URL.Scheme != first.Scheme || !strings.EqualFold(next.URL.Host, first.Host) {
		next.Header.Del("Authorization")
	}
	return nil
}
