package leakguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
)

// The reads the guard makes when gh's REST read of a visibility fails on its connection to GitHub
// and GitHub answered nothing (Guard.readWithoutGh). In a busy one-CPU pod, every gh call failed
// with `net/http: TLS handshake timeout` for over two hours, while curl and git, whose TLS is
// OpenSSL's through libcurl, finished their handshakes with the same hosts in under a second. The
// guard refused every push as a setup error for that whole time.

// transportError is a visibility read that failed on its connection to GitHub before GitHub
// answered anything: program is the program that made the read (gh or curl), and said is what it
// printed about the failure, with the repository's name masked.
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
//   - gh printed no status line, so GitHub sent no response: with --include, gh prints the status
//     line of every response, an error response included;
//   - gh printed exactly one line, `Get "https://api.github.com/repos/<slug>": <cause>`, the form
//     of Go's error for the request itself (net/url's Error), naming this read's own URL;
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
	if httpStatus, _ := responseHeaders(printed); httpStatus != 0 {
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

// errNoCurlRead is a curl read that the guard could not make: no curl on PATH, or no token for it
// (Guard.curlVisibility).
var errNoCurlRead = errors.New("the read through curl could not be made")

// readWithoutGh reads slug's visibility after gh's REST read failed on its connection to GitHub
// with no answer from GitHub (unreached, ghTransportFailure). It does not make the GraphQL read,
// which goes through gh too.
//
// First it makes the same REST read through curl (Guard.curlVisibility). curl's public, private or
// internal is the answer, as gh's is, and it is kept as gh's is. A rate limit that curl's read meets
// goes on to the reads a rate-limited REST read goes on to (readWithoutREST). Where the curl read
// cannot be made, or where curl fails on its connection too, the visibility is read with git's
// ls-remote and no credential (Guard.lsRemote), which answers public and nothing else. Every other
// outcome refuses the call, naming gh's failure and what each read said. A read that the caller's
// bound stops refuses the call too, because no bound is left for another read.
func (g *Guard) readWithoutGh(ctx context.Context, slug string, unreached *transportError) (Visibility, VisibilitySource, error) {
	visibility, err := g.curlVisibility(ctx, slug)
	var limited *rateLimitError
	var curlUnreached *transportError
	switch {
	case err == nil:
		return visibility, VisibilityFromCurl, nil
	case errors.As(err, &limited):
		visibility, source, err := g.readWithoutREST(ctx, slug, limited)
		if err != nil {
			return "", "", fmt.Errorf("%w; the read through curl: %w", unreached, err)
		}
		return visibility, source, nil
	case !errors.As(err, &curlUnreached) && !errors.Is(err, errNoCurlRead):
		return "", "", fmt.Errorf("%w, and curl could not answer: %w", unreached, err)
	}
	visibility, lsErr := g.lsRemote(ctx, githubOrigin, slug)
	if lsErr != nil {
		return "", "", fmt.Errorf("%w, %w, and git ls-remote could not answer: %w", unreached, err, lsErr)
	}
	return visibility, VisibilityFromLsRemote, nil
}

// curlBound is the longest the curl read runs, its token read included (Guard.curlVisibility). In
// the pod where every gh call failed its TLS handshake, curl answered GitHub's REST API in under a
// second. A curl read that takes longer than this leaves the rest of the caller's bound to
// ls-remote.
const curlBound = 20 * time.Second

// curlVisibility is gh's REST read of slug made through curl, whose TLS is OpenSSL's: the same
// request (GET /repos/<slug> at gitHubAPI) with the same token (Guard.ghToken), read by
// curlAnswer. The token goes to curl on its standard input, as the `Authorization` header that
// `--header @-` reads, and never in its argv, which any process of the machine can read. curl
// keeps the caller's environment but GH_TOKEN and GITHUB_TOKEN (curlInherits), so its proxy and
// its trust store are the caller's. Its options are these:
//
//   - `-q`, first as curl requires, so curl reads no .curlrc, and `--no-netrc`, so it reads no
//     netrc: the token is the one credential the request carries;
//   - `--include` prints the status line and the headers, which rateLimitFrom reads as it reads
//     gh's, and `--suppress-connect-headers` leaves out a proxy's answer to CONNECT, which would
//     otherwise come first;
//   - `--proto =https` allows no other protocol, and no redirect is followed;
//   - `--max-time` is the time left before the read's bound: curlBound, or the caller's own
//     deadline where that is sooner.
//
// No curl on PATH, and no token, are errNoCurlRead. A curl that its own bound stops is a
// *transportError. A curl or a token read that the caller's bound stops is the *stoppedError run
// answers.
func (g *Guard) curlVisibility(ctx context.Context, slug string) (Visibility, error) {
	curl, ok := lookpath.On(value(g.base(), "PATH"), "curl")
	if !ok {
		return "", fmt.Errorf("%w: curl is not on PATH", errNoCurlRead)
	}
	bounded, cancel := context.WithTimeout(ctx, curlBound)
	defer cancel()
	token, err := g.ghToken(bounded, slug)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", err
	case err != nil:
		return "", fmt.Errorf("%w: %w", errNoCurlRead, err)
	}
	deadline, _ := bounded.Deadline()
	maxTime := max(1, int(time.Until(deadline)/time.Second))
	var stderr bytes.Buffer
	status, printed, err := g.run(bounded, child{
		argv: []string{curl, "-q", "--no-netrc", "--silent", "--show-error", "--include", "--suppress-connect-headers",
			"--proto", "=https", "--max-time", strconv.Itoa(maxTime),
			"--header", "@-", "--header", "Accept: application/vnd.github+json", "--header", "X-GitHub-Api-Version: 2022-11-28",
			gitHubAPI + "/repos/" + slug},
		inherit: curlInherits,
		stdin:   strings.NewReader("Authorization: Bearer " + token + "\n"),
		stderr:  &stderr,
	})
	if err != nil {
		var stopped *stoppedError
		if errors.As(err, &stopped) && ctx.Err() == nil {
			return "", &transportError{program: "curl", said: err.Error()}
		}
		return "", err
	}
	return curlAnswer(status, printed, stderr.String(), slug)
}

// curlInherits is which names of the caller's environment the curl read keeps: every name but
// GH_TOKEN and GITHUB_TOKEN. The token goes to curl on its standard input alone.
func curlInherits(name string) bool {
	return name != "GH_TOKEN" && name != "GITHUB_TOKEN"
}

// curlTransportExits are curl's exit statuses for a read that got no answer (the EXIT CODES of
// curl's manual): 5 and 6, a proxy or a host whose name did not resolve; 7, no connection; 28, the
// --max-time bound ran out; 35, a TLS handshake that failed; 52, a server that sent nothing back;
// and 55 and 56, a connection that failed while curl sent or received.
var curlTransportExits = []int{5, 6, 7, 28, 35, 52, 55, 56}

// curlAnswer is what the curl read of slug answered. status is curl's exit status, stdout what it
// printed on standard output (the status line, the headers and the body, as --include prints
// them), and stderr its standard error, which only a failure quotes. An exit status of
// curlTransportExits with no status line printed is a *transportError. A rate limit, read as
// rateLimitFrom reads gh's answer, is a *rateLimitError. A 200 whose body's visibility is public,
// private or internal, read as gh's is, is the answer. Anything else is an error.
func curlAnswer(status int, stdout, stderr, slug string) (Visibility, error) {
	masked := maskPath(slug)
	said := func() string {
		return strings.ReplaceAll(strings.TrimSpace(strings.TrimSpace(visibilityBody(stdout))+" "+strings.TrimSpace(stderr)), slug, masked)
	}
	httpStatus, _ := responseHeaders(stdout)
	if status != 0 {
		if httpStatus == 0 && slices.Contains(curlTransportExits, status) {
			return "", &transportError{program: "curl", said: fmt.Sprintf("exit %d: %s", status, said())}
		}
		return "", fmt.Errorf("curl's read of repos/%s exited %d: %s", masked, status, said())
	}
	if limited := rateLimitFrom(stdout); limited != nil {
		return "", limited
	}
	if httpStatus != http.StatusOK {
		return "", fmt.Errorf("curl's read of repos/%s answered the status %d: %s", masked, httpStatus, said())
	}
	var answer struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(visibilityBody(stdout)), &answer); err != nil {
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
// (ghConfigDirFor), which is the directory it runs the guard's REST read with. It runs no gh and
// makes no request, so it answers while gh's TLS cannot. A real gh answers from its own
// configuration, which is what it authenticates the REST read with.
//
// The token comes back on gh's standard output. That output is read apart from gh's standard
// error, and no error quotes it.
func (g *Guard) ghToken(ctx context.Context, slug string) (string, error) {
	base := g.base()
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		token := value(base, name)
		switch {
		case token == "":
			continue
		case strings.ContainsAny(token, "\r\n"):
			// curl reads one header a line, so a line break would put a header of the
			// token's own on the request.
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
		return "", err
	}
	if status != 0 {
		return "", fmt.Errorf("gh auth git-credential get exited %d: %s", status,
			strings.ReplaceAll(strings.TrimSpace(stderr.String()), slug, maskPath(slug)))
	}
	// git's credential protocol takes the last value of a key, as git itself reads gh's answer.
	token := ""
	for line := range strings.Lines(printed) {
		if password, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "password="); ok {
			token = password
		}
	}
	if token == "" {
		return "", errors.New("gh auth git-credential get answered no password")
	}
	return token, nil
}
