package sessionproxy

import (
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
	"fmt"
	"io"
	"math/big"
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
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/sjawhar/legion/daemon/internal/testwait"
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
)

// The scopes each source asks IAM Credentials for, as cloudsqlconn.WithIAMAuthNTokenSources names
// them. The test writes them out and does not use the package's constants, so a wrong constant
// fails it.
var (
	apiScopes   = []string{"https://www.googleapis.com/auth/sqlservice.admin", "https://www.googleapis.com/auth/cloud-platform"}
	loginScopes = []string{"https://www.googleapis.com/auth/sqlservice.login"}
)

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
// API call. The impersonation library takes the test's own IAM Credentials client
// (option.WithHTTPClient) and uses it as it is, so the credential that the generateAccessToken call
// itself carries is not under test: the fake pod credentials sign only the calls of the row with no
// account. The instance has no private address, so each dial ends with the connector's refusal,
// which shows that the private-IP option still applies. Every request stays on loopback or in an
// in-process transport. The test sets an environment variable, so it is not parallel.
func TestTheConnectorSignsInAsTheServiceAccountItImpersonates(t *testing.T) {
	usePodCredentials(t)
	const noPrivateIP = `instance does not have IP of type "PRIVATE"`
	sources := []struct {
		name   string
		scopes []string
	}{{"the API source", apiScopes}, {"the login source", loginScopes}}

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
			err := dialThroughConnector(t, tc.account, iam, admin.url)

			asked := iam.requests()
			if tc.account == "" {
				if len(asked) != 0 {
					t.Errorf("the IAM Credentials fake saw %+v, want no request: with no account, no source impersonates", asked)
				}
			} else {
				for _, s := range sources {
					n := 0
					for _, r := range asked {
						if r.account == tc.account && slices.Equal(r.scopes, s.scopes) {
							n++
						}
					}
					if n != 1 {
						t.Errorf("%s asked IAM Credentials for a token as %s with the scopes %v %d times, want once; the fake saw %+v", s.name, tc.account, s.scopes, n, asked)
					}
				}
				if len(asked) != len(sources) {
					t.Errorf("the IAM Credentials fake saw %d requests, want %d, one for each source: %+v", len(asked), len(sources), asked)
				}
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
// after it is refused. The fake's tokens expire within seconds, so the test dials until a dial
// finds both tokens expired and its refresh mints them again; a source that keeps its first token
// never does, and the shared wait gives up naming what the fake saw. Every Admin API call must
// still carry the impersonated tokens, never the pod's own.
func TestTheImpersonatedTokensAreMintedAgainOnceTheyExpire(t *testing.T) {
	usePodCredentials(t)
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	// The library takes a token as expired ten seconds before its expiry (oauth2's expiryDelta),
	// and IAM's expireTime has whole seconds, so each token lasts between one and two seconds.
	iam := &iamCredentialsFake{t: t, lifetime: 12 * time.Second}
	admin := newAdminAPIFake(t)
	opts, err := connectorOptions(context.Background(), account, option.WithHTTPClient(&http.Client{Transport: handlerTransport{iam}}))
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
	// Each dial is refused (the instance has only a public address), and the refused dial drops
	// the instance's cached connection info, so the next dial refreshes it and asks each source
	// for its token.
	ctx := testwait.Context(t)
	testwait.Until(t, "the API source and the login source each to mint a second token once the first expired", func() (string, bool) {
		if conn, err := dialer.Dial(ctx, testInstance); conn != nil {
			_ = conn.Close()
		} else if err == nil {
			t.Fatal("Dial returned neither a connection nor an error")
		}
		api, login := mints(apiScopes), mints(loginScopes)
		return fmt.Sprintf("the API source minted %d tokens and the login source %d, want two each: a source that keeps its first token signs in with it after it expires; the fake saw %+v",
			api, login, iam.requests()), api >= 2 && login >= 2
	})

	checkAdminCalls(t, admin, impersonatedAPIToken, impersonatedLoginToken, " after its token expired")
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
// (GOOGLE_APPLICATION_CREDENTIALS). The file names a service account with a key the test generates
// and a token_uri at a server of the test's own, which answers every token request with podToken.
// Any use of the pod's own credentials then shows as podToken, and no read reaches a metadata
// server.
func usePodCredentials(t *testing.T) {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"access_token": podToken, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)

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
		"token_uri":      tokens.URL + "/token",
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

// tokenRequest is one generateAccessToken request: the account it impersonates and the scopes it
// asks for.
type tokenRequest struct {
	account string
	scopes  []string
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
	f.asked = append(f.asked, tokenRequest{account: account, scopes: body.Scope})
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
	t     testing.TB
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
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

// ServeHTTP answers the connector's calls. A request that carries none of the tokens this test's
// credential fakes mint (podToken, impersonatedAPIToken, impersonatedLoginToken) as its bearer is
// another process's: it is answered 404 and logged before it is recorded, and fails nothing. Each
// of the three is admitted, so checkAdminCalls still tells a call signed with the wrong one.
func (f *adminAPIFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Header.Get("Authorization") {
	case "Bearer " + podToken, "Bearer " + impersonatedAPIToken, "Bearer " + impersonatedLoginToken:
	default:
		f.t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the connector's", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
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

// dialThroughConnector builds a real connector with connectorOptions for account. Its IAM
// Credentials client is served by iam, and its Admin API is at admin. It dials testInstance once,
// under the test's context (testwait.Context), closes the connector and returns the dial's error.
func dialThroughConnector(t *testing.T, account string, iam http.Handler, admin string) error {
	t.Helper()
	opts, err := connectorOptions(context.Background(), account, option.WithHTTPClient(&http.Client{Transport: handlerTransport{iam}}))
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	dialer, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	conn, dialErr := dialer.Dial(testwait.Context(t), testInstance)
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

func writeJSON(t testing.TB, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write the answer: %v", err)
	}
}
