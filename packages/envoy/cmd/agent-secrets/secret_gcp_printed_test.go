// packages/envoy/cmd/agent-secrets/secret_gcp_printed_test.go
//
// What the GCP store forms print about Google's servers and the credentials file
// GOOGLE_APPLICATION_CREDENTIALS names: the userinfo endpoint's status and registered code, never
// its body or a library's text; the variable, never the file's path or content. Each test puts a
// made-up secret where a person or a server could put one, runs the form, and reads its standard
// error; a control row in each shows the clean case still works and still prints what it did.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc/codes"
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
			if strings.Contains(stderr, secret) {
				t.Errorf("stderr %q holds the secret", stderr)
			}
			for _, want := range append(row.want, "--owner <email>") {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not say %q", stderr, want)
				}
			}
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
			for _, leaked := range []string{secret, "unknown credential type", "invalid character"} {
				if strings.Contains(stderr, leaked) {
					t.Errorf("stderr %q holds %q", stderr, leaked)
				}
			}
			for _, want := range row.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not say %q", stderr, want)
				}
			}
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
		if want := "using the credentials file GOOGLE_APPLICATION_CREDENTIALS names: " + gac + "\n"; !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not say %q", stderr, want)
		}
		if strings.Contains(stderr, secret) {
			t.Errorf("stderr %q holds the file's path", stderr)
		}
	})
}

// TestGCPPrintedCommandsCarryTheCredentialsFileAsItsVariable: a create signed in with the file
// GOOGLE_APPLICATION_CREDENTIALS names prints the commands that settle what it left with that
// variable as the shell holds it - `GOOGLE_APPLICATION_CREDENTIALS="$GOOGLE_APPLICATION_CREDENTIALS"`
// before agent-secrets and `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="$GOOGLE_APPLICATION_CREDENTIALS"`
// before gcloud - so each still signs in as the create did, in the shell that set it, and none
// spells the file's path. Under a gcloud login (the control) the commands still carry the login
// itself, which names no file.
func TestGCPPrintedCommandsCarryTheCredentialsFileAsItsVariable(t *testing.T) {
	const secret = "hunter2pw-cmdfile-27d0ae"
	for _, mode := range []struct {
		name   string
		signIn func(t *testing.T)
		agent  string
		gcloud string
	}{
		{"a key file", func(t *testing.T) {
			useGcloudConfigurations(t, "box", orderConfigurations)
			path := filepath.Join(t.TempDir(), secret, "credentials.json")
			writeServiceAccountFile(t, path, "gac@example.iam.gserviceaccount.com", startGoogleTokenServer(t))
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
		}, `GOOGLE_APPLICATION_CREDENTIALS="$GOOGLE_APPLICATION_CREDENTIALS" `, `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="$GOOGLE_APPLICATION_CREDENTIALS" `},
		{"control: a named gcloud login", func(t *testing.T) {
			useNoADC(t)
			useGcloud(t, "ada@example.com", "grace@example.com")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}, "CLOUDSDK_CORE_ACCOUNT=grace@example.com ", "CLOUDSDK_CORE_ACCOUNT=grace@example.com "},
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
				mode.signIn(t)
				startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				useStdin(t, "")

				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
				}
				if strings.Contains(stderr, secret) {
					t.Errorf("stderr %q holds the credentials file's path", stderr)
				}
				for _, want := range scenario.want(mode.agent, mode.gcloud) {
					if !strings.Contains(stderr, want) {
						t.Errorf("stderr %q does not name %s", stderr, want)
					}
				}
			})
		}
	}
}
