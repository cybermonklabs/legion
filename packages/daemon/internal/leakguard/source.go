package leakguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// leakCheckReadBound is how long one read of the check's repository at a tag has: the mint of its
// token, GitHub's answer with the redirect it follows, and the pack of the tarball together.
// Measured from a pod on 2026-11-09, the whole fetch took 3.3 s, so two minutes is a slow GitHub
// many times over and still ends a read that hangs rather than every start that waits on it.
const leakCheckReadBound = 2 * time.Minute

// refusalBodyBytes is how much of GitHub's answer a refused read keeps: its first line is what
// the cause sentence quotes, and GitHub's refusals are a line of JSON.
const refusalBodyBytes = 64 << 10

// CheckRepository is the check's repository (CheckSlug), whose tarball a read fetches and which
// the daemon's token for that read is narrowed to.
var CheckRepository = ghrepo.MustParse(CheckSlug)

// Source is the daemon's reader of the shared public leak check: it reads the check's repository
// (CheckSlug) at the tag a pod's launcher names, with a read-only token of the daemon's own, and
// answers the leak-check directory alone, packed (PackTarball), for the daemon to serve to that
// launcher. It runs no gh and reads no gh configuration.
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
	// Token mints the read-only token the read is made with. Its error's text is what happened,
	// in the words a cause sentence takes after RefusalPrefix (Cause): the daemon, which mints
	// through its GitHub App and tells a mint GitHub refused from a narrowing it refused, writes
	// that sentence, so this package imports nothing of the App's.
	Token func(context.Context) (string, error)
	// API is GitHub's REST root; empty is https://api.github.com.
	API string
	// Client makes the read; nil is http.DefaultClient. Its redirect policy is Go's own, which
	// follows GitHub's redirect to the tarball's host and drops the Authorization header on the
	// way to another host.
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
// (PackTarball) and Frames the launcher_leak_check sequence that carries it
// (shimwire.SplitLeakCheck), each frame declaring the body's digest. Body and Frames are shared by
// every caller served the same read, so no caller writes to them.
type Served struct {
	Tag    string
	Body   []byte
	Frames []shimwire.LauncherLeakCheck
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
		return Served{}, errors.New(Cause(fmt.Sprintf("the start stopped waiting for the daemon's read of %s at %s", CheckSlug, tag), ctx.Err()))
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
// read is refused.
func (s *Source) read(ctx context.Context, tag string, bound time.Duration) (Served, error) {
	token, err := s.Token(ctx)
	if err != nil {
		return Served{}, errors.New(Cause(err.Error(), nil))
	}
	// GitHub answers the tarball route with a redirect to its download host
	// (https://docs.github.com/en/rest/repos/contents#download-a-repository-archive-tar), which
	// the client follows.
	target := githubrest.RepositoryAPI(s.API, CheckRepository) + "/tarball/" + tag
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Served{}, errors.New(Cause(fmt.Sprintf("the daemon could not ask GitHub for %s at %s", CheckSlug, tag), err))
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return Served{}, readFailure(ctx, tag, bound, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized && s.Forget != nil {
			s.Forget()
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyBytes))
		what := fmt.Sprintf("GitHub answered the read of %s at %s with HTTP %d", CheckSlug, tag, response.StatusCode)
		if strings.TrimSpace(string(body)) != "" {
			what += ": " + firstLine(string(body))
		}
		return Served{}, errors.New(Cause(what, nil))
	}
	body, err := PackTarball(response.Body)
	if err != nil {
		if ctx.Err() != nil {
			return Served{}, readFailure(ctx, tag, bound, err)
		}
		return Served{}, errors.New(Cause(fmt.Sprintf("the tarball of %s at %s %s", CheckSlug, tag, err), nil))
	}
	frames, err := shimwire.SplitLeakCheck(tag, body)
	if err != nil {
		return Served{}, errors.New(Cause(fmt.Sprintf("the daemon could not slice the check it read at %s", tag), err))
	}
	return Served{Tag: tag, Body: body, Frames: frames}, nil
}

// readFailure is the cause of a read that did not arrive: the bound's own sentence where the
// read's bound ended it, and the transport's error otherwise.
func readFailure(ctx context.Context, tag string, bound time.Duration, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New(Cause(fmt.Sprintf("the daemon's read of %s at %s did not finish within %s", CheckSlug, tag, bound), nil))
	}
	return errors.New(Cause(fmt.Sprintf("the daemon could not read %s at %s", CheckSlug, tag), err))
}
