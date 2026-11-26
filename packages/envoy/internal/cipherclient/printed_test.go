// packages/envoy/internal/cipherclient/printed_test.go
//
// What this package prints of a setting, a credential file or a server's answer: never a part of
// a credential a person put where a setting expected a URL, a file's path or a plain value, and
// never the content of a file that holds one. Each refusal still names the setting to look at,
// and a clean value prints whole, which is each test's control.

package cipherclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/testport"
)

// printedSecret stands for a credential in a place it does not belong. No printed text holds it.
const printedSecret = "hunter2pwPRINTED"

// exampleFlow is a device-flow client of a made-up client at issuer; its made-up client value is
// built rather than written as a literal beside a credential's field name.
func exampleFlow(issuer string) *DeviceFlow {
	client := "example-device-client"
	return &DeviceFlow{Issuer: issuer, ClientID: client, ClientSecret: client + "-value"}
}

// holdsNoSecret fails t when text holds printedSecret, or the user name the URL probes carry.
func holdsNoSecret(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, printedSecret) || strings.Contains(text, "tokuser9f3") {
		t.Errorf("%s = %q, which holds the credential", what, text)
	}
}

// namesAll fails t unless text names each of named.
func namesAll(t *testing.T, what, text string, named ...string) {
	t.Helper()
	for _, name := range named {
		if !strings.Contains(text, name) {
			t.Errorf("%s = %q, want it to name %q", what, text, name)
		}
	}
}

// TestATokenFilePrintsItsSettingAndNeverItsPath: AGENT_SECRETS_MACHINE_TOKEN_FILE names a file
// that holds a credential, and a person who pasted the token itself there would see it in every
// refusal and in every log line naming the identity. So the file prints as its setting: in the
// read error, in the empty-file refusal, in Describe, and in the trade's error and log line.
func TestATokenFilePrintsItsSettingAndNeverItsPath(t *testing.T) {
	dir := t.TempDir()
	missing := NewProjectedToken(filepath.Join(dir, printedSecret))
	_, _, err := missing.Token(context.Background())
	if err == nil {
		t.Fatal("Token read a token file that is not there")
	}
	holdsNoSecret(t, "the missing file's error", err.Error())
	namesAll(t, "the missing file's error", err.Error(), MachineTokenFileVar, "no such file or directory")
	holdsNoSecret(t, "Describe", missing.Describe())
	namesAll(t, "Describe", missing.Describe(), MachineTokenFileVar)

	empty := filepath.Join(dir, "empty-"+printedSecret)
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewProjectedToken(empty).Token(context.Background()); err == nil || !strings.Contains(err.Error(), MachineTokenFileVar) {
		t.Errorf("the empty file's error = %v, want one naming %s", err, MachineTokenFileVar)
	} else {
		holdsNoSecret(t, "the empty file's error", err.Error())
	}

	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "never", "token_type": "DPoP", "expires_in": 60}
	})
	var log bytes.Buffer
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: missing, Log: slog.New(slog.NewTextHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Token(context.Background())
	if err == nil {
		t.Fatal("the trade went ahead with no token")
	}
	holdsNoSecret(t, "the trade's error", err.Error())
	namesAll(t, "the trade's error", err.Error(), MachineTokenFileVar)
	// Token answers the failure and logs nothing. The renewal loop logs it before it waits to
	// ask again, and that first wait ends the loop here.
	loop, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.after = func(time.Duration) <-chan time.Time {
		cancel()
		return nil
	}
	if err := client.Run(loop, func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token from a file that is not there")
		return nil
	}); err != nil {
		t.Fatalf("Run = %v, want the failed trade logged and asked again", err)
	}
	namesAll(t, "the client's log", log.String(), "asking again", MachineTokenFileVar)
	holdsNoSecret(t, "the client's log", log.String())
}

// TestASecretFileThatCannotBeReadIsNamedBySetting: the sign-in client's secret is read from the
// file AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE names, and os.ReadFile's error repeats that path,
// which for a person who pasted the secret itself is the secret. The refusal names the setting
// and the system's reason alone.
func TestASecretFileThatCannotBeReadIsNamedBySetting(t *testing.T) {
	env := map[string]string{
		SignInClientIDVar:         "example-device-client",
		SignInClientSecretFileVar: filepath.Join(t.TempDir(), printedSecret),
	}
	_, err := SignInFlow(func(name string) string { return env[name] })
	if err == nil {
		t.Fatal("SignInFlow read a secret file that is not there")
	}
	holdsNoSecret(t, "SignInFlow's error", err.Error())
	namesAll(t, "SignInFlow's error", err.Error(), SignInClientSecretFileVar, "no such file or directory")
	// Control: a file that is there is read.
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFile, []byte("example-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env[SignInClientSecretFileVar] = secretFile
	flow, err := SignInFlow(func(name string) string { return env[name] })
	if err != nil || flow.ClientSecret != "example-secret" {
		t.Fatalf("SignInFlow = %+v, %v; want the secret the file holds", flow, err)
	}
}

// TestARefusedSettingIsNamedAndNotEchoed: a value this package refuses from the environment - an
// identity kind it does not know, a project list that is not one - is where a credential pasted
// into the wrong variable shows up, so the refusal names the setting and what it takes, and not
// the value. A value it accepts is read. An identity missing one of its variables names that one,
// and none of the values of those that are set.
func TestARefusedSettingIsNamedAndNotEchoed(t *testing.T) {
	env := map[string]string{IdentityVar: printedSecret}
	getenv := func(name string) string { return env[name] }
	_, err := MachineIdentity(getenv)
	if err == nil {
		t.Fatal("MachineIdentity took an identity kind it does not know")
	}
	holdsNoSecret(t, "MachineIdentity's error", err.Error())
	namesAll(t, "MachineIdentity's error", err.Error(), IdentityVar, IdentityKubernetes, IdentityMetadata, IdentityPool, IdentityGitHubActions)

	run := map[string]string{
		ActionsIDTokenRequestURLVar:   "https://token.actions.example/idtoken?api-version=" + printedSecret,
		ActionsIDTokenRequestTokenVar: printedSecret,
		MachineAudienceVar:            "https://cipher.example/" + printedSecret,
	}
	for _, missing := range []string{ActionsIDTokenRequestURLVar, ActionsIDTokenRequestTokenVar, MachineAudienceVar} {
		t.Run(IdentityGitHubActions+" without "+missing, func(t *testing.T) {
			env := map[string]string{IdentityVar: IdentityGitHubActions}
			for name, value := range run {
				if name != missing {
					env[name] = value
				}
			}
			source, err := MachineIdentity(func(name string) string { return env[name] })
			want := missing + " is required by " + IdentityVar + "=" + IdentityGitHubActions
			if err == nil || err.Error() != want {
				t.Fatalf("MachineIdentity = %v, %v; want %q", source, err, want)
			}
		})
	}

	for _, projects := range []string{"lower-" + printedSecret, "HUNTER2 HUNTER2"} {
		env[ProjectsVar] = projects
		if _, err := TradeProjects(getenv); err == nil {
			t.Errorf("TradeProjects took %q", projects)
		} else {
			holdsNoSecret(t, "TradeProjects' error", err.Error())
			if strings.Contains(err.Error(), "HUNTER2") || !strings.Contains(err.Error(), ProjectsVar) {
				t.Errorf("TradeProjects' error = %q, want one naming %s and not the value", err, ProjectsVar)
			}
		}
	}
	env[ProjectsVar] = "INFRA BET"
	if projects, err := TradeProjects(getenv); err != nil || strings.Join(projects, " ") != "BET INFRA" {
		t.Errorf("TradeProjects = %v, %v; want BET INFRA", projects, err)
	}
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) { return 500, nil })
	if _, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: &scriptedSource{}, Projects: []string{"lower-" + printedSecret}}); err == nil {
		t.Error("New took a project list that is not one")
	} else {
		holdsNoSecret(t, "New's error", err.Error())
	}
}

// TestADeviceLoginIsRefusedByName: device-login is the TypeScript client's own identity, which a
// laptop sets for its sessions, and no Go tool trades it. A Go tool started on that laptop says
// so by name, rather than calling it a kind it does not know.
func TestADeviceLoginIsRefusedByName(t *testing.T) {
	env := map[string]string{IdentityVar: "device-login"}
	source, err := MachineIdentity(func(name string) string { return env[name] })
	if err == nil || source != nil {
		t.Fatalf("MachineIdentity = %#v, %v; want device-login refused", source, err)
	}
	namesAll(t, "MachineIdentity's error", err.Error(), IdentityVar, "device-login", "TypeScript",
		IdentityKubernetes, IdentityMetadata, IdentityPool, IdentityGitHubActions)
}

// TestGitHubActionsDescribePrintsNeitherTheRequestURLNorItsToken: GitHub gives a job its request
// URL and its request token, and either can carry what lets another ask for the job's tokens, so
// Describe, which every trade's log line and error carries, names each by its variable alone. The
// audience prints as written where it is a plain URL, and by its variable otherwise.
func TestGitHubActionsDescribePrintsNeitherTheRequestURLNorItsToken(t *testing.T) {
	env := map[string]string{
		IdentityVar:                   IdentityGitHubActions,
		ActionsIDTokenRequestURLVar:   "https://tokuser9f3:" + printedSecret + "@token.actions.example/idtoken?api-version=2.0",
		ActionsIDTokenRequestTokenVar: printedSecret,
		MachineAudienceVar:            "https://cipher.example",
	}
	getenv := func(name string) string { return env[name] }
	source, err := MachineIdentity(getenv)
	if err != nil {
		t.Fatalf("MachineIdentity: %v", err)
	}
	holdsNoSecret(t, "Describe", source.Describe())
	namesAll(t, "Describe", source.Describe(), "GitHub Actions", ActionsIDTokenRequestURLVar, ActionsIDTokenRequestTokenVar, MachineAudienceVar, "https://cipher.example")

	env[MachineAudienceVar] = "https://cipher.example/?token=" + printedSecret
	source, err = MachineIdentity(getenv)
	if err != nil {
		t.Fatalf("MachineIdentity: %v", err)
	}
	holdsNoSecret(t, "Describe", source.Describe())
	namesAll(t, "Describe", source.Describe(), ActionsIDTokenRequestURLVar, ActionsIDTokenRequestTokenVar, MachineAudienceVar)
}

// TestAnIssuerThatCarriesACredentialIsNotPrinted: AGENT_SECRETS_SIGNIN_ISSUER is a URL, and every
// refusal of the sign-in prints it only as internal/redact's allow-list does - whole when it is a
// plain URL, not at all when it carries a user name, a password or a query - beside the setting's
// name, with the failure's class rather than go-oidc's or net/http's own text, which repeats the
// URL. A plain issuer that answers nothing prints whole, with its class: the control.
func TestAnIssuerThatCarriesACredentialIsNotPrinted(t *testing.T) {
	// A port held refused for the whole test (testport.Refused), not one a closed listener freed,
	// which another process on the machine can take.
	closed := testport.Refused(t)
	for _, issuer := range []string{
		"http://tokuser9f3:" + printedSecret + "@" + closed,
		"http://" + printedSecret + "@" + closed,
		"http://" + closed + "/?token=" + printedSecret,
		"http://" + closed + "/%zz" + printedSecret,
	} {
		t.Run(issuer, func(t *testing.T) {
			flow := exampleFlow(issuer)
			for name, call := range map[string]func() error{
				"Start":   func() error { _, err := flow.Start(context.Background()); return err },
				"Refresh": func() error { _, err := flow.Refresh(context.Background(), "a-refresh-token"); return err },
			} {
				err := call()
				if err == nil {
					t.Fatalf("%s reached an issuer that is not there", name)
				}
				holdsNoSecret(t, name+"'s error", err.Error())
				namesAll(t, name+"'s error", err.Error(), SignInIssuerVar)
			}
		})
	}
	t.Run("a plain issuer", func(t *testing.T) {
		flow := exampleFlow("http://" + closed)
		_, err := flow.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "http://"+closed) || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("Start's error = %v, want one naming http://%s and the refused connection", err, closed)
		}
	})
}

// TestAnIssuersAnswerPrintsItsStatusAndCodeAlone: what an issuer answers - a discovery document it
// could not serve, a refusal of the device code or of a refresh - prints as its status and, for
// an OAuth refusal, its error code, which is one plain word. Never its body or its description,
// which a server can fill with anything, a request it echoes included.
func TestAnIssuersAnswerPrintsItsStatusAndCodeAlone(t *testing.T) {
	var base string
	discoveryFails := true
	base = newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if discoveryFails {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("echo of client_secret=" + printedSecret))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"issuer":"` + base + `","device_authorization_endpoint":"` + base + `/device","token_endpoint":"` + base + `/token","authorization_endpoint":"` + base + `/auth","jwks_uri":"` + base + `/jwks"}`))
		case "/device":
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("echo of " + printedSecret))
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_client","error_description":"echo of ` + printedSecret + `"}`))
		}
	})
	flow := exampleFlow(base)
	if _, err := flow.Start(context.Background()); err == nil {
		t.Fatal("Start read a discovery document the issuer did not serve")
	} else {
		holdsNoSecret(t, "the failed discovery", err.Error())
		namesAll(t, "the failed discovery", err.Error(), "HTTP 500")
	}
	discoveryFails = false
	flow = exampleFlow(base)
	if _, err := flow.Start(context.Background()); err == nil {
		t.Fatal("Start took a device code the issuer refused")
	} else {
		holdsNoSecret(t, "the refused device code", err.Error())
		namesAll(t, "the refused device code", err.Error(), "HTTP 502")
	}
	if _, err := flow.Refresh(context.Background(), "a-refresh-token"); err == nil {
		t.Fatal("Refresh took a refresh the issuer refused")
	} else {
		holdsNoSecret(t, "the refused refresh", err.Error())
		namesAll(t, "the refused refresh", err.Error(), "HTTP 400", "invalid_client")
	}
}

// TestDiscoveryGoesThroughTheClientEveryOtherCallDoes: a DeviceFlow with no client of its own
// reads the discovery document through the client the device and refresh calls use - the one the
// context carries (oauth2.HTTPClient), else http.DefaultClient - and not through a client of
// discovery's own, which a proxy, a CA bundle or a test transport set on those would not reach.
func TestDiscoveryGoesThroughTheClientEveryOtherCallDoes(t *testing.T) {
	const issuer = "https://issuer.invalid"
	document := `{"issuer":"` + issuer + `","device_authorization_endpoint":"` + issuer + `/device","token_endpoint":"` + issuer + `/token","authorization_endpoint":"` + issuer + `/auth","jwks_uri":"` + issuer + `/jwks"}`
	answering := func(asked *int) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*asked++
			if r.URL.String() != issuer+"/.well-known/openid-configuration" {
				return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: r}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(document)), Request: r}, nil
		})}
	}
	for name, ctx := range map[string]func(t *testing.T, client *http.Client) context.Context{
		"the context's client": func(_ *testing.T, client *http.Client) context.Context {
			return context.WithValue(context.Background(), oauth2.HTTPClient, client)
		},
		"http.DefaultClient": func(t *testing.T, client *http.Client) context.Context {
			saved := http.DefaultClient
			http.DefaultClient = client
			t.Cleanup(func() { http.DefaultClient = saved })
			return context.Background()
		},
	} {
		t.Run(name, func(t *testing.T) {
			var asked int
			config, err := exampleFlow(issuer).discover(ctx(t, answering(&asked)))
			if err != nil {
				t.Fatalf("discover = %v, want the document %s answered", err, name)
			}
			if asked == 0 || config.Endpoint.DeviceAuthURL != issuer+"/device" {
				t.Fatalf("discover asked %s %d times and read the device endpoint %q", name, asked, config.Endpoint.DeviceAuthURL)
			}
		})
	}
}

// TestARedirectedDiscoveryPrintsWhereItEnded: the status a failed discovery prints is the last
// request's, so a 302 whose target refuses the connection prints as the refused connection it
// ended in, and unwraps to it, not as the 302 that sent it there. A discovery the issuer answered
// 404 prints that status, and its error still unwraps to go-oidc's own: the control. Each status is
// looked for as the error prints one ("HTTP 302"), never as its digits alone, which the issuer's
// own address in the same error can hold (port 33025 did).
func TestARedirectedDiscoveryPrintsWhereItEnded(t *testing.T) {
	// A port held refused for the whole test (testport.Refused), not one a closed listener freed.
	closed := testport.Refused(t)
	redirected := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+closed+"/.well-known/openid-configuration", http.StatusFound)
	})
	_, err := exampleFlow(redirected).Start(context.Background())
	if err == nil {
		t.Fatal("Start read a discovery document from a port that is closed")
	}
	namesAll(t, "the redirected discovery", err.Error(), "connection refused")
	if strings.Contains(err.Error(), "HTTP 302") || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("the redirected discovery = %q (errors.Is ECONNREFUSED: %t), want the refused connection it ended in", err, errors.Is(err, syscall.ECONNREFUSED))
	}

	missing := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("echo of " + printedSecret))
	})
	_, err = exampleFlow(missing).Start(context.Background())
	if err == nil {
		t.Fatal("Start read a discovery document the issuer answered 404")
	}
	holdsNoSecret(t, "the 404 discovery", err.Error())
	namesAll(t, "the 404 discovery", err.Error(), "the server answered HTTP 404")
	reached := false
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
		reached = reached || strings.Contains(cause.Error(), printedSecret)
	}
	if !reached {
		t.Errorf("the 404 discovery %q does not unwrap to go-oidc's error, which quotes the body", err)
	}
}

// TestADiscoveryWhoseBodyFailsPrintsTheReadFailure: a discovery answer whose status is 200 but
// whose body the client could not read whole - the connection cut half way, a deadline passing
// in the middle of it - prints as the read failure's class beside the issuer's setting, and
// unwraps to that failure, never as a document that names another issuer, which would send the
// reader after a mismatch that is not there. Nothing of the half-read body prints. A 200 whose
// whole document names another issuer still prints as that mismatch: the control.
func TestADiscoveryWhoseBodyFailsPrintsTheReadFailure(t *testing.T) {
	const mismatch = "names another issuer or is not one"
	half := `{"issuer":"echo of ` + printedSecret + `","device_authorization_endpoint":`
	cut := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the discovery's connection: %v", err)
			return
		}
		defer conn.Close()
		buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n" + half)
		buffered.Flush()
	})
	_, err := exampleFlow(cut).Start(context.Background())
	if err == nil {
		t.Fatal("Start read a discovery document whose connection was cut")
	}
	holdsNoSecret(t, "the cut discovery", err.Error())
	namesAll(t, "the cut discovery", err.Error(), SignInIssuerVar, "the request failed")
	if strings.Contains(err.Error(), mismatch) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("the cut discovery = %q (errors.Is ErrUnexpectedEOF: %t), want the cut read it ended in", err, errors.Is(err, io.ErrUnexpectedEOF))
	}

	stalled := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(half))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = exampleFlow(stalled).Start(ctx)
	if err == nil {
		t.Fatal("Start read a discovery document whose deadline passed in the middle of its body")
	}
	holdsNoSecret(t, "the stalled discovery", err.Error())
	namesAll(t, "the stalled discovery", err.Error(), SignInIssuerVar, "timed out")
	if strings.Contains(err.Error(), mismatch) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the stalled discovery = %q (errors.Is DeadlineExceeded: %t), want the deadline it ended in", err, errors.Is(err, context.DeadlineExceeded))
	}

	another := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"issuer":"https://another-issuer.example"}`))
	})
	_, err = exampleFlow(another).Start(context.Background())
	if err == nil {
		t.Fatal("Start read a discovery document that names another issuer")
	}
	namesAll(t, "the other issuer's discovery", err.Error(), SignInIssuerVar, mismatch)
}

// TestAnIssuerWhoseEndpointsRefuseTheConnectionPrintsTheClass: Start, Wait and Refresh each send
// a request past discovery, to the device or the token endpoint the issuer's document names, and
// when that endpoint refuses the connection each prints the refusal's class beside the issuer's
// setting, and unwraps to it. Nothing prints of the endpoint's URL, which the document can fill
// with a user name, a password and a query, nor of an issuer that carries a credential. A plain
// issuer prints whole: the control.
func TestAnIssuerWhoseEndpointsRefuseTheConnectionPrintsTheClass(t *testing.T) {
	// A socket held bound and never listening until the test ends (testport.Refused): every dial
	// to it is refused, and no other process on the machine can take the port meanwhile, as one
	// could take a port a closed listener freed.
	closed := testport.Refused(t)
	endpoint := "http://tokuser9f3:" + printedSecret + "@" + closed
	// The document names as its issuer the one the request asked for, user name and password
	// included, as an issuer that echoes its caller would.
	issuer := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked := "http://" + r.Host
		if user, pass, ok := r.BasicAuth(); ok {
			asked = "http://" + user + ":" + pass + "@" + r.Host
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                        asked,
			"device_authorization_endpoint": endpoint + "/device?token=" + printedSecret,
			"token_endpoint":                endpoint + "/token?token=" + printedSecret,
			"authorization_endpoint":        endpoint + "/auth",
			"jwks_uri":                      endpoint + "/jwks",
		})
	})
	host := strings.TrimPrefix(issuer, "http://")
	for _, row := range []struct {
		name, issuer string
		named        []string
	}{
		{"an issuer that carries a credential", "http://tokuser9f3:" + printedSecret + "@" + host, nil},
		{"control: a plain issuer", issuer, []string{"(" + SignInIssuerVar + " names " + issuer + ")"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			flow := exampleFlow(row.issuer)
			ctx := context.Background()
			_, startErr := flow.Start(ctx)
			_, waitErr := flow.Wait(ctx, &oauth2.DeviceAuthResponse{DeviceCode: "a-device-code", Interval: 1})
			_, refreshErr := flow.Refresh(ctx, "a-refresh-token")
			for name, err := range map[string]error{"Start": startErr, "Wait": waitErr, "Refresh": refreshErr} {
				if err == nil {
					t.Errorf("%s reached an endpoint that refuses the connection", name)
					continue
				}
				holdsNoSecret(t, name+"'s error", err.Error())
				namesAll(t, name+"'s error", err.Error(), append([]string{SignInIssuerVar, "connection refused"}, row.named...)...)
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Errorf("%s's error = %q, want it to unwrap to the refused connection", name, err)
				}
			}
		})
	}
}

// roundTripFunc is a transport that answers every request with f.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAnIdentityTokenThatDoesNotParseIsNotQuoted: the identity token a sign-in answers is a
// credential, and go-jose's and encoding/json's errors quote the parts of it they could not read
// - an algorithm name in its header, a value in its claims - so neither reaches the refusal.
func TestAnIdentityTokenThatDoesNotParseIsNotQuoted(t *testing.T) {
	segment := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, token := range []string{
		segment(`{"alg":"`+printedSecret+`"}`) + "." + segment(`{"email":"ada@example.com"}`) + ".c2ln",
		segment(`{"alg":"RS256"}`) + "." + segment(`{"email":`+printedSecret+`}`) + ".c2ln",
		segment(`{"alg":"RS256"}`) + "." + segment(`{"email":123456789012345678901234567890`+"9"+`}`) + ".c2ln",
	} {
		if _, err := PersonOf(token); err == nil {
			t.Errorf("PersonOf read a token that does not parse: %q", token)
		} else {
			holdsNoSecret(t, "PersonOf's error", err.Error())
			if strings.Contains(err.Error(), "1234567890") {
				t.Errorf("PersonOf's error = %q, which quotes the token's claims", err)
			}
		}
	}
	// Control: a token that parses names the person its email claim does, in lower case.
	good := segment(`{"alg":"RS256"}`) + "." + segment(`{"email":"Ada@Example.com"}`) + ".c2ln"
	if person, err := PersonOf(good); err != nil || person != "ada@example.com" {
		t.Errorf("PersonOf(a token that parses) = %q, %v; want ada@example.com", person, err)
	}
}

// TestTheSignInStorePrintsItsSettingAndNeverItsPathOrContent: the sign-in records hold a refresh
// token and a box key, so they are credential files: no refusal names their directory's path
// (AGENT_SECRETS_SIGNIN_DIR's value) or quotes their content. The store prints as its setting.
func TestTheSignInStorePrintsItsSettingAndNeverItsPathOrContent(t *testing.T) {
	parent := t.TempDir()
	blocked := filepath.Join(parent, printedSecret)
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := SignInStore{Dir: filepath.Join(blocked, "signins")}
	if err := store.Write(signedIn(t, "ada@example.com")); err == nil {
		t.Fatal("Write kept a record under a file")
	} else {
		holdsNoSecret(t, "Write's error", err.Error())
		namesAll(t, "Write's error", err.Error(), SignInDirVar)
	}
	if _, err := (SignInStore{Dir: blocked}).List(); err == nil {
		t.Fatal("List read a directory that is a file")
	} else {
		holdsNoSecret(t, "List's error", err.Error())
		namesAll(t, "List's error", err.Error(), SignInDirVar)
	}

	dir := filepath.Join(parent, "store-"+printedSecret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	good, err := json.Marshal(signedIn(t, "ada@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		content string
		reads   bool
	}{
		{`{"person":"ada@example.com","refresh_token":"` + printedSecret + `","box_key":"k","signed_in_at":"` + printedSecret + `"}`, false},
		{`{"person":"ada@example.com","refresh_token":` + printedSecret + `}`, false},
		{`{"person":"ada@example.com"}`, false},
		// Control: the record `agent-secrets login` keeps is read.
		{string(good), true},
	} {
		if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(row.content), 0o600); err != nil {
			t.Fatal(err)
		}
		records, err := (SignInStore{Dir: dir}).List()
		switch {
		case row.reads:
			if err != nil || len(records) != 1 || records[0].Person != "ada@example.com" {
				t.Errorf("List = %v, %v; want the one record a sign-in keeps", records, err)
			}
		case err == nil:
			t.Errorf("List read a record that is not one: %s", row.content)
		default:
			holdsNoSecret(t, "the bad record's error", err.Error())
			namesAll(t, "the bad record's error", err.Error(), SignInDirVar)
		}
	}
}

// TestASignInStoreWriteThatFailsPastItsDirectoryNamesItsSetting: Write makes the store's
// directory, holds it to this user, and puts the record in place, and a failure of either of the
// last two - a directory whose mode nobody may change, a directory standing where the record's
// file goes - names AGENT_SECRETS_SIGNIN_DIR and the system's reason, never the directory's path.
// With the way clear the same store keeps the record and List reads it: the control.
func TestASignInStoreWriteThatFailsPastItsDirectoryNamesItsSetting(t *testing.T) {
	ada := signedIn(t, "ada@example.com")
	t.Run("a directory whose mode cannot change", func(t *testing.T) {
		// A process's /proc directory: Linux refuses every chmod of it, root's included, so the
		// probe changes no mode on the machine whoever runs it.
		if info, err := os.Stat("/proc/self"); err != nil || !info.IsDir() {
			t.Skip("no /proc/self directory, so nothing here refuses a chmod to every user")
		}
		link := filepath.Join(t.TempDir(), printedSecret)
		if err := os.Symlink("/proc/self", link); err != nil {
			t.Fatal(err)
		}
		err := SignInStore{Dir: link}.Write(ada)
		if err == nil {
			t.Fatal("Write held a directory whose mode cannot change to this user")
		}
		holdsNoSecret(t, "the refused chmod", err.Error())
		namesAll(t, "the refused chmod", err.Error(), SignInDirVar, "chmod: operation not permitted")
	})
	t.Run("a directory where the record goes", func(t *testing.T) {
		store := SignInStore{Dir: filepath.Join(t.TempDir(), printedSecret, "signins")}
		if err := os.MkdirAll(store.Path(ada.Person), 0o700); err != nil {
			t.Fatal(err)
		}
		err := store.Write(ada)
		if err == nil {
			t.Fatal("Write put a record in place over a directory")
		}
		// os.Rename refuses a directory where the file goes as EEXIST, on every Unix alike.
		holdsNoSecret(t, "the refused rename", err.Error())
		namesAll(t, "the refused rename", err.Error(), SignInDirVar, "rename: file exists")

		if err := os.Remove(store.Path(ada.Person)); err != nil {
			t.Fatal(err)
		}
		if err := store.Write(ada); err != nil {
			t.Fatalf("Write with the way clear: %v", err)
		}
		if records, err := store.List(); err != nil || len(records) != 1 || records[0].Person != ada.Person {
			t.Fatalf("List = %v, %v; want the record Write kept", records, err)
		}
	})
}
