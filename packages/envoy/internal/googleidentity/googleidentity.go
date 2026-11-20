// packages/envoy/internal/googleidentity/googleidentity.go
//
// Package googleidentity reads a machine's own Google identity from the Compute Engine metadata
// server. Token reads the identity token a Google Cloud VM, and a pod on Google Kubernetes
// Engine, presents as the subject of the broker's RFC 8693 exchange. It asks for format=full, so
// a token minted on an instance carries the google.compute_engine claims an exchange entry may
// pin on, and it reads the token alone: nothing here verifies a signature, an issuer, an audience
// or an expiry, which is the broker's work, and nothing here parses a claim. Probe says, within
// ProbeTimeout, whether a Google metadata server answers and which service account it names, and
// AccessTokenSource reads that account's OAuth access token, for a store command's last
// credential source (cmd/agent-secrets, newGCPBackend). An answer that does not carry
// Metadata-Flavor: Google is no metadata server to either of them.
//
// The transport has no proxy at all, as cloud.google.com/go/compute/metadata's does.
// http.DefaultClient honours HTTP_PROXY, and the metadata server's address is link-local rather
// than loopback, so the default transport would hand whatever a proxy variable names the
// machine's identity token — a credential that acts as that machine until it expires, fetched
// again before every renewal, so one misread variable would hand over a fresh one each time.
package googleidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	// MetadataHostVar replaces the metadata server's host (and port), as Google's own client
	// libraries read it. Its value is a host, with no scheme.
	MetadataHostVar = "GCE_METADATA_HOST"
	// defaultMetadataHost is the metadata server every Google machine answers on.
	defaultMetadataHost = "metadata.google.internal"
	// identityPath mints an identity token for the instance's default service account.
	identityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	// timeout bounds one token fetch. The metadata server is on the host's own link, so a fetch
	// that takes longer than this is a fault, not a slow network.
	timeout = 10 * time.Second
)

// client is the proxy-free transport this package's reason for being rests on; the package
// comment says why it may never be http.DefaultClient. Its dial, name resolution included, is
// bounded by ProbeTimeout (dialer): a machine that has a metadata server reaches it on its own link
// at once, and a laptop whose resolver or network holds the metadata server's name learns within
// that bound that it has none, whichever read asked.
var client = &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}

// dialer is client's dialer. It is named so that this package's test can read the deadline each
// dial runs under, through its ControlContext, which nothing else here sets.
var dialer = &net.Dialer{Timeout: ProbeTimeout}

// tokenURL is the identity-token URL for audience, honouring MetadataHostVar.
func tokenURL(audience string) string {
	return metadataURL(identityPath) + "?audience=" + url.QueryEscape(audience) + "&format=full"
}

// Token asks the metadata server for this machine's identity token for audience. format=full is
// required: without it Google mints a token carrying no google.compute_engine claims, which names
// no project, zone or instance, so an exchange entry pinned to either refuses it. Every error
// names the URL it asked and never the token.
func Token(ctx context.Context, audience string) (string, error) {
	callURL := tokenURL(audience)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callURL, nil)
	if err != nil {
		return "", fmt.Errorf("ask the metadata server at %s: %w", callURL, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the metadata server at %s could not be reached (%s names its host): %w", callURL, MetadataHostVar, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read the metadata server's answer from %s: %w", callURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("the metadata server at %s answered HTTP %d", callURL, resp.StatusCode)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("the metadata server at %s answered an empty identity token", callURL)
	}
	return token, nil
}

// ErrNoMetadataServer is the answer of Probe and of AccessTokenSource's token where no Google
// listened at its address: its host name did not resolve, no connection came within
// ProbeTimeout, nothing answered within the read's own bound (ProbeTimeout for Probe, the 10 s
// token-fetch bound for a token), or what answered did not say it is Google's. A caller reads it
// as the absence of a machine identity, not as a fault.
var ErrNoMetadataServer = errors.New("no metadata server")

// ProbeTimeout bounds Probe, and the dial of every read here (client). A machine that has a
// metadata server answers on its own link in milliseconds; a laptop has none, and a dial to
// metadata.google.internal there can hang on a resolver or a captive network, so this is the whole
// wait such a machine pays to learn it has no metadata server.
const ProbeTimeout = 1500 * time.Millisecond

// emailPath names the instance's default service account, and tokenPath hands out its OAuth
// access token.
const (
	emailPath = "/computeMetadata/v1/instance/service-accounts/default/email"
	tokenPath = "/computeMetadata/v1/instance/service-accounts/default/token"
)

// metadataURL is path on the metadata server, honouring MetadataHostVar.
func metadataURL(path string) string {
	host := os.Getenv(MetadataHostVar)
	if host == "" {
		host = defaultMetadataHost
	}
	return "http://" + host + path
}

// Probe asks the metadata server which service account this machine runs as, within
// ProbeTimeout, through the package's proxy-free transport. It answers ErrNoMetadataServer
// (wrapped with what it saw) where no Google metadata server answered: a dial error, a host name
// that does not resolve, a timeout, or an answer without the Metadata-Flavor: Google header that
// every answer of Google's carries. A Google metadata server that answers anything but a success,
// or an empty email, is a fault, and its error is not ErrNoMetadataServer.
func Probe(ctx context.Context) (string, error) {
	callURL := metadataURL(emailPath)
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, callURL, nil)
	if err != nil {
		return "", fmt.Errorf("ask the metadata server at %s: %w", callURL, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("ask the metadata server at %s: %w", callURL, ctx.Err())
		}
		return "", fmt.Errorf("%w: nothing answered at %s within %s (%s names its host): %v", ErrNoMetadataServer, callURL, ProbeTimeout, MetadataHostVar, err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Metadata-Flavor") != "Google" {
		return "", fmt.Errorf("%w: what answered at %s is not a Google metadata server (its answer carries no Metadata-Flavor: Google)", ErrNoMetadataServer, callURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("read the metadata server's answer from %s: %w", callURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("the metadata server at %s answered HTTP %d", callURL, resp.StatusCode)
	}
	email := strings.TrimSpace(string(body))
	if email == "" {
		return "", fmt.Errorf("the metadata server at %s names no service account", callURL)
	}
	return email, nil
}

// AccessTokenSource is the OAuth access token of the instance's default service account, read
// from the metadata server through the package's proxy-free transport, each token expiring when
// the metadata server's expires_in says. It is not cloud.google.com/go/compute/metadata's
// ComputeTokenSource, whose OnGCE answer that library caches for the whole process. A caller
// that asks for several tokens wraps it in oauth2.ReuseTokenSource.
func AccessTokenSource() oauth2.TokenSource { return metadataTokenSource{} }

// metadataTokenSource is AccessTokenSource's oauth2.TokenSource. Its errors name the URL it
// asked and never a token. It answers ErrNoMetadataServer, wrapped with what it saw, where no
// Google metadata server answered (ErrNoMetadataServer says when), as Probe does; a Google server
// that answers anything but a success, or no token, is a fault, and its error is not
// ErrNoMetadataServer. The connection comes within ProbeTimeout or not at all (client); once it
// has, the server's own minting of the token has the whole 10 s token-fetch bound.
type metadataTokenSource struct{}

func (metadataTokenSource) Token() (*oauth2.Token, error) {
	callURL := metadataURL(tokenPath)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ask the metadata server at %s: %w", callURL, err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	asked := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: the metadata server at %s could not be reached (%s names its host): %v", ErrNoMetadataServer, callURL, MetadataHostVar, err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Metadata-Flavor") != "Google" {
		return nil, fmt.Errorf("%w: what answered at %s is not a Google metadata server (its answer carries no Metadata-Flavor: Google)", ErrNoMetadataServer, callURL)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the metadata server at %s answered HTTP %d", callURL, resp.StatusCode)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("the metadata server at %s answered something other than an access token", callURL)
	}
	if answer.AccessToken == "" || answer.ExpiresIn <= 0 {
		return nil, fmt.Errorf("the metadata server at %s answered no access token or no expiry", callURL)
	}
	return &oauth2.Token{AccessToken: answer.AccessToken, TokenType: answer.TokenType, Expiry: asked.Add(time.Duration(answer.ExpiresIn) * time.Second)}, nil
}
