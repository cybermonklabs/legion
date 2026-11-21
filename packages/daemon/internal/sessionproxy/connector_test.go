package sessionproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

// The tokens the rig's fakes mint. Each names the credential that minted it, so the token a
// request carries shows which source signed it.
const (
	// podToken is the token of the pod's own credentials: the token endpoint of the test's
	// credential file answers every request with it.
	podToken = "pod-token"
	// impersonatedAPIToken and impersonatedLoginToken are the IAM Credentials fake's tokens for
	// the account: one for the Admin API's scopes and one for the login scope.
	impersonatedAPIToken   = "impersonated-api-token"
	impersonatedLoginToken = "impersonated-login-token"
	// federatedToken is the security token service fake's token: the federated token it gives
	// for the writer token.
	federatedToken = "federated-token"
)

// The scopes each source asks IAM Credentials for, as cloudsqlconn.WithIAMAuthNTokenSources names
// them. The test writes them out and does not use the package's constants, so a wrong constant
// fails it.
var (
	apiScopes   = []string{"https://www.googleapis.com/auth/sqlservice.admin", "https://www.googleapis.com/auth/cloud-platform"}
	loginScopes = []string{"https://www.googleapis.com/auth/sqlservice.login"}
)

// impersonatedSources are the two impersonated sources, each with the scopes it asks for.
var impersonatedSources = []struct {
	name   string
	scopes []string
}{{"the API source", apiScopes}, {"the login source", loginScopes}}

// The paths of the two Admin API calls the connector makes for testInstance.
const (
	connectSettingsPath       = "/sql/v1beta4/projects/example-project/instances/example-instance/connectSettings"
	generateEphemeralCertPath = "/sql/v1beta4/projects/example-project/instances/example-instance:generateEphemeralCert"
)

// The connector signs in as the account the proxy impersonates. One impersonated source signs the
// Admin API calls, and the other gives the login token in the ephemeral-certificate exchange. With
// no account, both are the pod's own credentials. The real connector and the real
// impersonation library run against fakes of the pod's credentials, of the IAM Service Account
// Credentials API and of the Cloud SQL Admin API, so the test sees which token reaches which Admin
// API call. connectorOptions gives the impersonation library a client over the test's own IAM
// Credentials transport, which the library uses as it is, so the credential that the
// generateAccessToken call itself carries is not under test: the fake pod credentials sign only
// the calls of the row with no account. The instance has no private address, so each dial ends
// with the connector's refusal, which shows that the private-IP option still applies. Every
// request stays on loopback or in an in-process transport. The test sets an environment variable,
// so it is not parallel.
func TestTheConnectorSignsInAsTheServiceAccountItImpersonates(t *testing.T) {
	usePodCredentials(t)
	const noPrivateIP = `instance does not have IP of type "PRIVATE"`

	for _, tc := range []struct {
		name    string
		account string
		// apiToken is the bearer token every Admin API call carries, and loginToken is the
		// access_token of generateEphemeralCert.
		apiToken, loginToken string
	}{
		{
			name:       "impersonating legion-sessions@example-project.iam.gserviceaccount.com",
			account:    "legion-sessions@example-project.iam.gserviceaccount.com",
			apiToken:   impersonatedAPIToken,
			loginToken: impersonatedLoginToken,
		},
		{name: "as the pod itself", apiToken: podToken, loginToken: podToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iam := &iamCredentialsFake{t: t}
			admin := newAdminAPIFake(t)
			err := dialThroughConnector(t, Signer{Account: tc.account}, googleEndpoints{iam: handlerTransport{iam}}, admin.url)

			asked := iam.requests()
			if tc.account == "" {
				if len(asked) != 0 {
					t.Errorf("the IAM Credentials fake saw %+v, want no request: with no account, no source impersonates", asked)
				}
			} else {
				checkEachSourceMintedOnce(t, asked, tc.account)
			}

			// Every Admin API call must carry the expected tokens, so when the proxy impersonates,
			// no call carries the pod's own.
			checkAdminCalls(t, admin, tc.apiToken, tc.loginToken, "")

			if err == nil || !strings.Contains(err.Error(), noPrivateIP) {
				t.Errorf("Dial = %v, want the connector's refusal %q: the instance has only a public address, and the connector dials the private one", err, noPrivateIP)
			}
		})
	}
}

// A proxy serves its generation for as long as the generation runs, which is days, and each
// impersonated token lasts an hour. So each source must mint its token again through IAM
// Credentials once the one it holds has expired: a source that keeps its first token (a static
// one, or one given a fixed Lifetime, which the library never refreshes) hands the connector an
// expired token at its first refresh after that hour, and every connection the generation opens
// after it is refused. The fake's tokens expire within seconds, so each dial after the first one
// finds both tokens expired; every Admin API call must still carry the impersonated tokens, never
// the pod's own.
func TestTheImpersonatedTokensAreMintedAgainOnceTheyExpire(t *testing.T) {
	usePodCredentials(t)
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	// The library takes a token as expired ten seconds before its expiry (oauth2's expiryDelta),
	// and IAM's expireTime has whole seconds, so each token lasts between one and two seconds.
	iam := &iamCredentialsFake{t: t, lifetime: 12 * time.Second}
	admin := newAdminAPIFake(t)
	opts, err := connectorOptions(context.Background(), Signer{Account: account}, googleEndpoints{iam: handlerTransport{iam}})
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	dialer, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin.url+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	defer func() { _ = dialer.Close() }()

	mints := func(scopes []string) int {
		n := 0
		for _, r := range iam.requests() {
			if r.account == account && slices.Equal(r.scopes, scopes) {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(20 * time.Second)
	for mints(apiScopes) < 2 || mints(loginScopes) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("after 20 s of dials, the API source minted %d tokens and the login source %d, want two each: a source that keeps its first token signs in with it after it expires; the fake saw %+v",
				mints(apiScopes), mints(loginScopes), iam.requests())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if conn, err := dialer.Dial(ctx, testInstance); conn != nil {
			_ = conn.Close()
		} else if err == nil {
			t.Fatal("Dial returned neither a connection nor an error")
		}
		cancel()
		time.Sleep(time.Second)
	}

	checkAdminCalls(t, admin, impersonatedAPIToken, impersonatedLoginToken, " after its token expired")
}

// With a writer token, the connector impersonates the account with the federated token that the
// security token service gives for the token file's contents, and never with the pod's own
// credentials: nothing asks the pod's credential file or the metadata server. The real connector,
// the real impersonation library and the real exchange run against fakes of the security token
// service, of IAM Credentials and of the Cloud SQL Admin API. The IAM Credentials fake is the base
// transport under the proxy's own client, so it sees the Authorization that the client puts on
// each generateAccessToken call. One exchange serves both impersonated sources. The test sets
// environment variables, so it is not parallel.
func TestTheConnectorImpersonatesWithTheWriterTokenAndNeverThePodsCredentials(t *testing.T) {
	neverAskThePodsCredentials(t)
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	const noPrivateIP = `instance does not have IP of type "PRIVATE"`
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeWriterToken(t, tokenFile, "writer-token-1")
	sts := newSTSFake(t, 3600)
	iam := &iamCredentialsFake{t: t}
	admin := newAdminAPIFake(t)
	err := dialThroughConnector(t, Signer{Account: account, WriterAudience: testWriterAudience, WriterTokenFile: tokenFile},
		googleEndpoints{iam: handlerTransport{iam}, stsURL: sts.url}, admin.url)

	if got := sts.exchanged(); !slices.Equal(got, []string{"writer-token-1"}) {
		t.Errorf("the security token service was sent the subject tokens %q, want the token file's contents once: one federated source serves both impersonated sources", got)
	}
	asked := iam.requests()
	checkEachSourceMintedOnce(t, asked, account)
	checkSignedWithTheFederatedToken(t, asked)
	checkAdminCalls(t, admin, impersonatedAPIToken, impersonatedLoginToken, "")
	if err == nil || !strings.Contains(err.Error(), noPrivateIP) {
		t.Errorf("Dial = %v, want the connector's refusal %q: the instance has only a public address, and the connector dials the private one", err, noPrivateIP)
	}
}

// The kubelet rewrites the writer token before it expires, and a proxy serves its generation for
// days, so each exchange reads the token file again: a source that kept what it read first would
// send an expired token once that one ran out, and every connection after it would be refused.
// Here the federated token and the impersonated ones each last two seconds at most, so dials a
// second apart exchange again. Once the file is rewritten, the exchanges send its new contents,
// and none sends the old contents again.
func TestTheWriterTokenFileIsReadAgainAtEachExchange(t *testing.T) {
	neverAskThePodsCredentials(t)
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeWriterToken(t, tokenFile, "writer-token-1")
	// The proxy exchanges again writerTokenEarlyRefresh before the federated token's expiry, so a
	// token with two seconds more than that lasts two seconds.
	sts := newSTSFake(t, int((writerTokenEarlyRefresh+2*time.Second)/time.Second))
	// As in TestTheImpersonatedTokensAreMintedAgainOnceTheyExpire, each impersonated token lasts
	// between one and two seconds.
	iam := &iamCredentialsFake{t: t, lifetime: 12 * time.Second}
	admin := newAdminAPIFake(t)
	opts, err := connectorOptions(context.Background(), Signer{Account: account, WriterAudience: testWriterAudience, WriterTokenFile: tokenFile},
		googleEndpoints{iam: handlerTransport{iam}, stsURL: sts.url})
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	dialer, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin.url+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	defer func() { _ = dialer.Close() }()
	dial := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if conn, err := dialer.Dial(ctx, testInstance); conn != nil {
			_ = conn.Close()
		} else if err == nil {
			t.Fatal("Dial returned neither a connection nor an error")
		}
	}

	dial()
	if len(sts.exchanged()) == 0 {
		t.Fatal("the first dial made no exchange of the writer token")
	}
	writeWriterToken(t, tokenFile, "writer-token-2")
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Contains(sts.exchanged(), "writer-token-2") {
		if time.Now().After(deadline) {
			t.Fatalf("after 20 s of dials, the exchanges sent the subject tokens %q, want the token file's new contents: a source that keeps what it read first sends an expired token", sts.exchanged())
		}
		time.Sleep(time.Second)
		dial()
	}

	// The exchanges are serialized and the file is replaced whole, so every exchange before the
	// first one with the new contents sent the old ones, and every exchange after it the new ones.
	subjects := sts.exchanged()
	rewritten := slices.Index(subjects, "writer-token-2")
	for i, subject := range subjects {
		want := "writer-token-1"
		if i >= rewritten {
			want = "writer-token-2"
		}
		if subject != want {
			t.Errorf("exchange %d of %q sent %q, want %q", i+1, subjects, subject, want)
		}
	}
	checkSignedWithTheFederatedToken(t, iam.requests())
	checkAdminCalls(t, admin, impersonatedAPIToken, impersonatedLoginToken, " after the writer token was rewritten")
}

// A fake's handler reads each request it is sent and fails the test on a body it cannot read,
// so stopping a fake at the end of its test must never run a handler on a connection the stop
// closed (stopServing says how it does not). The connector does leave such a request: when a
// login token expires within four minutes, as the tokens of the rows above do, it starts a
// refresh as soon as one completes, and a dial that then fails drops the instance from its cache
// and abandons that refresh's requests without waiting for the fake. The row holds a request whose
// headers the server has read and whose handler has not started (headersHeldListener), on a new
// connection and on an idle one that has served a request before: a forced reproduction of the
// connector rows' failure holds it on both kinds, idle ones in most runs. stopServing must close
// the connection and run no handler on it.
func TestStopServingRunsNoHandlerOnAConnectionItCloses(t *testing.T) {
	const served = "GET /served HTTP/1.1\r\nHost: fake.example\r\n\r\n"
	const held = "POST /held HTTP/1.1\r\nHost: fake.example\r\nContent-Length: 5\r\n\r\n"
	for _, tc := range []struct {
		name string
		// before is what the connection serves before the held request.
		before string
	}{
		{name: "on a new connection"},
		{name: "on an idle connection", before: served},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := make(chan error, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/held" {
					return
				}
				_, err := io.ReadAll(r.Body)
				ran <- err
			}))
			listener := &headersHeldListener{Listener: server.Listener, conns: make(chan *headersHeldConn, 1), before: len(tc.before), headers: len(held)}
			server.Listener = listener
			server.Start()
			t.Cleanup(func() { stopServing(server) })
			client, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatalf("dial the server: %v", err)
			}
			defer func() { _ = client.Close() }()
			if tc.before != "" {
				if _, err := io.WriteString(client, tc.before); err != nil {
					t.Fatalf("send the request served first: %v", err)
				}
				answer, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatalf("read the answer to the request served first: %v", err)
				}
				_ = answer.Body.Close()
			}
			if _, err := io.WriteString(client, held); err != nil {
				t.Fatalf("send the held request's headers: %v", err)
			}
			conn := <-listener.conns
			if err := <-conn.arrived; err != nil {
				t.Fatalf("read the held request's complete headers: %v", err)
			}

			stopServing(server)
			select {
			case <-conn.closed:
			default:
				t.Errorf("stopServing left the held request's connection open")
			}
			select {
			case err := <-ran:
				t.Errorf("the handler ran on the connection stopServing closed, and read the body as %v", err)
			default:
			}
		})
	}
}

// checkEachSourceMintedOnce fails the test unless asked, the generateAccessToken requests that
// the IAM Credentials fake saw, are one for each impersonated source: as account, with the
// source's scopes.
func checkEachSourceMintedOnce(t *testing.T, asked []tokenRequest, account string) {
	t.Helper()
	for _, s := range impersonatedSources {
		n := 0
		for _, r := range asked {
			if r.account == account && slices.Equal(r.scopes, s.scopes) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s asked IAM Credentials for a token as %s with the scopes %v %d times, want once; the fake saw %+v", s.name, account, s.scopes, n, asked)
		}
	}
	if len(asked) != len(impersonatedSources) {
		t.Errorf("the IAM Credentials fake saw %d requests, want %d, one for each source: %+v", len(asked), len(impersonatedSources), asked)
	}
}

// checkSignedWithTheFederatedToken fails the test unless every generateAccessToken request in
// asked carries the federated token, which the proxy's IAM Credentials client puts on each call.
func checkSignedWithTheFederatedToken(t *testing.T, asked []tokenRequest) {
	t.Helper()
	if len(asked) == 0 {
		t.Error("the IAM Credentials fake saw no generateAccessToken request")
	}
	for _, r := range asked {
		if want := "Bearer " + federatedToken; r.authorization != want {
			t.Errorf("generateAccessToken as %s with the scopes %v carried the Authorization %q, want the federated token's %q", r.account, r.scopes, r.authorization, want)
		}
	}
}

// checkAdminCalls fails the test unless admin saw at least one complete connectSettings call and
// one complete generateEphemeralCert call, every call carries the bearer token apiToken, and every
// complete generateEphemeralCert call has the access_token loginToken. It checks the Authorization
// of an incomplete call too, but not its access_token: the connector abandoned that call before
// its body ended. when goes after the token in each token message, for example " after its token
// expired".
func checkAdminCalls(t *testing.T, admin *adminAPIFake, apiToken, loginToken, when string) {
	t.Helper()
	calls := admin.requests()
	for _, path := range []string{connectSettingsPath, generateEphemeralCertPath} {
		if !slices.ContainsFunc(calls, func(c adminCall) bool { return c.path == path && c.complete }) {
			t.Errorf("the connector never completed a call to %s; the Admin API fake saw %+v", path, calls)
		}
	}
	for _, c := range calls {
		if want := "Bearer " + apiToken; c.authorization != want {
			t.Errorf("the API source signed %s with %q%s, want %q", c.path, c.authorization, when, want)
		}
		if c.path == generateEphemeralCertPath && c.complete && c.accessToken != loginToken {
			t.Errorf("the login source gave generateEphemeralCert the access_token %q%s, want %q", c.accessToken, when, loginToken)
		}
	}
}

// usePodCredentials makes a credential file of the test's own the pod's credentials
// (podCredentialsServedBy), whose token server answers every token request with podToken. Any
// use of the pod's own credentials then shows as podToken, and no read reaches a metadata server.
func usePodCredentials(t *testing.T) {
	t.Helper()
	podCredentialsServedBy(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"access_token": podToken, "token_type": "Bearer", "expires_in": 3600})
	})
}

// neverAskThePodsCredentials makes the pod's credentials fail the test on any use: the credential
// file (GOOGLE_APPLICATION_CREDENTIALS) names a token server that fails the test on any request,
// and the metadata server (GCE_METADATA_HOST) is a server that does the same. A test of the
// writer token then shows that nothing asks either of them.
func neverAskThePodsCredentials(t *testing.T) {
	t.Helper()
	podCredentialsServedBy(t, refuseEveryRequest(t, "the token server of the pod's credential file"))
	metadata := httptest.NewServer(refuseEveryRequest(t, "the metadata server"))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
}

// refuseEveryRequest is a handler that fails the test on every request it gets. who names the
// server it stands for in the failure.
func refuseEveryRequest(t *testing.T, who string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s was asked %s %s, and nothing may ask it", who, r.Method, r.URL)
		http.Error(w, "nothing may ask this server", http.StatusForbidden)
	}
}

// podCredentialsServedBy makes a credential file of the test's own the pod's credentials
// (GOOGLE_APPLICATION_CREDENTIALS). The file names a service account with a key the test generates
// and a token_uri at a server of the test's own, which tokens answers.
func podCredentialsServedBy(t *testing.T, tokens http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(tokens)
	t.Cleanup(server.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the pod's key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode the pod's key: %v", err)
	}
	credentials, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "example-project",
		"private_key_id": "pod-key",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "legion-pod@example-project.iam.gserviceaccount.com",
		"token_uri":      server.URL + "/token",
	})
	if err != nil {
		t.Fatalf("encode the pod's credentials: %v", err)
	}
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(file, credentials, 0o600); err != nil {
		t.Fatalf("write the pod's credentials: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
}

// tokenRequest is one generateAccessToken request: the account it impersonates, the scopes it
// asks for and the Authorization header it carries.
type tokenRequest struct {
	account       string
	scopes        []string
	authorization string
}

// iamCredentialsFake is the IAM Service Account Credentials API, served in process through
// handlerTransport. It answers generateAccessToken for any account with the token for the scopes
// the request asks for, records each request, and fails the test on any other request. Each token
// expires lifetime after it is minted, or one hour after when lifetime is zero, which is what IAM
// Credentials gives a request that names no lifetime.
type iamCredentialsFake struct {
	t        *testing.T
	lifetime time.Duration

	mu    sync.Mutex
	asked []tokenRequest
}

func (f *iamCredentialsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	account, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/")
	if ok {
		account, ok = strings.CutSuffix(account, ":generateAccessToken")
	}
	if !ok || r.Method != http.MethodPost {
		f.t.Errorf("the IAM Credentials fake was asked %s %s, which is no generateAccessToken request", r.Method, r.URL)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body struct {
		Scope []string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("read the body of generateAccessToken: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.asked = append(f.asked, tokenRequest{account: account, scopes: body.Scope, authorization: r.Header.Get("Authorization")})
	f.mu.Unlock()

	var token string
	switch {
	case slices.Equal(body.Scope, apiScopes):
		token = impersonatedAPIToken
	case slices.Equal(body.Scope, loginScopes):
		token = impersonatedLoginToken
	default:
		f.t.Errorf("the IAM Credentials fake was asked for a token as %s with the scopes %v, which no source has", account, body.Scope)
		http.Error(w, "unknown scopes", http.StatusBadRequest)
		return
	}
	lifetime := f.lifetime
	if lifetime == 0 {
		lifetime = time.Hour
	}
	writeJSON(f.t, w, map[string]string{"accessToken": token, "expireTime": time.Now().Add(lifetime).UTC().Format(time.RFC3339)})
}

func (f *iamCredentialsFake) requests() []tokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// stsFake is Google's security token service, on loopback. It answers the writer token's exchange
// and fails the test on any other request. Each exchange must use the token-exchange grant and ask
// for an access token, for testWriterAudience and the cloud-platform scope, with a JWT subject
// token. The fake records the subject token of each exchange and answers federatedToken, which
// expires expiresIn seconds later.
type stsFake struct {
	t         *testing.T
	url       string
	expiresIn int

	mu       sync.Mutex
	subjects []string
}

func newSTSFake(t *testing.T, expiresIn int) *stsFake {
	t.Helper()
	f := &stsFake{t: t, expiresIn: expiresIn}
	server := httptest.NewServer(f)
	t.Cleanup(func() { stopServing(server) })
	f.url = server.URL + "/v1/token"
	return f
}

func (f *stsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/token" {
		f.t.Errorf("the security token service fake was asked %s %s, which is no token exchange", r.Method, r.URL)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("read the exchange's form: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for name, want := range map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"audience":             testWriterAudience,
		"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
		"scope":                "https://www.googleapis.com/auth/cloud-platform",
	} {
		if got := r.PostForm.Get(name); got != want {
			f.t.Errorf("the exchange sent %s=%q, want %q", name, got, want)
		}
	}
	f.mu.Lock()
	f.subjects = append(f.subjects, r.PostForm.Get("subject_token"))
	f.mu.Unlock()
	writeJSON(f.t, w, map[string]any{
		"access_token": federatedToken, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type": "Bearer", "expires_in": f.expiresIn,
	})
}

// exchanged is the subject token of every exchange the fake answered, in order.
func (f *stsFake) exchanged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subjects)
}

// writeWriterToken makes token the contents of the writer token file at path. It replaces the
// file whole, by a rename, as the kubelet replaces a projected token, so no exchange reads it half
// written.
func writeWriterToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path+".new", []byte(token), 0o600); err != nil {
		t.Fatalf("write the writer token: %v", err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatalf("replace the writer token: %v", err)
	}
}

// handlerTransport serves every request with its handler in process, so a client that uses it
// sends nothing out of the test.
type handlerTransport struct {
	handler http.Handler
}

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return w.Result(), nil
}

// stopServing stops the loopback server of a fake whose handler reads each request and fails the
// test on what it cannot read, as a cleanup at the end of its test (docs/testing.md, "A fake HTTP
// server whose client gives up on requests").
//
// httptest.Server.Close alone closes each connection that is new or idle: the idle ones through
// net/http's closeIdleConns, the new ones in a loop of its own. net/http marks a connection active
// only after it has read a request's headers, so a connection on which it has read them but not
// yet started the request's handler can still be new or idle. The handler then runs on the
// closed connection and reads its body as "use of closed network connection", which fails the
// test for no defect of the code under test (TestStopServingRunsNoHandlerOnAConnectionItCloses).
// Shutdown first marks the server as shutting down, and net/http checks that mark after it marks
// a connection active and before it starts the handler, so no handler starts on a connection the
// stop closes. Its context has already ended, so Shutdown closes the idle connections and
// returns. Close then closes the new and idle connections left and waits for the active handlers,
// whose connections it leaves open: their clients must finish or cancel those requests for the
// cleanup to return. A server whose handler fails the test on every request (refuseEveryRequest)
// keeps Close alone, so a request whose headers the server has read by then still fails the test.
func stopServing(server *httptest.Server) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	_ = server.Config.Shutdown(ended)
	server.Close()
}

// headersHeldListener serves TestStopServingRunsNoHandlerOnAConnectionItCloses's connection and
// sends it on conns. The connection passes its first before bytes through: the requests it serves
// first, whose answers the client reads before it sends more. Then it returns the held request's
// headers bytes one at a time, and holds the last one until the connection is closed, so the
// server has the whole headers only once the stop has closed the connection. A hold of a whole
// socket read instead could hold part of the headers alone, and Close alone would then end the
// connection on the unparsed request and pass the row.
type headersHeldListener struct {
	net.Listener
	conns           chan *headersHeldConn
	before, headers int
}

func (l *headersHeldListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	conn := &headersHeldConn{Conn: c, arrived: make(chan error, 1), closed: make(chan struct{}), beforeLeft: l.before, headersLeft: l.headers}
	select {
	case l.conns <- conn:
	default:
	}
	return conn, nil
}

// headersHeldConn is one connection headersHeldListener accepted. arrived receives nil once the
// held request's headers have all arrived, or the error that ended them first. closed is closed
// with the connection.
type headersHeldConn struct {
	net.Conn
	arrived                 chan error
	closed                  chan struct{}
	beforeLeft, headersLeft int
	closeOnce               sync.Once
}

// Read holds only the read that takes the held request's last header byte. A read that times
// out passes through: while a handler runs, net/http reads one byte in the background and ends
// that read with a deadline once the handler has returned.
func (c *headersHeldConn) Read(p []byte) (int, error) {
	switch {
	case c.beforeLeft > 0:
		n, err := c.Conn.Read(p[:min(len(p), c.beforeLeft)])
		c.beforeLeft -= n
		return n, err
	case c.headersLeft > 0:
		n, err := c.Conn.Read(p[:1])
		c.headersLeft -= n
		var timeout net.Error
		if c.headersLeft == 0 || err != nil && !(errors.As(err, &timeout) && timeout.Timeout()) {
			select {
			case c.arrived <- err:
			default:
			}
		}
		if c.headersLeft == 0 {
			<-c.closed
		}
		return n, err
	}
	return c.Conn.Read(p)
}

func (c *headersHeldConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// adminCall is one Admin API request: its path, its Authorization header and, for
// generateEphemeralCert, the access_token of its body. complete says that the fake read the whole
// request. A generateEphemeralCert call is incomplete when its body ended early, because the
// connector abandoned it; it then has no access_token. A connectSettings call, which has no body,
// is complete. A call the fake does not answer is not.
type adminCall struct {
	path, authorization, accessToken string
	complete                         bool
}

// adminAPIFake is the Cloud SQL Admin API for testInstance, on loopback. The instance has a public
// address and no private one, and a server CA of the fake's own, which signs the ephemeral
// certificates. The fake records each request when its headers arrive, before it reads the body,
// so a request the connector abandons stays recorded. It fails the test on a call it does not
// answer.
type adminAPIFake struct {
	t     *testing.T
	url   string
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	caPEM string

	mu    sync.Mutex
	calls []adminCall
}

func newAdminAPIFake(t *testing.T) *adminAPIFake {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the server CA's key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "example-instance server CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("create the server CA: %v", err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the server CA: %v", err)
	}
	f := &adminAPIFake{t: t, ca: ca, caKey: key, caPEM: certificatePEM(der)}
	server := httptest.NewServer(f)
	t.Cleanup(func() { stopServing(server) })
	f.url = server.URL
	return f
}

func (f *adminAPIFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := adminCall{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == connectSettingsPath:
		call.complete = true
		f.record(call)
		writeJSON(f.t, w, &sqladmin.ConnectSettings{
			Region:          "us-central1",
			DatabaseVersion: "POSTGRES_16",
			BackendType:     "SECOND_GEN",
			// A public address only. It is loopback, so even a dial that took it would stay on
			// the test's machine.
			IpAddresses:  []*sqladmin.IpMapping{{Type: "PRIMARY", IpAddress: "127.0.0.1"}},
			ServerCaCert: &sqladmin.SslCert{Cert: f.caPEM},
		})
	case r.Method == http.MethodPost && r.URL.Path == generateEphemeralCertPath:
		// The fake records the request before it reads the body. So the test checks the
		// request's Authorization whether or not the connector abandons the request.
		i := f.record(call)
		var body sqladmin.GenerateEphemeralCertRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// A body that ends early is a request the connector abandoned: it cancels a refresh
			// it no longer needs, such as one in flight when a failed dial drops the instance
			// from its cache, and the cancel can land between the headers and the body. That is
			// no defect of the code under test. The recorded call stays incomplete, so the test
			// does not check its empty access_token, and the fake answers nothing.
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return
			}
			f.t.Errorf("read the body of generateEphemeralCert: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.completeCall(i, body.AccessToken)
		cert, err := f.issue(body.PublicKey)
		if err != nil {
			f.t.Errorf("issue the ephemeral certificate: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(f.t, w, &sqladmin.GenerateEphemeralCertResponse{EphemeralCert: &sqladmin.SslCert{Cert: cert}})
	default:
		f.record(call)
		f.t.Errorf("the connector called %s %s, which the Admin API fake does not answer", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// issue signs a client certificate for the connector's public key with the server CA, as the
// Admin API does, valid for the hour an ephemeral certificate lasts.
func (f *adminAPIFake) issue(publicKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return "", errors.New("the public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "legion-sessions"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, f.ca, key, f.caKey)
	if err != nil {
		return "", err
	}
	return certificatePEM(der), nil
}

// record adds call to the calls the fake saw and returns its index, with which completeCall
// finds it again.
func (f *adminAPIFake) record(call adminCall) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return len(f.calls) - 1
}

// completeCall gives the recorded call at index i the access_token of its body and marks the call
// complete: the fake read its whole body.
func (f *adminAPIFake) completeCall(i int, accessToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[i].accessToken = accessToken
	f.calls[i].complete = true
}

func (f *adminAPIFake) requests() []adminCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// dialThroughConnector builds a real connector with connectorOptions for signer, which reaches
// the token services at google, and its Admin API at admin. It dials testInstance once within
// 10 s, closes the connector and returns the dial's error.
func dialThroughConnector(t *testing.T, signer Signer, google googleEndpoints, admin string) error {
	t.Helper()
	opts, err := connectorOptions(context.Background(), signer, google)
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	dialer, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, dialErr := dialer.Dial(ctx, testInstance)
	if conn != nil {
		_ = conn.Close()
	}
	if err := dialer.Close(); err != nil {
		t.Errorf("close the connector: %v", err)
	}
	return dialErr
}

func certificatePEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write the answer: %v", err)
	}
}
