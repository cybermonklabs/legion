// packages/envoy/cmd/cipher/secret_gcp_printed_test.go
//
// What the GCP store forms print about Google's servers and the credentials file
// GOOGLE_APPLICATION_CREDENTIALS names: the userinfo endpoint's and a token endpoint's status and
// registered code, never their body or a library's text; the variable, never the file's path or
// content, except in a command printed for running again, which carries the path of the file the
// sign-in read. Each test puts a made-up secret where a person or a server could put one, runs the
// form, and reads its standard error; a control row in each shows the clean case still works and
// still prints what it did.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
)

// roundTripFunc is an http.RoundTripper made of one function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// useGoogleAPIsStandIn makes answer the reply to every request this process sends to
// www.googleapis.com for the rest of t, through http.DefaultTransport, which the userinfo call's
// client reaches (oauth2.NewClient over http.DefaultClient); every other request goes on as it
// would. It answers how many requests it took.
func useGoogleAPIsStandIn(t *testing.T, answer func(*http.Request) (*http.Response, error)) *atomic.Int32 {
	t.Helper()
	base := http.DefaultTransport
	calls := &atomic.Int32{}
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			return base.RoundTrip(r)
		}
		calls.Add(1)
		return answer(r)
	})
	t.Cleanup(func() { http.DefaultTransport = base })
	return calls
}

// googleAnswer is an HTTP answer of status carrying body as JSON.
func googleAnswer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)),
		Request: r,
	}
}

// TestGCPUserinfoPrintsItsStatusAndCodeNeverItsBody: --owner me asks Google's userinfo endpoint
// whose account the sign-in is, through the real gcpUserinfo, and a failure of that call prints
// the endpoint, its status and registered code, or the failure's class, and nothing else: not a
// refusal's body that echoes the token, not the body of a token endpoint's refusal that x/oauth2
// keeps, not a transport error's own words. A userinfo answer that names a person still makes
// that person the owner (the control: the stand-in is the endpoint the form reached).
func TestGCPUserinfoPrintsItsStatusAndCodeNeverItsBody(t *testing.T) {
	const secret = "hunter2pw-userinfo-4b7e19"
	real := gcpUserinfo
	for _, row := range []struct {
		name   string
		signIn func(t *testing.T)
		answer func(*http.Request) (*http.Response, error)
		want   []string
	}{
		{"a refusal whose body echoes the token", func(t *testing.T) {
			t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-ada-token")
		}, func(r *http.Request) (*http.Response, error) {
			return googleAnswer(r, http.StatusUnauthorized, `{"error":{"code":401,"message":"Request had invalid authentication credentials: `+secret+`","status":"UNAUTHENTICATED"}}`), nil
		}, []string{"https://www.googleapis.com/oauth2/v2/userinfo", "the server answered HTTP 401 UNAUTHENTICATED"}},
		{"a token refresh the token endpoint refuses", func(t *testing.T) {
			tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"echo of ` + secret + `"}`))
			}))
			t.Cleanup(tokens.Close)
			gcpMetadataToken = func(ctx context.Context) (*google.Credentials, error) {
				config := &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: tokens.URL}}
				refresh := "refresh-" + secret
				return &google.Credentials{TokenSource: config.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh})}, nil
			}
		}, func(r *http.Request) (*http.Response, error) {
			return googleAnswer(r, http.StatusOK, `{"email":"ada@example.com"}`), nil
		}, []string{"https://www.googleapis.com/oauth2/v2/userinfo", "the server answered HTTP 400 invalid_grant"}},
		{"a transport failure whose words name a secret", func(t *testing.T) {
			t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-ada-token")
		}, func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("proxyconnect tcp: dial proxy-%s.example:3128: %w", secret, syscall.ECONNREFUSED)
		}, []string{"https://www.googleapis.com/oauth2/v2/userinfo", "connection refused"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			useNoGcloud(t)
			gcpUserinfo = real
			row.signIn(t)
			useGoogleAPIsStandIn(t, row.answer)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			refuseValueReads(t)

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != exitUsageError {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, exitUsageError)
			}
			wantNamedNotPrinted(t, "stderr", stderr, append(row.want, "--owner <email>"), secret)
			requireNothingWritten(t, fake, broker)
		})
	}
	t.Run("control: a userinfo answer that names a person", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		useNoGcloud(t)
		gcpUserinfo = real
		t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-ada-token")
		calls := useGoogleAPIsStandIn(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/oauth2/v2/userinfo" || r.Header.Get("Authorization") != "Bearer lent-ada-token" {
				return googleAnswer(r, http.StatusNotFound, `{}`), nil
			}
			return googleAnswer(r, http.StatusOK, `{"email":"Ada@Example.com"}`), nil
		})
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
		if owner := fake.ownerOf("new-test"); owner != "ada@example.com" {
			t.Fatalf("NEW_TEST's owner = %q, want ada@example.com", owner)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("the userinfo stand-in took %d requests, want 1", n)
		}
	})
}

// TestGCPCredentialsFilePrintsItsVariableNeverItsPathOrContent: the file
// GOOGLE_APPLICATION_CREDENTIALS names holds a credential, and the variable is where a person who
// pasted the credential itself puts it, so no line the command prints spells the variable's value
// or quotes the file: a file that is not there prints the system's reason, one that does not load
// fixed text, never x/oauth2's words, which quote the file's type. The well-known credentials file
// is refused the same way, by its own name. A file that holds a key still signs every call in,
// and the line naming the sign-in names the variable, not the path.
func TestGCPCredentialsFilePrintsItsVariableNeverItsPathOrContent(t *testing.T) {
	const secret = "hunter2pw-credfile-91c3d7"
	writeIn := func(t *testing.T, data string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), secret, "credentials.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, row := range []struct {
		name string
		use  func(t *testing.T, dir string)
		want []string
	}{
		{"the variable holds the credential itself", func(t *testing.T, _ string) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", secret)
		}, []string{"GOOGLE_APPLICATION_CREDENTIALS", "open: no such file or directory"}},
		{"the file is a credential of a type of its own", func(t *testing.T, _ string) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeIn(t, `{"type":"`+secret+`"}`))
		}, []string{"GOOGLE_APPLICATION_CREDENTIALS", "holds no Google Cloud credential this command can load"}},
		{"the file is not JSON", func(t *testing.T, _ string) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeIn(t, secret))
		}, []string{"GOOGLE_APPLICATION_CREDENTIALS", "holds no Google Cloud credential this command can load"}},
		{"the well-known file is a credential of a type of its own", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "application_default_credentials.json"), []byte(`{"type":"`+secret+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"the well-known credentials file", "holds no Google Cloud credential this command can load"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			dir := useGcloudConfigurations(t, "box", orderConfigurations)
			row.use(t, dir)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

			_, stderr, code := runSecret("show", "SCRATCH_TEST")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, row.want, secret, "unknown credential type", "invalid character")
			if _, calls := fake.calledUnder(); calls != 0 {
				t.Errorf("Secret Manager saw %d calls, want none", calls)
			}
		})
	}
	t.Run("control: a file that holds a key", func(t *testing.T) {
		const gac = "gac@example.iam.gserviceaccount.com"
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		holdScratchTest(fake)
		startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
		useGcloudConfigurations(t, "box", orderConfigurations)
		path := filepath.Join(t.TempDir(), secret, "credentials.json")
		writeServiceAccountFile(t, path, gac, startGoogleTokenServer(t))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

		_, stderr, code := runSecret("show", "SCRATCH_TEST")
		if code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
		if got, calls := fake.calledUnder(); got != "Bearer not-a-secret-for-"+gac || calls == 0 {
			t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the key's", got, calls)
		}
		wantNamedNotPrinted(t, "stderr", stderr, []string{"using the credentials file GOOGLE_APPLICATION_CREDENTIALS names: " + gac + "\n"}, secret)
	})
}

// TestGCPPrintedCommandsCarryTheCredentialsFileItSignedInWith: a create signed in with the file
// GOOGLE_APPLICATION_CREDENTIALS names prints the commands that settle what it left with that
// file's absolute path, shell-quoted, before agent-secrets (GOOGLE_APPLICATION_CREDENTIALS=) and
// before gcloud (CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=), so a pasted command signs in with the
// same file in any shell and from any directory, though the variable named it relative to this
// one. The file was read and signed in with, so its path is no credential a person pasted. No
// command carries the access token the file's key was given. Where no variable chose the source
// (the metadata server) the commands carry no prefix, and under a gcloud login they carry the
// login (the controls).
func TestGCPPrintedCommandsCarryTheCredentialsFileItSignedInWith(t *testing.T) {
	const secret = "hunter2pw-cmdtoken-27d0ae"
	for _, mode := range []struct {
		name string
		// signIn makes the mode's source present and answers the prefix of the agent-secrets
		// lines and of the gcloud line, built by hand.
		signIn func(t *testing.T) (agent, gcloud string)
		// absent are what no line of the mode carries.
		absent []string
	}{
		{"a key file named by a relative path", func(t *testing.T) (string, string) {
			useGcloudConfigurations(t, "box", orderConfigurations)
			tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"` + secret + `","expires_in":3600,"token_type":"Bearer"}`))
			}))
			t.Cleanup(tokens.Close)
			dir := filepath.Join(t.TempDir(), "keys dir")
			writeServiceAccountFile(t, filepath.Join(dir, "credentials.json"), "gac@example.iam.gserviceaccount.com", tokens.URL)
			t.Chdir(dir)
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "credentials.json")
			quoted := "'" + filepath.Join(dir, "credentials.json") + "'"
			return "GOOGLE_APPLICATION_CREDENTIALS=" + quoted + " ", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + quoted + " "
		}, []string{secret, "$GOOGLE_APPLICATION_CREDENTIALS"}},
		{"control: the metadata server, no variable set", func(t *testing.T) (string, string) {
			useGCPIdentity(t, "", "robot@example.iam.gserviceaccount.com")
			useNoGcloud(t)
			return "", ""
		}, []string{"GOOGLE_APPLICATION_CREDENTIALS=", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="}},
		{"control: a named gcloud login", func(t *testing.T) (string, string) {
			useNoADC(t)
			useGcloud(t, "ada@example.com", "grace@example.com")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
			return "CLOUDSDK_CORE_ACCOUNT=grace@example.com ", "CLOUDSDK_CORE_ACCOUNT=grace@example.com "
		}, []string{"GOOGLE_APPLICATION_CREDENTIALS=", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="}},
	} {
		for _, scenario := range []struct {
			name  string
			setUp func(fake *fakeSecretManager)
			want  func(agent, gcloud string) []string
		}{
			{"another writer's version kept", func(fake *fakeSecretManager) {
				fake.beforeCall = func(method string) {
					if method == "ListSecretVersions" {
						if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: newTestID, Payload: &secretmanagerpb.SecretPayload{Data: []byte("theirs")}}); err != nil {
							panic(err)
						}
					}
				}
			}, func(agent, _ string) []string {
				return []string{"`" + agent + "agent-secrets secret show NEW_TEST`", "`" + agent + "agent-secrets secret set NEW_TEST`"}
			}},
			{"the empty secret's delete refused", func(fake *fakeSecretManager) {
				fake.refuseMethods = map[string]codes.Code{"DeleteSecret": codes.PermissionDenied}
			}, func(_, gcloud string) []string { return []string{"`" + gcloud + clearEmptySecret + "`"} }},
		} {
			t.Run(mode.name+", "+scenario.name, func(t *testing.T) {
				fake := newFakeSecretManager()
				scenario.setUp(fake)
				startFakeSecretManager(t, fake)
				agent, gcloud := mode.signIn(t)
				startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				useStdin(t, "")

				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
				}
				wantNamedNotPrinted(t, "stderr", stderr, scenario.want(agent, gcloud), mode.absent...)
			})
		}
	}
}

// useProductionShapedDial serves fake over TLS on loopback (HTTP/2, as gRPC needs) for the rest of
// t, and points gcpClientDial at it the way the real gcpClientDial dials Secret Manager: through
// option.WithCredentials, so the credential's token source is spent by Google's own transport
// (cloud.google.com/go/auth and its oauth2adapt), not by the test's bearerOf.
func useProductionShapedDial(t *testing.T, fake *fakeSecretManager) {
	t.Helper()
	server := grpc.NewServer(grpc.UnaryInterceptor(fake.recordAuthorization))
	secretmanagerpb.RegisterSecretManagerServiceServer(server, fake)
	listener := httptest.NewUnstartedServer(server)
	listener.EnableHTTP2 = true
	listener.StartTLS()
	t.Cleanup(listener.Close)
	roots := x509.NewCertPool()
	roots.AddCert(listener.Certificate())
	replaceGCPDial(t, func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error) {
		return secretmanager.NewClient(ctx, option.WithCredentials(creds), option.WithLogger(slog.New(slog.DiscardHandler)),
			option.WithEndpoint(listener.Listener.Addr().String()),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots}))),
			option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(interceptor)))
	})
}

// TestGCPStoreCallUnderARefusedTokenRefreshPrintsItsClass: a store call whose credential's token
// the token endpoint refuses fails with the gRPC status the transport gives it, Unauthenticated,
// and the refusal's class - its status and registered code - never the body of the token
// endpoint's answer, which x/oauth2 and cloud.google.com/go/auth keep in their error and gRPC
// prints into the status message. It holds for a key file GOOGLE_APPLICATION_CREDENTIALS names and
// for the well-known credentials file, through the test's dial and through one shaped as the real
// one. A token endpoint that answers still signs the call in (the control).
func TestGCPStoreCallUnderARefusedTokenRefreshPrintsItsClass(t *testing.T) {
	const secret = "hunter2pw-refresh-5d82c1"
	refusing := func(t *testing.T) string {
		t.Helper()
		tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"echo of ` + secret + `"}`))
		}))
		t.Cleanup(tokens.Close)
		return tokens.URL
	}
	for _, row := range []struct {
		name       string
		wellKnown  bool
		production bool
	}{
		{"a key file GOOGLE_APPLICATION_CREDENTIALS names", false, false},
		{"a key file GOOGLE_APPLICATION_CREDENTIALS names, the real dial", false, true},
		{"the well-known credentials file", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			if row.production {
				useProductionShapedDial(t, fake)
			}
			holdScratchTest(fake)
			dir := useGcloudConfigurations(t, "box", orderConfigurations)
			path := filepath.Join(t.TempDir(), "credentials.json")
			if row.wellKnown {
				path = filepath.Join(dir, "application_default_credentials.json")
			}
			writeServiceAccountFile(t, path, "gac@example.iam.gserviceaccount.com", refusing(t))
			if !row.wellKnown {
				t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
			}
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

			_, stderr, code := runSecret("show", "SCRATCH_TEST")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{"Secret Manager GetSecret answered Unauthenticated: fetch an access token with the credentials file: the server answered HTTP 400"}, []string{secret, "Response:", "error_description"}...)
		})
	}
	t.Run("control: a token endpoint that answers, the real dial", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useProductionShapedDial(t, fake)
		holdScratchTest(fake)
		useGcloudConfigurations(t, "box", orderConfigurations)
		path := filepath.Join(t.TempDir(), "credentials.json")
		writeServiceAccountFile(t, path, "gac@example.iam.gserviceaccount.com", startGoogleTokenServer(t))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

		if _, stderr, code := runSecret("show", "SCRATCH_TEST"); code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
		if got, calls := fake.calledUnder(); got != "Bearer not-a-secret-for-gac@example.iam.gserviceaccount.com" || calls == 0 {
			t.Fatalf("Secret Manager calls ran under %q (%d calls), want the key's token", got, calls)
		}
	})
}

// TestARefusedGcloudSelectorIsNotPrinted: CLOUDSDK_CORE_ACCOUNT naming a login gcloud does not hold,
// and CLOUDSDK_ACTIVE_CONFIG_NAME naming a configuration this machine does not hold, are refused
// naming the variable and what gcloud does hold, never the value: the variable is where a person
// who pasted a credential in the wrong place puts it. A login and a configuration gcloud holds
// still sign in (the controls).
func TestARefusedGcloudSelectorIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-selector-5f21a8"
	t.Run("CLOUDSDK_CORE_ACCOUNT", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		useGcloud(t, "ada@example.com", "grace@example.com")
		t.Setenv("CLOUDSDK_CORE_ACCOUNT", secret+"@example.com")
		startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
		}
		wantNamedNotPrinted(t, "stderr", stderr, []string{"CLOUDSDK_CORE_ACCOUNT names an account gcloud holds no login for (not printed; it holds ada@example.com, grace@example.com)"}, []string{secret}...)
	})
	t.Run("control: CLOUDSDK_CORE_ACCOUNT naming a login gcloud holds", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		useGcloud(t, "ada@example.com", "grace@example.com")
		t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
		if _, stderr, code := runSecret("list"); code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
	})
	for _, gcloud := range []bool{true, false} {
		t.Run(fmt.Sprintf("CLOUDSDK_ACTIVE_CONFIG_NAME, gcloud on PATH %v", gcloud), func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			useGcloudConfigurations(t, "box", orderConfigurations)
			if !gcloud {
				t.Setenv("PATH", t.TempDir())
			}
			t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", secret)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

			_, stderr, code := runSecret("show", "SCRATCH_TEST")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{"CLOUDSDK_ACTIVE_CONFIG_NAME names a gcloud configuration this machine does not hold (not printed; gcloud configurations here: box, person)"}, []string{secret}...)
		})
	}
	t.Run("control: CLOUDSDK_ACTIVE_CONFIG_NAME naming a configuration this machine holds", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		holdScratchTest(fake)
		startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
		useGcloudConfigurations(t, "box", orderConfigurations)
		t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		if _, stderr, code := runSecret("show", "SCRATCH_TEST"); code != 0 {
			t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
		}
	})
}

// TestGcloudsStandardErrorIsNotPrinted: a gcloud call that fails is named by the call and its exit
// status, with the advice to run the same gcloud command to see its message, and never by what
// gcloud wrote on its standard error: a refused refresh writes the token endpoint's
// error_description there, which the server writes what it likes in. It holds for the login list
// and for the token. A gcloud that answers still signs the form in (the control).
func TestGcloudsStandardErrorIsNotPrinted(t *testing.T) {
	const secret = "hunter2pw-gcloudstderr-7c40b2"
	refused := `echo "ERROR: (gcloud.auth.print-access-token) There was a problem refreshing your current auth tokens: ('invalid_grant: echo of ` + secret + `', {'error': 'invalid_grant', 'error_description': 'echo of ` + secret + `'})" >&2; exit 1`
	for _, row := range []struct {
		name, authList, token string
		code                  int
		want                  string
	}{
		{"the token refresh", `printf '%s\n' '[{"account":"ada@example.com","status":"ACTIVE"}]'`, refused, 1,
			"`gcloud auth print-access-token` failed for ada@example.com: exit status 1: run the same gcloud command yourself to see its message"},
		{"the login list", strings.ReplaceAll(refused, "print-access-token", "list"), `printf '%s\n' 'unused'`, 1,
			"`gcloud auth list` failed: exit status 1: run the same gcloud command yourself to see its message"},
		{"control: a gcloud that answers", `printf '%s\n' '[{"account":"ada@example.com","status":"ACTIVE"}]'`, `printf '%s\n' ` + shellWord(stubTokenPrefix+"ada@example.com"), 0,
			"using the active gcloud account (gcloud configuration default): ada@example.com"},
	} {
		t.Run(row.name, func(t *testing.T) {
			useNoADC(t)
			script := strings.Join([]string{
				"#!/bin/sh",
				`case "$1 $2" in`,
				`"auth list") ` + row.authList + ` ;;`,
				`"config list") printf '%s\n' '{"auth":{}}' ;;`,
				`"auth print-access-token") ` + row.token + ` ;;`,
				`*) exit 2 ;;`,
				"esac",
				"",
			}, "\n")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))

			_, stderr, code := runSecret("list")
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, []string{secret, "error_description"}...)
		})
	}
}

// TestTheWellKnownCredentialsFileIsNamedByItsSettingNeverItsPath: CLOUDSDK_CONFIG names the
// directory of the well-known credentials file, and is where a person who pasted a credential in
// the wrong place puts it. A value that is no directory is not absent - a name too long for the
// file system (ENAMETOOLONG), or a file (ENOTDIR) - so the file is read and refused, naming the
// setting and the system's reason, never the path. A directory that holds a key file still signs
// in, and the line naming the sign-in names the setting, not the path (the control).
func TestTheWellKnownCredentialsFileIsNamedByItsSettingNeverItsPath(t *testing.T) {
	secret := "hunter2pw-" + "wellknown-" + strings.Repeat("a1b2c3d4", 40)
	const named = "the well-known credentials file (application_default_credentials.json in the directory CLOUDSDK_CONFIG names)"
	for _, row := range []struct {
		name string
		dir  func(t *testing.T) string
		code int
		want string
	}{
		{"a long credential in CLOUDSDK_CONFIG", func(*testing.T) string { return secret }, 1,
			"read " + named + ": open: file name too long"},
		{"CLOUDSDK_CONFIG names a file", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), secret[:40])
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, 1, "read " + named + ": open: not a directory"},
		{"control: CLOUDSDK_CONFIG names a directory that holds a key file", func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), secret[:40])
			writeServiceAccountFile(t, filepath.Join(dir, "application_default_credentials.json"), "gac@example.iam.gserviceaccount.com", startGoogleTokenServer(t))
			return dir
		}, 0, "using " + named + ": gac@example.iam.gserviceaccount.com"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			holdScratchTest(fake)
			startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			useGcloudConfigurations(t, "box", orderConfigurations)
			t.Setenv("CLOUDSDK_CONFIG", row.dir(t))
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			_, stderr, code := runSecret("show", "SCRATCH_TEST")
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, "hunter2pw-")
		})
	}
}

// TestAnAccountGooglesAnswerNamesPrintsOnlyWhereItIsAServiceAccounts: the metadata server's
// email, which the using line names, and Google userinfo's email for an account that is no
// person's, which --owner me's refusal names, are text whoever answers writes, line breaks
// included. Each prints only where it has a service account's shape; any other answer prints as
// fixed words. Control: a service account's email still prints in both places, so the check sees
// the answer.
func TestAnAccountGooglesAnswerNamesPrintsOnlyWhereItIsAServiceAccounts(t *testing.T) {
	planted := "hunter2pw-" + "accountanswer-" + "c71d"
	for _, row := range []struct {
		name, email, want string
		owner             bool
	}{
		{"the metadata server's email, two lines", planted + "\nagent-secrets secret list: forged line", "using the metadata server: " + metadataGenericPrincipal, false},
		{"control: the metadata server's email, a service account's", "machine@example.iam.gserviceaccount.com", "using the metadata server: machine@example.iam.gserviceaccount.com", false},
		{"userinfo's email, no person's", planted, "names the access token in CLOUDSDK_AUTH_ACCESS_TOKEN as an account that is no person's email (not printed", true},
		{"control: userinfo's email, Compute Engine's default service account", "123456789012-compute@developer.gserviceaccount.com", "as the service account 123456789012-compute@developer.gserviceaccount.com, which is no person's email", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			useNoGcloud(t)
			args := []string{"list"}
			if row.owner {
				real := gcpUserinfo
				gcpUserinfo = func(context.Context, *google.Credentials) (string, error) { return row.email, nil }
				t.Cleanup(func() { gcpUserinfo = real })
				t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-ada-token")
				args = []string{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}
			} else {
				useGCPIdentity(t, "", row.email)
			}
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			refuseValueReads(t)
			_, stderr, _ := runSecret(args...)
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.want}, "hunter2pw-", "forged line")
			if row.owner {
				requireNothingWritten(t, fake, broker)
			}
		})
	}
}

// TestACredentialsFilesAccountPrintsOnlyWhereItIsAServiceAccounts: the account a credentials
// file names - a key file's client_email, or the account in an impersonating file's
// service_account_impersonation_url - is text in a credential's file, so the using line and every
// store refusal name it only where it has a service account's shape. Control: a service account's
// email in either field still prints.
func TestACredentialsFilesAccountPrintsOnlyWhereItIsAServiceAccounts(t *testing.T) {
	planted := "hunter2pw-" + "fileaccount-" + "9e1d"
	const robot = "deploy-bot@example-project.iam.gserviceaccount.com"
	impersonating := func(account string) string {
		return `{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` + account + `:generateAccessToken"}`
	}
	for _, row := range []struct{ name, file, want string }{
		{"a key file's client_email that is no service account's", `{"type":"service_account","client_email":"` + planted + `"}`, "the service account the credentials file names (not printed"},
		{"an impersonated account that is no service account's", impersonating(planted), "the service account the credentials file names (not printed"},
		{"control: a key file's service account", `{"type":"service_account","client_email":"` + robot + `"}`, robot},
		{"control: an impersonated service account", impersonating(robot), robot},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := credentialsFilePrincipal(&google.Credentials{JSON: []byte(row.file)})
			wantNamedNotPrinted(t, "the principal", got, []string{row.want}, planted)
		})
	}
}
