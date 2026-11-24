package sessionproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// The credentials the redirect rows' stand-ins hand out or read; -RDX- is a marker the redaction
// check reads in captured output.
const (
	// redirectPodBearer is the token of the pod's own credentials in a row that signs with them.
	redirectPodBearer = "pod-bearer-not-a-secret-RDX-5d2b"
	// redirectWriterToken is the writer token file's contents, the exchange's subject token.
	redirectWriterToken = "writer-token-not-a-secret-RDX-a8f3"
	// redirectFederatedBearer is the federated token the exchange's stand-in answers.
	redirectFederatedBearer = "federated-bearer-not-a-secret-RDX-71c6"
)

// iamCredentialsHost is the host the impersonation library sends generateAccessToken to, a
// constant of the library's (impersonationClient says why).
const iamCredentialsHost = "iamcredentials.googleapis.com"

// TestAGoogleCallFollowsNoRedirect: whatever answers in one of Google's places answers the
// proxy's request with a redirect to a stand-in on the same host name at another port, whose
// requests the stand-in keeps (redirectstest.Sent), and the Location quotes the credential the
// request carried. oauth2.Transport sets a bearer on every request it sends, a redirect's among
// them, and a 307 sends a form again, so a client that followed the redirect would send the
// stand-in the credential whatever host the Location named. Each row's stand-in is sent nothing,
// and the call fails naming the redirect's status:
//
//   - admin-api: the Admin API answers a call signed with the pod's bearer 302 (connectorOptions'
//     Admin API client), and the dial fails on googleapi's 302;
//   - token-endpoint: the pod's credential file's token endpoint answers the token read, whose form
//     carries the signed assertion, 307 (podTokenSource), and the dial fails on x/oauth2's 307;
//   - iam-pod: IAM Credentials answers generateAccessToken signed with the pod's bearer 302
//     (impersonationClient without a writer token);
//   - sts-exchange: the security token service answers the exchange, whose form carries the writer
//     token, 307 (impersonationClient's exchange);
//   - iam-federated: IAM Credentials answers generateAccessToken signed with the federated token
//     302 (impersonationClient with a writer token).
//
// The first two dial the real connector through connectorDialer, as a pod's proxy does. The other
// three ask the impersonation library for the Admin API's token over the client
// impersonationClient makes, as connectorOptions' API source does, and read the library's error:
// a dial prints none of a failed mint's words, since net/http's error of the Admin API call that
// asked for the token names the call's URL, whose query the rule cannot read as clean. The twins
// show a client that follows each stand-in's redirect sends the stand-in the credential, so the
// rows can see one sent on. Captures sessionproxy.redirect-followed/<case>, what the stand-in was
// sent. The rows set environment variables, so they are not parallel.
func TestAGoogleCallFollowsNoRedirect(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	bearerEcho := redirectstest.EchoBearer(t, func(bearer string) string { return redirectstest.To(target, bearer) })
	toIAM := toIAMCredentials{echo: strings.TrimPrefix(bearerEcho, "http://")}
	const account = "legion-sessions@example-project.iam.gserviceaccount.com"
	writer := func(t *testing.T) Signer {
		tokenFile := filepath.Join(t.TempDir(), "token")
		writeWriterToken(t, tokenFile, redirectWriterToken)
		return Signer{Account: account, WriterAudience: testWriterAudience, WriterTokenFile: tokenFile}
	}
	podBearer := func(t *testing.T) {
		podCredentialsServedBy(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"access_token": redirectPodBearer, "token_type": "Bearer", "expires_in": 3600})
		})
	}

	t.Run("twin-bearer", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, bearerEcho+"/v1/projects/-/serviceAccounts/"+account+":generateAccessToken", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+redirectPodBearer)
		redirectstest.RequireSentOn(t, request, redirectPodBearer, sent)
	})
	t.Run("twin-form", func(t *testing.T) {
		echo, _ := formEcho(t, "subject_token", target)
		request, err := http.NewRequest(http.MethodPost, standIn(t, echo)+"/v1/token", strings.NewReader(url.Values{"subject_token": {redirectWriterToken}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		redirectstest.RequireSentOn(t, request, redirectWriterToken, sent)
	})

	for _, c := range []struct {
		// refusal is the words of the error that names the redirect's status.
		name, refusal string
		// call sets up the row's stand-ins and makes the call, and answers the credentials the
		// redirected request carried and the call's error.
		call func(t *testing.T) ([]string, error)
	}{
		{"admin-api", "got HTTP response code 302", func(t *testing.T) ([]string, error) {
			podBearer(t)
			return []string{redirectPodBearer}, dialAsAPod(t, Signer{}, googleEndpoints{}, bearerEcho)
		}},
		{"token-endpoint", "cannot fetch token: 307 Temporary Redirect", func(t *testing.T) ([]string, error) {
			echo, assertions := formEcho(t, "assertion", target)
			podCredentialsServedBy(t, echo)
			err := dialAsAPod(t, Signer{}, googleEndpoints{}, newAdminAPIFake(t).url)
			seen := assertions()
			if len(seen) == 0 || slices.Contains(seen, "") {
				t.Fatalf("the token endpoint's stand-in was sent %q, no token read with its assertion", seen)
			}
			return seen, err
		}},
		{"iam-pod", "impersonate: status code 302", func(t *testing.T) ([]string, error) {
			podBearer(t)
			return []string{redirectPodBearer}, mintAsTheAPISource(t, Signer{Account: account}, googleEndpoints{iam: toIAM})
		}},
		{"sts-exchange", "oauth2/google: status code 307", func(t *testing.T) ([]string, error) {
			neverAskThePodsCredentials(t)
			echo, _ := formEcho(t, "subject_token", target)
			google := googleEndpoints{stsURL: standIn(t, echo) + "/v1/token", iam: handlerTransport{refuseEveryRequest(t, "IAM Credentials")}}
			return []string{redirectWriterToken}, mintAsTheAPISource(t, writer(t), google)
		}},
		{"iam-federated", "impersonate: status code 302", func(t *testing.T) ([]string, error) {
			neverAskThePodsCredentials(t)
			sts := standIn(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"access_token": redirectFederatedBearer, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
					"token_type": "Bearer", "expires_in": 3600,
				})
			})
			return []string{redirectFederatedBearer}, mintAsTheAPISource(t, writer(t), googleEndpoints{stsURL: sts + "/v1/token", iam: toIAM})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			credentials, err := c.call(t)
			text := sent()
			oauth2calltest.RequireNoPiece(t, "sessionproxy.redirect-followed/"+c.name, text, credentials...)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), c.refusal) {
				t.Errorf("the call answered %v, want it refused with %q", err, c.refusal)
			}
		})
	}
}

// toIAMCredentials is an IAM transport (googleEndpoints.iam) that sends each request for the IAM
// Credentials API's host to the stand-in at echo (host:port), and every other request, a followed
// redirect's among them, out over http.DefaultTransport.
type toIAMCredentials struct {
	echo string
}

func (t toIAMCredentials) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == iamCredentialsHost {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = "http", t.echo, ""
	}
	return http.DefaultTransport.RoundTrip(r)
}

// formEcho is a handler in a token endpoint's place that answers every request 307 Temporary
// Redirect, which sends the request's form again, with a Location to target quoting the value of
// the form's field (redirectstest.To), and seen, the values of that field it was sent.
func formEcho(t *testing.T, field, target string) (echo http.HandlerFunc, seen func() []string) {
	var (
		mu     sync.Mutex
		values []string
	)
	echo = func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("read the form: %v", err)
		}
		value := r.PostForm.Get(field)
		mu.Lock()
		values = append(values, value)
		mu.Unlock()
		w.Header().Set("Location", redirectstest.To(target, value))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	return echo, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(values)
	}
}

// standIn serves handler until t ends and answers its base URL.
func standIn(t *testing.T, handler http.HandlerFunc) string {
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// dialAsAPod builds a real connector with connectorOptions for signer, which reaches Google at
// google and the Admin API at admin, dials testInstance once within 10 s through connectorDialer,
// whose scope reads the connector's error as a pod's proxy does, closes it and answers the dial's
// error.
func dialAsAPod(t *testing.T, signer Signer, google googleEndpoints, admin string) error {
	t.Helper()
	opts, err := connectorOptions(context.Background(), signer, google)
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	connector, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	dialer := connectorDialer{dialer: connector}
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

// mintAsTheAPISource asks the impersonation library for the signer's account's Admin API token
// over the IAM Credentials client impersonationClient makes for signer with google, as
// connectorOptions' API source does, and answers the library's error.
func mintAsTheAPISource(t *testing.T, signer Signer, google googleEndpoints) error {
	t.Helper()
	iam, err := impersonationClient(context.Background(), signer, google)
	if err != nil {
		t.Fatalf("impersonationClient: %v", err)
	}
	source, err := impersonate.CredentialsTokenSource(context.Background(), impersonate.CredentialsConfig{
		TargetPrincipal: signer.Account,
		Scopes:          []string{sqlAdminScope, cloudPlatformScope},
	}, option.WithHTTPClient(iam))
	if err != nil {
		t.Fatalf("CredentialsTokenSource: %v", err)
	}
	_, err = source.Token()
	return err
}
