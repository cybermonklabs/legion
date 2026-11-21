package api

// going_agent_reads.go is the agent's two reads outside Dispatch, which the server makes for it,
// since the agent holds no secret and reads nothing but its socket: a GitHub read, of a
// repository Dispatch maps to a project the run's person can read, through the server's GitHub
// App; and a web read, of an address the person's words or a tool result of the run names, which
// must resolve to a public address, checked on every connection the read makes - after the DNS
// answer and on each redirect - through its dialer's Control hook, so a name that resolves to a
// public address once and a private one the next time cannot pass. A read stops at
// agentwire.CallBodyLimit or goingWebReadTime and says it was cut.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const (
	// goingWebReadTime bounds a web read, its connection, its redirects and its body together.
	goingWebReadTime = 10 * time.Second
	// goingWebRedirects is how many redirects a web read follows.
	goingWebRedirects = 5
)

// goingRepositoryPart is an owner or a repository name as GitHub spells one.
var goingRepositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// githubRead reads a repository through the server's GitHub App: the repository must be one
// Dispatch maps to a project (repo_projects), and that project one the run's person can read, as
// the route table answers the person's own read of the project's architecture sources, a small
// read any credential of the project may make, which the project scope check judges; the read is
// GET of the path under the repository's API path, with an installation token of the App, its
// body cut at agentwire.CallBodyLimit.
func (a *AgentListener) githubRead(ctx context.Context, request agentwire.CallRequest, doer goingInProcess) (agentwire.CallAnswer, *agentwire.Error) {
	owner, name, split := strings.Cut(request.Repository, "/")
	if !split || !goingRepositoryPart.MatchString(owner) || !goingRepositoryPart.MatchString(name) || owner == ".." || name == ".." {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "%q is no repository as owner/name", request.Repository)
	}
	path, refusal := goingRepositoryPath(request.Path)
	if refusal != nil {
		return agentwire.CallAnswer{}, refusal
	}
	repo := refs.ExternalRepo(owner, name)
	var project string
	err := a.s.deps.Store.Pool.QueryRow(ctx, `select project from repo_projects where repo = $1`, repo).Scan(&project)
	unmapped := goingAgentRefusal(agentwire.CodeRepositoryUnmapped,
		"Dispatch maps %s to no project you can read, so the server does not read it", repo)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return agentwire.CallAnswer{}, unmapped
	case err != nil:
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/architecture-sources", nil)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if answer := doer.serve(read); answer.status != 0 && answer.status != http.StatusOK {
		return agentwire.CallAnswer{}, unmapped
	}
	if a.s.deps.GitHub == nil {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeGitHubUnavailable, "this server holds no GitHub App")
	}
	token, err := a.s.deps.GitHub.RepositoryToken(ctx, owner, name)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeGitHubUnavailable, "an installation token for %s: %v", repo, err)
	}
	body, status, header, err := a.s.deps.GitHub.ReadWithin(ctx, token, "/repos/"+owner+"/"+name+path, agentwire.CallBodyLimit)
	var tooLarge *githubapp.ResponseTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return agentwire.CallAnswer{Status: http.StatusOK, Cut: true}, nil
	case err != nil:
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeGitHubUnavailable, "read %s: %v", repo, err)
	}
	return agentwire.CallAnswer{Status: status, Body: body, ContentType: header.Get("Content-Type")}, nil
}

// goingRepositoryPath is a GitHub read's path under its repository, with its query, as the API
// path it appends ("/pulls/12"): relative, with no segment that leaves the repository.
func goingRepositoryPath(path string) (string, *agentwire.Error) {
	parsed, err := url.Parse(path)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || strings.HasPrefix(path, "/") {
		return "", goingAgentRefusal(agentwire.CodeBadRequest, "%q is no path under a repository", path)
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == ".." || segment == "." {
			return "", goingAgentRefusal(agentwire.CodeBadRequest, "%q leaves its repository", path)
		}
	}
	if parsed.Path == "" {
		return parsed.String(), nil
	}
	return "/" + parsed.String(), nil
}

// webRead reads a web address for a run: the address must be in the person's words or in a tool
// result of the run (goingAddressSeen), and the read itself is the listener's goingWebReader.
func (a *AgentListener) webRead(ctx context.Context, request agentwire.CallRequest) (agentwire.CallAnswer, *agentwire.Error) {
	address := strings.TrimSpace(request.URL)
	if address == "" {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a web read names its address")
	}
	seen, err := a.s.goingAddressSeen(ctx, request.Run, address)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if !seen {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeAddressUnseen,
			"%s is in none of your words and no tool result of this run, so the server does not read it", address)
	}
	return a.web.read(ctx, address)
}

// goingAddressSeen reports whether address is in the person's words - those of run and of the
// runs of its conversation before it - or in a tool result of run: a saved tool step's body, or
// the answer a request of the run's calls landed with.
func (s *server) goingAddressSeen(ctx context.Context, run, address string) (bool, error) {
	var seen bool
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select exists (
			select 1 from going_runs r
			where r.id = $1::uuid and (
				strpos(r.words, $2) > 0
				or exists (select 1 from going_runs c
				           where c.conversation_id = coalesce(r.conversation_id, r.id) and c.created_at <= r.created_at
				             and strpos(c.words, $2) > 0)
				or exists (select 1 from going_run_steps st where st.run_id = r.id and st.kind = $3 and strpos(st.body::text, $2) > 0)
				or exists (select 1 from going_landings l where l.run_id = r.id and strpos(l.answer::text, $2) > 0)
			)
		)
	`, run, address, agentwire.StepTool).Scan(&seen)
	return seen, err
}

// goingWebAdmit says whether a web read may connect to an address: goingPublicAddress, which a
// test widens to admit its own server.
type goingWebAdmit func(netip.Addr) bool

// goingWebReader reads the web for a run's call: each connection it makes is admitted by admit,
// and the whole read is bounded by limit.
type goingWebReader struct {
	admit goingWebAdmit
	limit time.Duration
}

// goingAddressRefused is a connection a web read's dialer refused: its address is not admitted.
type goingAddressRefused struct {
	address string
}

func (e goingAddressRefused) Error() string {
	return e.address + " is no public address"
}

// read is GET of address, on a client of its own that uses no proxy, follows at most
// goingWebRedirects redirects, and connects only where admit admits the address the dialer is
// about to connect to, which it checks after the name's DNS answer and on every redirect's
// connection. Its body is kept up to agentwire.CallBodyLimit, and a body that goes on past that,
// or past the read's limit, is answered as far as it was read and marked cut.
func (w goingWebReader) read(ctx context.Context, address string) (agentwire.CallAnswer, *agentwire.Error) {
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "%q is no http or https address", address)
	}
	ctx, cancel := context.WithTimeout(ctx, w.limit)
	defer cancel()
	dialer := &net.Dialer{Control: func(_, hostPort string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(hostPort)
		if err != nil {
			return goingAddressRefused{address: hostPort}
		}
		resolved, err := netip.ParseAddr(host)
		if err != nil || !w.admit(resolved.Unmap()) {
			return goingAddressRefused{address: host}
		}
		return nil
	}}
	transport := &http.Transport{Proxy: nil, DialContext: dialer.DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > goingWebRedirects {
			return fmt.Errorf("the read stopped after %d redirects", goingWebRedirects)
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "%q is no address the server can read: %v", address, err)
	}
	response, err := client.Do(request)
	var refused goingAddressRefused
	switch {
	case errors.As(err, &refused):
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeAddressRefused,
			"the read of %s reached %s, which is a loopback, private, link-local or metadata address, so it was not read", address, refused.address)
	case err != nil && ctx.Err() != nil:
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeWebFailed, "%s did not answer within %s", address, w.limit)
	case err != nil:
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeWebFailed, "read %s: %v", address, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, agentwire.CallBodyLimit+1))
	answer := agentwire.CallAnswer{Status: response.StatusCode, Body: body, ContentType: response.Header.Get("Content-Type")}
	switch {
	case len(body) > agentwire.CallBodyLimit:
		answer.Body, answer.Cut = body[:agentwire.CallBodyLimit], true
	case err != nil && ctx.Err() != nil:
		answer.Cut = true
	case err != nil:
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeWebFailed, "read the body of %s: %v", address, err)
	}
	return answer, nil
}

// goingNonPublicPrefixes are the address blocks no web read reaches beside those netip names
// (loopback, private, link-local, multicast, unspecified): this network, the shared address space
// of carrier-grade NAT, the IETF's protocol assignments, the documentation blocks, benchmarking,
// the reserved block with the broadcast address, the discard-only block, and the IPv6 blocks that
// carry an IPv4 address inside them (NAT64, 6to4, Teredo), which could carry a private one.
var goingNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// goingPublicAddress reports whether a web read may connect to address: a global unicast
// address that is none of loopback, private (the cloud's IPv6 metadata address among them),
// link-local (the cloud's metadata address 169.254.169.254 among them), multicast or unspecified,
// and in no block of goingNonPublicPrefixes. An IPv4 address written as IPv6 is judged as IPv4.
func goingPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range goingNonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
