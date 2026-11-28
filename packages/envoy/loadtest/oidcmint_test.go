package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// The copy's subjects (manifests/listener.yaml, cluster.sh mint_identity), and one the listener
// lists nowhere.
const (
	testDispatchSubject = "system:serviceaccount:loadtest:default"
	testRunnerSubject   = "system:serviceaccount:loadtest:loadgen-runner"
	testStrangerSubject = "system:serviceaccount:loadtest:stranger"
	testMintAudience    = "envoy-listener-loadtest"
)

// issuerManifest is cluster/manifests/oidc-issuer.yaml as the copy's issuer pod runs it: the
// ConfigMap's keys, and the documents volume's mount and socat's EXEC address of its one container.
type issuerManifest struct {
	data      map[string]string
	mountPath string
	exec      string
}

// readIssuerManifest reads cluster/manifests/oidc-issuer.yaml, refusing one that holds no ConfigMap
// with serve.sh, or no Deployment whose one container mounts that ConfigMap.
func readIssuerManifest(t *testing.T) issuerManifest {
	t.Helper()
	f, err := os.Open(filepath.Join("cluster", "manifests", "oidc-issuer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var m issuerManifest
	decoder := yaml.NewDecoder(f)
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
			Spec     struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args         []string `yaml:"args"`
							VolumeMounts []struct {
								Name      string `yaml:"name"`
								MountPath string `yaml:"mountPath"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
						Volumes []struct {
							Name      string `yaml:"name"`
							ConfigMap struct {
								Name string `yaml:"name"`
							} `yaml:"configMap"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("oidc-issuer.yaml: %v", err)
		}
		switch doc.Kind {
		case "ConfigMap":
			m.data = doc.Data
		case "Deployment":
			pod := doc.Spec.Template.Spec
			if len(pod.Containers) != 1 || len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap.Name != doc.Metadata.Name {
				t.Fatalf("oidc-issuer.yaml's Deployment does not run one container over the ConfigMap %s: %+v", doc.Metadata.Name, pod)
			}
			for _, mount := range pod.Containers[0].VolumeMounts {
				if mount.Name == pod.Volumes[0].Name {
					m.mountPath = mount.MountPath
				}
			}
			if args := pod.Containers[0].Args; len(args) > 0 {
				m.exec = args[len(args)-1]
			}
		}
	}
	if m.data["serve.sh"] == "" || m.mountPath == "" {
		t.Fatalf("oidc-issuer.yaml holds no serve.sh or no mount of its ConfigMap: %+v", m)
	}
	return m
}

// minted is one oidc-mint run: the key's public half and each subject's token, and, for a run
// startCopyIssuer made, the private key, so a test can sign one more token under the served key.
type minted struct {
	key     oidcMintKey
	tokens  map[string]string
	private *ecdsa.PrivateKey
}

// mintWithKey is one run of oidc-mint's signing (mintOIDCWith) at issuer for subjects, on private.
func mintWithKey(t *testing.T, private *ecdsa.PrivateKey, issuer string, subjects ...string) minted {
	t.Helper()
	opts := oidcMintOptions{issuer: issuer, audience: testMintAudience, lifetime: time.Hour}
	for _, subject := range subjects {
		opts.subjects = append(opts.subjects, oidcMintSubject{subject: subject})
	}
	key, tokens, err := mintOIDCWith(private, opts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := minted{key: key, tokens: map[string]string{}, private: private}
	for i, subject := range subjects {
		m.tokens[subject] = tokens[i]
	}
	return m
}

// mintSubjects is one oidc-mint run at issuer for subjects, through the command itself: one -sub
// <subject>=<file> each, the key read from what it prints and each token from its file.
func mintSubjects(t *testing.T, issuer string, subjects ...string) minted {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-issuer", issuer, "-audience", testMintAudience, "-lifetime", "1h"}
	for i, subject := range subjects {
		args = append(args, "-sub", fmt.Sprintf("%s=%s/token-%d", subject, dir, i))
	}
	var stdout bytes.Buffer
	if err := oidcMintTo(args, &stdout); err != nil {
		t.Fatalf("oidc-mint %v: %v", args, err)
	}
	m := minted{tokens: map[string]string{}}
	if err := json.Unmarshal(stdout.Bytes(), &m.key); err != nil {
		t.Fatalf("oidc-mint printed %q, not the key's public half: %v", stdout.String(), err)
	}
	for i, subject := range subjects {
		token, err := os.ReadFile(fmt.Sprintf("%s/token-%d", dir, i))
		if err != nil {
			t.Fatal(err)
		}
		m.tokens[subject] = string(token)
	}
	return m
}

// startCopyIssuer serves oidc-issuer.yaml's ConfigMap as the copy's issuer pod does, on a loopback
// port: every placeholder filled from the issuer's URL and the key one signing run for subjects
// made (mintWithKey, on a key the test generates and keeps), the keys written into one directory,
// as the pod's volume holds them, and every connection handed to `sh serve.sh` on its standard
// input and output, as socat's EXEC address does, closed a second after the shell exits, as socat
// closes it. It answers the issuer's URL and that run.
func startCopyIssuer(t *testing.T, subjects ...string) (string, minted) {
	t.Helper()
	m := readIssuerManifest(t)
	if want := "EXEC:/bin/sh " + m.mountPath + "/serve.sh"; m.exec != want {
		t.Fatalf("oidc-issuer.yaml's socat runs %q, not %q, so this test serves another script than the pod does", m.exec, want)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	issuer := "http://" + listener.Addr().String()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	run := mintWithKey(t, private, issuer, subjects...)
	dir := t.TempDir()
	fill := strings.NewReplacer("__OIDC_ISSUER__", issuer, "__OIDC_KEY_ID__", run.key.KeyID,
		"__OIDC_KEY_X__", run.key.X, "__OIDC_KEY_Y__", run.key.Y)
	for name, value := range m.data {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fill.Replace(value)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				cmd := exec.Command("sh", filepath.Join(dir, "serve.sh"))
				cmd.Stdin, cmd.Stdout, cmd.Stderr = conn, conn, os.Stderr
				cmd.WaitDelay = time.Second
				_ = cmd.Run()
			}()
		}
	}()
	return issuer, run
}

// TestTheCopysIssuerServesTheKeyEveryMintedSubjectVerifiesUnder: the listener's own verifier
// (internal/oidc, the one cmd/listener builds from ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE)
// discovers the issuer oidc-issuer.yaml serves and verifies both tokens one oidc-mint run wrote,
// each as its own subject, under the one key set served; it refuses a token of another run's key
// and a verifier of another audience, so the pass is the served key's and the tokens' claims', not
// a verifier that takes anything.
func TestTheCopysIssuerServesTheKeyEveryMintedSubjectVerifiesUnder(t *testing.T) {
	issuer, run := startCopyIssuer(t, testDispatchSubject, testRunnerSubject)
	// Every read below - the discovery, each verify, the refusals and the 404 - carries the shared
	// wait's context and is asked again while the issuer has not answered it (issuerAnswered), so
	// no read of the `sh serve.sh` issuer this test starts has a span that decides the row.
	ctx := testwait.Context(t)

	// Built as cmd/listener builds it from ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE
	// (oidc.NewLazyVerifier), with the discovery its background run makes done here first.
	verifier := oidc.NewLazyVerifier(issuer, testMintAudience)
	issuerRead(t, "the listener's verifier to discover the copy's issuer", func() error { return verifier.Discover(ctx) })
	for _, subject := range []string{testDispatchSubject, testRunnerSubject} {
		claims, err := issuerAnswered(t, "the copy's issuer to answer the verify of "+subject+"'s token", func() (oidc.Claims, error) {
			return verifier.Verify(ctx, run.tokens[subject])
		})
		if err != nil {
			t.Fatalf("the listener's verifier refused %s's token: %v", subject, err)
		}
		if claims.Subject != subject {
			t.Errorf("%s's token verifies as %q", subject, claims.Subject)
		}
	}

	other := mintSubjects(t, issuer, testDispatchSubject)
	if _, err := issuerAnswered(t, "the copy's issuer to answer the verify of a token of another run's key", func() (oidc.Claims, error) {
		return verifier.Verify(ctx, other.tokens[testDispatchSubject])
	}); !errors.Is(err, oidc.ErrSignature) {
		t.Errorf("a token of a key the issuer does not serve was answered %v, not a signature failure", err)
	}
	elsewhere := oidc.NewLazyVerifier(issuer, "another-audience")
	issuerRead(t, "a verifier of another audience to discover the copy's issuer", func() error { return elsewhere.Discover(ctx) })
	if _, err := issuerAnswered(t, "the copy's issuer to answer the verify at a verifier of another audience", func() (oidc.Claims, error) {
		return elsewhere.Verify(ctx, run.tokens[testDispatchSubject])
	}); !errors.Is(err, oidc.ErrAudience) {
		t.Errorf("a minted token at a verifier of another audience was answered %v, not an audience failure", err)
	}

	answered := testwait.Until(t, "the copy's issuer to answer a path it serves nothing at", func() (statusRead, bool) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/elsewhere", nil)
		if err != nil {
			t.Fatalf("build the request for a path the issuer serves nothing at: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return statusRead{Err: err}, false
		}
		_ = response.Body.Close()
		return statusRead{Status: response.StatusCode}, true
	})
	if answered.Status != http.StatusNotFound {
		t.Errorf("the issuer answered %d for a path it serves nothing at, not 404", answered.Status)
	}
}

// statusRead is one read of a path of the copy's issuer: the status it answered, or the transport
// error that read ended with. Its fields are exported so that a wait that gives up prints the error
// itself, not a pointer: fmt calls no Error() on an error it reaches through an unexported field.
type statusRead struct {
	Status int
	Err    error
}

// issuerAnswer is one read of the copy's issuer through the listener's verifier: what it returned,
// and its error. Exported for the reason statusRead's fields are.
type issuerAnswer[T any] struct {
	Value T
	Err   error
}

// issuerAnswered asks read until the copy's issuer has answered it, and returns that answer. A read
// the issuer did not answer ended in a transport error, a *url.Error somewhere in its chain: the
// connection was refused, or the verifier's own attempt bound ended it (internal/oidc:
// discoveryAttemptTimeout 10 s, no second discovery within discoveryRetrySpacing 5 s, the key
// read's FetchTimeout; go-oidc returns the request's error as it is, and Keys.read wraps it with
// %w). Such a read is no verdict, since a loaded machine can end it, and the row asks again. Every
// other answer is the issuer answering and is returned at once: the refusals this row reads, and a
// fault the issuer answered, such as a discovery document or a key set served at the wrong path,
// that does not parse, or that names another issuer. oidc.ErrIssuerUnavailable is not the class to
// ask again on, because discovery wraps every one of those answered faults in it as well. The wait's
// one bound is the test binary's deadline.
func issuerAnswered[T any](t *testing.T, what string, read func() (T, error)) (T, error) {
	t.Helper()
	got := testwait.Until(t, what, func() (issuerAnswer[T], bool) {
		var a issuerAnswer[T]
		a.Value, a.Err = read()
		var unanswered *url.Error
		return a, !errors.As(a.Err, &unanswered)
	})
	return got.Value, got.Err
}

// issuerRead is issuerAnswered for a read whose answer is its error alone, such as Discover, and
// fails the test, naming the fault, when the issuer answered it with anything but success.
func issuerRead(t *testing.T, what string, read func() error) {
	t.Helper()
	if _, err := issuerAnswered(t, what, func() (struct{}, error) { return struct{}{}, read() }); err != nil {
		t.Fatalf("%s: the issuer answered with a fault: %v", what, err)
	}
}

// identityProbeBody is the send cluster.sh's check_dispatch_identity makes (IDENTITY_PROBE_SCRIPT):
// marked with the role -identity-probe, which no claim can name, and naming no target.
const identityProbeBody = `{"message":"identity probe","role":"-identity-probe"}`

// startCopysListener starts the listener binary as the copy's manifests/listener.yaml configures
// it, against the issuer at issuer and a NATS of its own, with ENVOY_OIDC_ACCOUNTS accounts and
// Dispatch's principal testDispatchSubject; it answers the process and its port.
func startCopysListener(t *testing.T, binary, issuer, accounts string) (*cmdtest.Process, int) {
	t.Helper()
	_, natsURL := testnats.Start(t)
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(binary)
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=loadtest",
		"NATS_URLS=" + natsURL,
		"ENVOY_API_TOKEN=listener-token-not-a-secret",
		"ENVOY_OIDC_ISSUER=" + issuer,
		"ENVOY_OIDC_AUDIENCE=" + testMintAudience,
		"ENVOY_OIDC_ACCOUNTS=" + accounts,
		"ENVOY_DISPATCH_PRINCIPAL=" + testDispatchSubject,
	}
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return cmdtest.Start(t, "the copy's listener", cmd), held.Port
}

// TestTheCopysListenerTakesTheSubjectsItListsUnderTheMintedKeyAndRefusesTheRest: the listener
// binary, configured as the copy's, starts against the issuer oidc-issuer.yaml serves and takes
// the three tokens one oidc-mint run wrote, all under the one served key, by its own list: the
// role mark from Dispatch's token (400 naming the role, the answer check_dispatch_identity reads),
// the runner's token as the runner, not as Dispatch (403 naming it), and the token whose subject
// ENVOY_OIDC_ACCOUNTS does not list refused (401, logged not_listed). A token signed by the served
// key with the right key id, subject and audience but another issuer is refused 401, logged
// reason issuer: the key alone admits no token. The control is the copy's fault before the fix:
// the same listener given an issuer it cannot reach takes no role mark from Dispatch's token. An
// older listener exits when it cannot read its issuer; this one, built from this tree, starts,
// reports "oidc": false on /healthz, answers that issuer's tokens 503 with Retry-After: 1 until it
// can read it, and logs that it retries.
func TestTheCopysListenerTakesTheSubjectsItListsUnderTheMintedKeyAndRefusesTheRest(t *testing.T) {
	binary := cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-listener", Target: "../cmd/listener"})
	issuer, run := startCopyIssuer(t, testDispatchSubject, testRunnerSubject, testStrangerSubject)
	listener, port := startCopysListener(t, binary, issuer, testDispatchSubject+","+testRunnerSubject)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	// /healthz answers 503 "starting" from the moment the listener serves it, before NATS connects,
	// until NATS is connected, its webhook and /v1 routes are open, its receive loop has started and
	// its interest copy has read its bucket, which may come after /v1 opens; a 200 "degraded"
	// listener serves /v1 too. Only "healthy" says all of that and that every dependency answers.
	deadline := time.Now().Add(90 * time.Second)
	answer := "none"
	for {
		response, err := http.Get(base + "/healthz")
		if err == nil {
			var health struct{ Status string }
			_ = json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if health.Status == "healthy" {
				break
			}
			answer = fmt.Sprintf("%d %q", response.StatusCode, health.Status)
		}
		select {
		case <-listener.Exited:
			t.Fatalf("the copy's listener exited before it was healthy:\n%s", listener.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy's listener was not healthy within 90 s (last /healthz answer %s):\n%s", answer, listener.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	send := func(token string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, base+"/v1/messages/send", strings.NewReader(identityProbeBody))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	if status, body := send(run.tokens[testDispatchSubject]); status != http.StatusBadRequest || !strings.Contains(body, `\"-identity-probe\" is no role a claim can name`) {
		t.Errorf("Dispatch's minted token was answered %d %s, not the 400 naming the role that says the mark was taken from it", status, body)
	}
	if status, body := send(run.tokens[testRunnerSubject]); status != http.StatusForbidden || !strings.Contains(body, "proves "+testRunnerSubject) {
		t.Errorf("the runner's minted token was answered %d %s, not the 403 that says it was verified as the runner", status, body)
	}
	if status, body := send(run.tokens[testStrangerSubject]); status != http.StatusUnauthorized {
		t.Errorf("a minted token whose subject the listener does not list was answered %d %s, not 401", status, body)
	}
	listener.WaitForOutput(t, "not_listed", testStrangerSubject)
	elsewhere := mintWithKey(t, run.private, issuer+"/elsewhere", testDispatchSubject)
	if elsewhere.key.KeyID != run.key.KeyID {
		t.Fatalf("the token of another issuer names key %s, not the served %s", elsewhere.key.KeyID, run.key.KeyID)
	}
	if status, body := send(elsewhere.tokens[testDispatchSubject]); status != http.StatusUnauthorized {
		t.Errorf("Dispatch's token signed by the served key with iss %s/elsewhere was answered %d %s, not 401", issuer, status, body)
	}
	listener.WaitForOutput(t, `"reason":"issuer"`)

	// The control: an issuer the listener cannot reach, as the cluster's own is from the fenced
	// namespace (there a connection that never answers, here one refused). The listener starts and
	// serves, but answers Dispatch's token for that issuer 503, not the 400 that says it took the
	// mark: the copy's fault before the fix, a token whose issuer the listener cannot read. The
	// port is one the test holds and nothing listens on: a closed listener's port is any process's
	// to take, and an issuer that takes it is reached.
	unreachable := "http://" + testport.Refused(t)
	strandedIssuer := unreachable
	stranded, strandedPort := startCopysListener(t, binary, strandedIssuer, testDispatchSubject)
	strandedBase := "http://127.0.0.1:" + strconv.Itoa(strandedPort)
	health := map[string]any{}
	for deadline := time.Now().Add(90 * time.Second); ; {
		if response, err := http.Get(strandedBase + "/healthz"); err == nil {
			health = map[string]any{}
			_ = json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if health["status"] == "healthy" {
				break
			}
		}
		select {
		case <-stranded.Exited:
			t.Fatalf("the listener given an unreachable issuer exited:\n%s", stranded.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener given an unreachable issuer was not healthy within 90 s (%v):\n%s", health, stranded.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if health["oidc"] != false {
		t.Errorf("the listener given an unreachable issuer reports oidc %v on /healthz, not false", health["oidc"])
	}
	request, err := http.NewRequest(http.MethodPost, strandedBase+"/v1/messages/send", strings.NewReader(identityProbeBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+mintWithKey(t, run.private, strandedIssuer, testDispatchSubject).tokens[testDispatchSubject])
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" {
		t.Errorf("Dispatch's token for the issuer the listener cannot reach was answered %d %s (Retry-After %q), not 503 with Retry-After 1", response.StatusCode, body, response.Header.Get("Retry-After"))
	}
	stranded.WaitForOutput(t, "oidc: discover the issuer; retrying in the background", strandedIssuer)
}

// TestOIDCMintWritesOneTokenPerSubjectUnderTheKeyItPrints: one run prints the key's public half
// alone, its id the RFC 7638 thumbprint of its point, and writes each -sub's token to that -sub's
// file, mode 600, each ES256 under that key id and carrying exactly its subject and the run's
// claims, living the lifetime from the moment it was minted.
func TestOIDCMintWritesOneTokenPerSubjectUnderTheKeyItPrints(t *testing.T) {
	dir := t.TempDir()
	issuer := "http://oidc-issuer.loadtest.svc:8080"
	files := map[string]string{testDispatchSubject: filepath.Join(dir, "dispatch"), testRunnerSubject: filepath.Join(dir, "runner")}
	// A file there already, as cluster.sh's mktemp leaves one, is replaced.
	if err := os.WriteFile(files[testDispatchSubject], nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	before := time.Now().Unix()
	if err := oidcMintTo([]string{"-issuer", issuer, "-audience", testMintAudience, "-lifetime", "168h",
		"-sub", testDispatchSubject + "=" + files[testDispatchSubject], "-sub", testRunnerSubject + "=" + files[testRunnerSubject]}, &stdout); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()
	var printed map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || len(printed) != 3 {
		t.Fatalf("oidc-mint printed %q, not the key's id and point alone", stdout.String())
	}
	key := oidcMintKey{KeyID: printed["kid"], X: printed["x"], Y: printed["y"]}
	for name, value := range printed {
		if len(value) != 43 || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			t.Errorf("%s %q is not 43 base64url characters, the shape cluster.sh renders", name, value)
		}
	}
	var public jose.JSONWebKey
	if err := public.UnmarshalJSON([]byte(`{"kty":"EC","crv":"P-256","x":"` + key.X + `","y":"` + key.Y + `"}`)); err != nil {
		t.Fatalf("the printed point is no P-256 key: %v", err)
	}
	thumbprint, err := public.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if want := base64.RawURLEncoding.EncodeToString(thumbprint); key.KeyID != want {
		t.Errorf("key id %q is not the point's thumbprint %q", key.KeyID, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("the token directory holds %v, not the two token files alone (err %v)", entries, err)
	}
	for subject, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s's token file is mode %o, not 600", subject, info.Mode().Perm())
		}
		token, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := jose.ParseSignedCompact(string(token), []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatalf("%s's token is no compact ES256 JWS: %v", subject, err)
		}
		if header := signed.Signatures[0].Header; header.KeyID != key.KeyID || header.ExtraHeaders["typ"] != "JWT" {
			t.Errorf("%s's token names key %q and type %v, not %q and JWT", subject, header.KeyID, header.ExtraHeaders["typ"], key.KeyID)
		}
		payload, err := signed.Verify(public.Key)
		if err != nil {
			t.Fatalf("%s's token does not verify under the printed point: %v", subject, err)
		}
		var claims struct {
			Iss, Sub, Aud string
			Iat, Exp      int64
		}
		var all map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil || json.Unmarshal(payload, &all) != nil {
			t.Fatalf("%s's token carries %s", subject, payload)
		}
		if len(all) != 5 || claims.Iss != issuer || claims.Sub != subject || claims.Aud != testMintAudience ||
			claims.Iat < before || claims.Iat > after || claims.Exp-claims.Iat != int64((168*time.Hour).Seconds()) {
			t.Errorf("%s's token carries %s, not iss %s, sub %s, aud %s, an iat of the run and an exp 168 h later", subject, payload, issuer, subject, testMintAudience)
		}
	}
}

// TestOIDCMintRefusesWhatNoListenerCouldVerify: a missing claim or -sub, a lifetime that is not
// positive, an issuer the listener's verifier could not discover or compare byte for byte, a
// subject the listener's account list could not name, and a subject or file given twice are
// refused, each naming what is wrong, before any key is made.
func TestOIDCMintRefusesWhatNoListenerCouldVerify(t *testing.T) {
	sub := "-sub=" + testDispatchSubject + "=" + filepath.Join(t.TempDir(), "token")
	good := []string{"-issuer", "http://oidc-issuer.loadtest.svc:8080", "-audience", testMintAudience, "-lifetime", "168h", sub}
	if _, err := oidcMintArgs(good); err != nil {
		t.Fatalf("the copy's own flags were refused: %v", err)
	}
	with := func(flag, value string) []string {
		args := append([]string(nil), good...)
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				args[i+1] = value
			}
		}
		return args
	}
	for _, row := range []struct {
		args []string
		want string
	}{
		{with("-issuer", ""), "-issuer is required"},
		{with("-audience", ""), "-audience is required"},
		{with("-lifetime", "0s"), "-lifetime must be a positive duration"},
		{good[:6], "-sub is required"},
		{with("-issuer", "oidc-issuer.loadtest.svc:8080"), "is not an absolute http or https URL"},
		{with("-issuer", "http://oidc-issuer.loadtest.svc:8080/"), "is not an absolute http or https URL"},
		{with("-issuer", "http://oidc-issuer.loadtest.svc:8080?x=1"), "is not an absolute http or https URL"},
		{with("-issuer", "ftp://oidc-issuer.loadtest.svc"), "is not an absolute http or https URL"},
		{append(good[:6:6], "-sub", testDispatchSubject), "is not <subject>=<file>"},
		{append(good[:6:6], "-sub", testDispatchSubject+"="), "is not <subject>=<file>"},
		{append(good[:6:6], "-sub", "default=/tmp/x"), `subject "default" is not system:serviceaccount:<namespace>:<name>`},
		{append(good[:6:6], "-sub", "system:serviceaccount:loadtest:a:b=/tmp/x"), "is not system:serviceaccount:<namespace>:<name>"},
		{append(good[:6:6], "-sub", "system:serviceaccount::default=/tmp/x"), "is not system:serviceaccount:<namespace>:<name>"},
		{append(append([]string(nil), good...), "-sub", testDispatchSubject+"=/tmp/other"), "subject " + testDispatchSubject + " is given twice"},
		{append(append([]string(nil), good...), "-sub", testRunnerSubject+"="+strings.TrimPrefix(sub, "-sub="+testDispatchSubject+"=")), "is given twice"},
		{append(append([]string(nil), good...), "extra"), `unexpected argument "extra"`},
	} {
		if _, err := oidcMintArgs(row.args); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("oidc-mint %v was answered %v, not an error naming %q", row.args, err, row.want)
		}
	}
}
