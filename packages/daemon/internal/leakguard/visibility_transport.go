package leakguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The reads the guard makes when gh's REST read of a visibility fails on its connection to GitHub
// and GitHub answered nothing (Guard.readWithoutGh). In a busy one-CPU pod, every gh call failed
// with `net/http: TLS handshake timeout` for over two hours, while curl and git, whose TLS is
// OpenSSL's through libcurl, finished their handshakes with the same hosts in under a second. The
// guard refused every push as a setup error for that whole time.

// transportError is a read that failed on its connection to GitHub before GitHub answered
// anything: a visibility read, or cmd/legion's fork read (CurlResponse.Unreached). program is the
// program that made the read (gh or curl), and said is what it printed about the failure, with the
// repository's name masked where the read masks it: the visibility read does, and the fork read,
// which names the repository anyway, does not.
type transportError struct {
	program string
	said    string
}

func (e *transportError) Error() string {
	return e.program + " could not reach GitHub: " + e.said
}

// gitHubAPI is the origin of GitHub's REST API, which gh's REST read (`gh api --hostname
// github.com`) and the curl read both ask.
const gitHubAPI = "https://api.github.com"

// ghTransportFailures are the endings of the line gh prints when its REST read failed on its
// connection. Each is the wording of Go's own net or net/http package, and each names a failure
// before any response arrived:
//
//   - "net/http: TLS handshake timeout": the HTTP transport's TLSHandshakeTimeout ended the
//     handshake. This is the failure of the busy one-CPU pod.
//   - "i/o timeout": a socket deadline ended a connect or a read, as in
//     `dial tcp 140.82.112.5:443: i/o timeout`, the wording of Go's dial timeout.
//   - "connection reset by peer": the connection was dropped (ECONNRESET) before any answer came.
//   - "connection refused": nothing accepted the connection (ECONNREFUSED), at GitHub or at the
//     proxy the environment names.
//   - "context deadline exceeded (Client.Timeout exceeded while awaiting headers)": the HTTP
//     client's own Timeout ended the request before any response header came. The caller's
//     bound is never this: a gh that the caller's bound stops is killed and is a *stoppedError.
var ghTransportFailures = []string{
	"net/http: TLS handshake timeout",
	"i/o timeout",
	"connection reset by peer",
	"connection refused",
	"context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
}

// ghTransportFailure is the transport failure that gh's REST read of slug reports, where gh exited
// with status after it printed printed, and nil for any other outcome. It is one only where all of
// these hold:
//
//   - gh exited non-zero;
//   - gh printed exactly one line, `Get "https://api.github.com/repos/<slug>": <cause>`, the form
//     of Go's error for the request itself (net/url's Error), naming this read's own URL. So gh
//     printed no status line, and GitHub sent no response: with --include, gh prints the status
//     line of every response, an error response included, on a line of its own;
//   - and the cause ends with one of ghTransportFailures.
//
// gh 2.98.0 printed exactly that line, and exited 1, for a TLS handshake that never finished, for
// a connection reset after the proxy's tunnel opened, and for a proxy that refused the connection
// (measured on 2026-11-18 through a local proxy). Every other outcome stays what it was: a GitHub
// error body, a 404, a 403 that is not a rate limit, a malformed answer, a refusal by `legion gh`,
// and a failed DNS lookup, which gh prints as `error connecting to api.github.com`.
func ghTransportFailure(status int, printed, slug string) *transportError {
	if status == 0 {
		return nil
	}
	line := strings.TrimSpace(printed)
	if strings.ContainsAny(line, "\r\n") {
		return nil
	}
	cause, ok := strings.CutPrefix(line, `Get "`+gitHubAPI+"/repos/"+slug+`": `)
	if !ok {
		return nil
	}
	for _, failure := range ghTransportFailures {
		if strings.HasSuffix(cause, failure) {
			return &transportError{program: "gh", said: strings.ReplaceAll(line, slug, maskPath(slug))}
		}
	}
	return nil
}

// ghReadBound is the longest the guard lets gh's REST read run (Guard.ghVisibility, through
// ghReadContext). A healthy read takes about a second: five reads of a repository's visibility by
// the real gh took 0.72 to 0.88 s on 2026-11-18. A gh whose TLS handshake cannot finish takes far
// longer to fail: about 42 s in the busy one-CPU pod, which is longer than the whole bound some
// callers give a read (`legion gh` gives the shared check's own read 15 s). Stopping gh sooner leaves
// the rest of the caller's bound to curl. It is a variable so that a test can shorten it.
var ghReadBound = 8 * time.Second

// curlReserve is how much of a caller's bound the guard keeps for the curl read when it bounds gh's
// REST read (ghReadContext): 5 s. In the pod where gh could not finish a handshake, curl answered
// GitHub in under a second. It is a variable so that a test can shorten it.
var curlReserve = 5 * time.Second

// ghReadContext is the context gh's REST read runs under: ctx bounded at ghReadBound, or, where ctx
// has a deadline, at that deadline less curlReserve if that is sooner. Where ctx's deadline leaves
// curlReserve or less, gh runs under ctx alone, as it did before this bound: what time there is is
// the caller's, and a stop is the caller's.
func ghReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound := ghReadBound
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - curlReserve
		if left <= 0 {
			return ctx, func() {}
		}
		bound = min(bound, left)
	}
	return context.WithTimeout(ctx, bound)
}

// stoppedByItsOwnBound is the stop err carries, and whether it is a stop by a bound of the guard's
// own rather than the caller's: a child that run stopped (a *stoppedError) while the caller's ctx
// has not ended. A bound of the guard's own is a context.WithTimeout of ctx whose cancel runs only
// after its read returns, so it stops a child by its deadline alone. Any other stop is ctx's end,
// by its deadline or its cancel, which leaves no bound for another read.
func stoppedByItsOwnBound(ctx context.Context, err error) (*stoppedError, bool) {
	var stopped *stoppedError
	return stopped, errors.As(err, &stopped) && ctx.Err() == nil
}

// ghStoppedByItsBound is the transport failure of a gh REST read of slug that the guard's own bound
// (ghReadContext) stopped: err is run's error, printed what gh printed before the stop, and ctx the
// caller's context. It is one only where the stop was the guard's own bound (stoppedByItsOwnBound)
// and where gh printed no status line, so GitHub sent no response. Anything else is nil: a stop by
// the caller's deadline or by its cancel stays the error it is, since no bound is left for another
// read, and so does a gh that heard GitHub's status line and then hung.
func ghStoppedByItsBound(ctx context.Context, err error, printed, slug string) *transportError {
	stopped, own := stoppedByItsOwnBound(ctx, err)
	if !own {
		return nil
	}
	if httpStatus, _ := responseHeaders(printed); httpStatus != 0 {
		return nil
	}
	message := stopped.Error()
	if line := strings.TrimSpace(printed); line != "" {
		message += ": " + line
	}
	return &transportError{program: "gh", said: strings.ReplaceAll(message, slug, maskPath(slug))}
}

// errNoCurlRead is a read through curl that the guard could not make: no curl at the guard's curl,
// or a token that holds a line break (Guard.CurlGet), or no token for the visibility read
// (Guard.curlVisibility).
var errNoCurlRead = errors.New("the read through curl could not be made")

// errNoTimeForCurl is a curl read that the guard did not make because less than curlLeast of the
// caller's bound was left for it (curlTime).
var errNoTimeForCurl = errors.New("too little of the caller's bound is left for the read through curl")

// readWithoutGh reads slug's visibility after gh's REST read failed on its connection to GitHub
// with no answer from GitHub (unreached, ghTransportFailure). It makes no GraphQL read, which goes
// through gh too, except where curl meets a spent core limit: then the reads after a rate limit
// follow (readWithoutREST), and the first of them is GraphQL's through gh.
//
// First it makes the same REST read through curl (Guard.curlVisibility). curl's public, private or
// internal is the answer, as gh's is, and it is kept as gh's is. A rate limit that curl's read meets
// goes on to the reads a rate-limited REST read goes on to (readWithoutREST). Where the curl read
// cannot be made (errNoCurlRead), or where curl fails on its connection too (a *transportError),
// the visibility is read with git's ls-remote and no credential (Guard.lsRemote), which answers
// public and nothing else. Every other outcome refuses the call, naming gh's failure and what each
// read said, GitHub's 404 to curl among them, which the refusal wraps as the *notFoundError it is.
// A read that the caller's bound stops refuses the call too, because no bound is left for another
// read, and so does too little time left for curl (errNoTimeForCurl), which leaves none for
// ls-remote either.
func (g *Guard) readWithoutGh(ctx context.Context, slug string, unreached *transportError) (Visibility, VisibilitySource, error) {
	visibility, err := g.curlVisibility(ctx, slug)
	var limited *rateLimitError
	switch {
	case err == nil:
		return visibility, VisibilityFromCurl, nil
	case errors.As(err, &limited):
		visibility, source, err := g.readWithoutREST(ctx, slug, limited)
		if err != nil {
			return "", "", fmt.Errorf("%w; the read through curl: %w", unreached, err)
		}
		return visibility, source, nil
	case errors.As(err, new(*transportError)), errors.Is(err, errNoCurlRead):
		visibility, lsErr := g.lsRemote(ctx, githubOrigin, slug)
		if lsErr != nil {
			return "", "", fmt.Errorf("%w, %w, and git ls-remote could not answer: %w", unreached, err, lsErr)
		}
		return visibility, VisibilityFromLsRemote, nil
	case errors.Is(err, errNoTimeForCurl):
		return "", "", fmt.Errorf("%w, and %w", unreached, err)
	default:
		return "", "", fmt.Errorf("%w, and curl could not answer: %w", unreached, err)
	}
}

// curlBound is the longest the curl read runs, its token read included (Guard.curlVisibility). In
// the pod where every gh call failed its TLS handshake, curl answered GitHub's REST API in under a
// second. A curl read that takes longer than this leaves the rest of the caller's bound to
// ls-remote.
const curlBound = 20 * time.Second

// curlMargin is how much sooner than its read's deadline curl's --max-time ends, so that curl
// stops itself and says why before the deadline kills it.
const curlMargin = 500 * time.Millisecond

// curlLeast is the least time the curl read is made with. With less left there is no time for
// curl or for ls-remote after it, and the call is refused as it was before these reads existed.
const curlLeast = time.Second

// curlTime is the --max-time of a curl read that must end by deadline: the time left before it,
// less curlMargin, and whether that is at least curlLeast.
func curlTime(deadline time.Time) (time.Duration, bool) {
	left := time.Until(deadline) - curlMargin
	return left, left >= curlLeast
}

// curlProgram is the curl the guard's reads through curl run where Options.Curl names none: the
// system's own, at a fixed path, and never the first curl on the caller's PATH, which is the
// caller's to put there and would be handed the token on its standard input.
const curlProgram = "/usr/bin/curl"

// curlVisibility is gh's REST read of slug made through curl, whose TLS is OpenSSL's: the same
// request (GET /repos/<slug> at gitHubAPI, which Guard.CurlGet makes) with the same token
// (Guard.ghToken), read by curlAnswer. Its bound is curlBound, or the caller's own deadline where
// that is sooner, and the token read runs inside it, so curl's --max-time is what the token read
// left of it.
//
// Less than curlLeast left, before the token read or after it, is errNoTimeForCurl, and no curl
// runs; the first check also spares the token read, and so does the check that the guard's curl
// is there, which CurlGet makes again before it runs curl. No curl there, and no token, are
// errNoCurlRead. A curl that its own bound stops is a *transportError. A curl or a token read that
// the caller's bound stops is the *stoppedError run answers.
func (g *Guard) curlVisibility(ctx context.Context, slug string) (Visibility, error) {
	bounded, cancel := context.WithTimeout(ctx, curlBound)
	defer cancel()
	deadline, _ := bounded.Deadline()
	if _, ok := curlTime(deadline); !ok {
		return "", errNoTimeForCurl
	}
	if err := g.curlMissing(); err != nil {
		return "", err
	}
	token, err := g.ghToken(bounded, slug)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", err
	case err != nil:
		return "", fmt.Errorf("%w: %w", errNoCurlRead, err)
	}
	response, err := g.CurlGet(bounded, "repos/"+slug, token)
	if err != nil {
		if stopped, own := stoppedByItsOwnBound(ctx, err); own {
			return "", &transportError{program: "curl", said: stopped.Error()}
		}
		return "", err
	}
	return curlAnswer(response.Exit, response.Stdout, response.Stderr, slug)
}

// curlMissing is errNoCurlRead, naming the path, where the guard's curl is no regular file, and
// nil where it is one.
func (g *Guard) curlMissing() error {
	if info, err := os.Stat(g.curl); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: there is no curl at %s", errNoCurlRead, g.curl)
	}
	return nil
}

// CurlGet is one GET of path at gitHubAPI, made through the guard's curl with token. path is a
// path of GitHub's REST API without its leading slash, such as `repos/<owner>/<name>`; the host is
// gitHubAPI, which no caller can change. It is the one read through curl there is: the guard's
// visibility read after gh failed on its connection (Guard.curlVisibility), and cmd/legion's
// credential helper's read of a fork, both make it here, so both send the token the same way.
//
// The token goes to curl on its standard input, as the `Authorization` header that `--header @-`
// reads, and never in its argv, which any process of the machine can read. curl keeps of the
// caller's environment only the names curlInherits keeps: its proxy, which then sees only a TLS
// tunnel, and no trust store, so the CAs curl trusts are the system's own and no proxy the caller
// names can read the token. curl runs in a process group of its own (Guard.run), which the end of
// ctx stops whole, with anything curl started. Its options are these:
//
//   - `-q`, first as curl requires, so curl reads no .curlrc, and `--no-netrc`, so it reads no
//     netrc: the token is the one credential the request carries;
//   - `--include` prints the status line and the headers, which CurlResponse reads apart from the
//     body, and `--suppress-connect-headers` leaves out a proxy's answer to CONNECT, which would
//     otherwise come first;
//   - `--proto =https` allows no other protocol, and no redirect is followed;
//   - `--max-time` is the time left before ctx's deadline, less curlMargin (curlTime).
//
// ctx's deadline is the read's bound, and a ctx with none is refused, since curl's --max-time
// needs one. Less than curlLeast left is errNoTimeForCurl. No curl at the guard's curl is
// errNoCurlRead, and so is a token that holds a line break, a carriage return included: curl reads
// the header on its standard input a line at a time, so the token would add a header of its own to
// the request. In each of these cases no curl runs. A curl that the end of ctx stopped is the
// *stoppedError run answers, and one that a signal ended is that end with what curl printed
// beneath it (procgroup.Said). A curl that exited is a CurlResponse and no error, whatever its
// exit status and whatever GitHub answered: the caller reads which (CurlResponse).
func (g *Guard) CurlGet(ctx context.Context, path, token string) (CurlResponse, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return CurlResponse{}, errors.New("the read through curl has no deadline, which curl's --max-time needs")
	}
	maxTime, ok := curlTime(deadline)
	if !ok {
		return CurlResponse{}, errNoTimeForCurl
	}
	if err := g.curlMissing(); err != nil {
		return CurlResponse{}, err
	}
	if strings.ContainsAny(token, "\r\n") {
		return CurlResponse{}, fmt.Errorf("%w: the token holds a line break, which would add a header of its own to the request", errNoCurlRead)
	}
	var stderr bytes.Buffer
	status, printed, err := g.run(ctx, child{
		argv: []string{g.curl, "-q", "--no-netrc", "--silent", "--show-error", "--include", "--suppress-connect-headers",
			"--proto", "=https", "--max-time", strconv.FormatFloat(maxTime.Seconds(), 'f', 1, 64),
			"--header", "@-", "--header", "Accept: application/vnd.github+json", "--header", "X-GitHub-Api-Version: 2022-11-28",
			gitHubAPI + "/" + path},
		inherit: curlInherits,
		stdin:   strings.NewReader("Authorization: Bearer " + token + "\n"),
		stderr:  &stderr,
	})
	if err != nil {
		return CurlResponse{}, procgroup.Said(err, visibilityBody(printed)+"\n"+stderr.String())
	}
	return CurlResponse{Exit: status, Stdout: printed, Stderr: stderr.String()}, nil
}

// CurlResponse is a GET through curl that curl ended itself (Guard.CurlGet). Exit is curl's exit
// status, which is 0 for any answer GitHub gave, whatever its HTTP status, since curl runs without
// --fail. Stdout is the status line, the headers and the body, as --include prints them, and
// Stderr is what curl said of a failure. Its methods read GitHub's answer out of Stdout with the
// guard's own parse of that output, the one curlAnswer reads the visibility read's answer with.
type CurlResponse struct {
	Exit   int
	Stdout string
	Stderr string
}

// Status is the HTTP status of GitHub's answer, 0 where curl printed no status line, and the
// answer's headers, each name lowercased (responseHeaders).
func (r CurlResponse) Status() (int, map[string]string) { return responseHeaders(r.Stdout) }

// Body is the body of GitHub's answer: what follows its headers (visibilityBody).
func (r CurlResponse) Body() string { return visibilityBody(r.Stdout) }

// RateLimit is the rate limit GitHub refused the read for, read as rateLimitFrom reads gh's
// answer, and nil where the answer reports none. Its text names the reset of a spent core limit
// in UTC, or the wait that the secondary limit's Retry-After asks for.
func (r CurlResponse) RateLimit() error {
	if limited := rateLimitFrom(r.Stdout); limited != nil {
		return limited
	}
	return nil
}

// Unreached is the *transportError of a curl that got no answer from GitHub (curlUnreached), and
// nil for any other: a curl that printed a status line heard GitHub, whatever its exit. Its text
// quotes curl as it printed, unmasked, for a caller that names the repository it read anyway; the
// visibility read masks it (curlAnswer).
func (r CurlResponse) Unreached() error {
	if httpStatus, _ := r.Status(); !curlUnreached(r.Exit, httpStatus) {
		return nil
	}
	return &transportError{program: "curl", said: fmt.Sprintf("exit %d: %s", r.Exit, strings.TrimSpace(strings.TrimSpace(r.Body())+" "+strings.TrimSpace(r.Stderr)))}
}

// curlInherits is which names of the caller's environment the curl read keeps: those a program
// carrying a GitHub token keeps (TokenChildKeeps), which hold neither GH_TOKEN nor GITHUB_TOKEN,
// since the token goes to curl on its standard input alone. The proxy variables are kept, so a
// host that reaches GitHub through a proxy reaches it the same way here, and the proxy sees only
// the TLS tunnel curl opens through it. Every other name is left out: the trust stores
// (SSL_CERT_FILE, SSL_CERT_DIR, CURL_CA_BUNDLE), by which a proxy would be trusted to read inside
// that tunnel, SSLKEYLOGFILE, a file OpenSSL writes the connection's TLS keys to, and any name a
// later curl learns to read.
func curlInherits(name string) bool { return TokenChildKeeps(name) }

// TokenChildKeeps is whether a program that carries a GitHub token keeps the name of its caller's
// environment: gh, which `legion gh` runs with an installation's token, and the guard's curl read.
// It keeps what any program needs to run (PATH, HOME, TMPDIR), the terminal (TERM), the locale and
// time zone (LANG and the LC_ names, TZ), and the proxy variables in both cases (HTTPS_PROXY,
// HTTP_PROXY, ALL_PROXY, NO_PROXY), which the daemon hands a pane or a pod as its route out
// (runtime/tmux's paneEnvAllowList, the guard's own ls-remote): gh and curl reach GitHub through
// such a proxy by a CONNECT tunnel, and with no trust store of the caller's, the proxy reads
// nothing inside it. Nothing else is kept: an allowlist, so that no trust store (SSL_CERT_FILE,
// SSL_CERT_DIR, CURL_CA_BUNDLE), no TLS key log (SSLKEYLOGFILE), no program the tool would hand
// text to (EDITOR, VISUAL, BROWSER, PAGER) and no name a later release learns to read reaches it
// from the caller. A proxy and a trust store the caller names together would hand the token to
// whatever the proxy is, so a deployment whose proxy inspects TLS with its own CA installs that
// CA in the system's trust store, which gh and curl read by default. A caller that needs more of
// its environment names it beside this, with what reads it.
func TokenChildKeeps(name string) bool {
	return tokenChildNames[name]
}

// tokenChildNames are the names TokenChildKeeps keeps, each by its whole name, as
// runtime/tmux's paneEnvAllowList lists the locale's.
var tokenChildNames = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TERM": true, "TZ": true, "LANG": true,
	"LC_ADDRESS": true, "LC_ALL": true, "LC_COLLATE": true, "LC_CTYPE": true, "LC_IDENTIFICATION": true,
	"LC_MEASUREMENT": true, "LC_MESSAGES": true, "LC_MONETARY": true, "LC_NAME": true, "LC_NUMERIC": true,
	"LC_PAPER": true, "LC_TELEPHONE": true, "LC_TIME": true,
	"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true,
	"ALL_PROXY": true, "all_proxy": true, "NO_PROXY": true, "no_proxy": true,
}

// curlTransportExits are curl's exit statuses for a read that got no answer (the EXIT CODES of
// curl's manual): 5 and 6, a proxy or a host whose name did not resolve; 7, no connection; 28, the
// --max-time bound ran out; 35, a TLS handshake that failed; 52, a server that sent nothing back;
// and 55 and 56, a connection that failed while curl sent or received.
var curlTransportExits = []int{5, 6, 7, 28, 35, 52, 55, 56}

// curlUnreached is whether a curl that exited exit, after it printed the HTTP status httpStatus,
// 0 for no status line, got no answer from GitHub: it exited with one of curlTransportExits and
// printed no status line. A curl that printed one heard GitHub, whatever its exit.
func curlUnreached(exit, httpStatus int) bool {
	return httpStatus == 0 && slices.Contains(curlTransportExits, exit)
}

// curlAnswer is what the curl read of slug answered. status is curl's exit status, stdout what it
// printed on standard output (the status line, the headers and the body, as --include prints
// them), and stderr its standard error, which only a failure quotes. An exit status of
// curlTransportExits with no status line printed is a *transportError; a curl that printed one
// heard GitHub, so its failure is refused, not read with ls-remote. A rate limit, read as
// rateLimitFrom reads gh's answer, is a *rateLimitError. GitHub's 404 is a *notFoundError. A 200
// whose body's visibility is public, private or internal, read as gh's is, is the answer. Anything
// else is an error.
func curlAnswer(status int, stdout, stderr, slug string) (Visibility, error) {
	masked := maskPath(slug)
	body := visibilityBody(stdout)
	httpStatus, _ := responseHeaders(stdout)
	if status != 0 {
		if curlUnreached(status, httpStatus) {
			return "", &transportError{program: "curl", said: fmt.Sprintf("exit %d: %s", status, said(body, stderr, slug))}
		}
		return "", fmt.Errorf("curl's read of repos/%s exited %d: %s", masked, status, said(body, stderr, slug))
	}
	if limited := rateLimitFrom(stdout); limited != nil {
		return "", limited
	}
	if httpStatus == http.StatusNotFound {
		return "", &notFoundError{slug: slug}
	}
	if httpStatus != http.StatusOK {
		return "", fmt.Errorf("curl's read of repos/%s answered the status %d: %s", masked, httpStatus, said(body, stderr, slug))
	}
	var answer struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		return "", fmt.Errorf("curl's read of repos/%s answered a body that is not JSON: %v", masked, err)
	}
	switch visibility := Visibility(strings.ToLower(strings.TrimSpace(answer.Visibility))); visibility {
	case Public, Private, Internal:
		return visibility, nil
	default:
		return "", fmt.Errorf("curl's read of repos/%s answered no visibility GitHub reports", masked)
	}
}

// ghToken is the token that gh sends with its reads of slug, which the curl read sends in gh's
// place. It is the caller's GH_TOKEN, else its GITHUB_TOKEN: gh sends either one first, in that
// order, and `legion gh` runs the guard's reads under it (cmd/legion's ghVisibilityRead). Else it
// is the password that `gh auth git-credential get` answers for the repository's URL.
//
// In a pod, that gh is `legion gh`. It answers this request before anything else (cmd/legion's
// runGh) with runGitCredential: it reads the hosts.yml of the bundle's directory for the repository
// (ghConfigDirFor), which is the directory it runs the guard's REST read with. For a repository of
// an owner the bundle holds a directory for, it runs no gh and makes no request, so it answers
// while gh's TLS cannot. The guard's REST read of any other owner's repository is one `legion gh`
// refuses before gh runs, so that repository never reaches this read (Guard.readUnservedOwner).
// A real gh answers from its own
// configuration, which is what it authenticates the REST read with. A request through `legion gh`
// writes an `auth git-credential` line to its call log, as every credential request through it
// does.
//
// The token comes back on gh's standard output. That output is read apart from gh's standard
// error, and no error quotes it. A token that holds a line break, a carriage return included, is
// refused wherever it comes from: curl reads the header on its standard input a line at a time, so
// the token would put a header of its own on the request.
func (g *Guard) ghToken(ctx context.Context, slug string) (string, error) {
	base := g.base()
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		token := value(base, name)
		switch {
		case token == "":
			continue
		case strings.ContainsAny(token, "\r\n"):
			return "", fmt.Errorf("%s holds a line break", name)
		}
		return token, nil
	}
	var stderr bytes.Buffer
	status, printed, err := g.run(ctx, child{
		argv:   []string{"gh", "auth", "git-credential", "get"},
		stdin:  strings.NewReader("protocol=https\nhost=github.com\npath=" + slug + ".git\n\n"),
		stderr: &stderr,
	})
	if err != nil {
		return "", procgroup.Said(err, stderr.String())
	}
	if status != 0 {
		return "", fmt.Errorf("gh auth git-credential get exited %d: %s", status, said("", stderr.String(), slug))
	}
	// git's credential protocol takes the last value of a key, as git itself reads gh's answer.
	token := ""
	for line := range strings.Lines(printed) {
		if password, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "password="); ok {
			token = password
		}
	}
	switch {
	case token == "":
		return "", errors.New("gh auth git-credential get answered no password")
	case strings.ContainsAny(token, "\r\n"):
		return "", errors.New("gh auth git-credential get answered a password that holds a line break")
	}
	return token, nil
}
