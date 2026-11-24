package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARenewalRefusalPrintsNoCredentialItsAnswerEchoes: something in the broker's place answers a
// pod's renewal and echoes the request it was sent: a 502 page that repeats its JSON body (the pod
// token), an envelope that repeats its Proof header, and a terminal envelope that repeats it too.
// The renewal's error, the --pod refusal line (podRefusal) and renewLoop's stderr line hold none
// of it: doStatus reads every word of a refused answer through the record of the request it sent
// (redact.Carried), whatever the answer's shape. Captures agent-secrets.answer-echo/<case>,
// /<case>/pod-refusal and /<case>/renew-stderr.
func TestARenewalRefusalPrintsNoCredentialItsAnswerEchoes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		status int
		answer func(body []byte, proof string) string
	}{
		{"502-page-echoes-body", http.StatusBadGateway, func(body []byte, _ string) string {
			return "bad gateway; the upstream refused " + string(body)
		}},
		{"401-envelope-echoes-proof", http.StatusUnauthorized, func(_ []byte, proof string) string {
			return `{"code":"PROOF_INVALID","error":"the proof ` + proof + ` is not one this broker reads"}`
		}},
		{"403-terminal-envelope-echoes-proof", http.StatusForbidden, func(_ []byte, proof string) string {
			return `{"code":"ENROLLMENT_NOT_LIVE","error":"not live; the request carried Proof ` + proof + `"}`
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var proofs []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				proof := r.Header.Get("Proof")
				mu.Lock()
				proofs = append(proofs, proof)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.answer(body, proof))
			}))
			defer server.Close()
			base := cipheraddr.New(server.URL)
			signer := &fileSigner{key: key, enrollmentID: "enr-echo"}
			_, renewErr := newClient(base).RenewEnrollment(context.Background(), signer, "enr-echo", redacttest.Token)
			if renewErr == nil {
				t.Fatal("the renewal answered no error")
			}
			file := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(file, []byte(redacttest.Token), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			renewLoop(ctx, newClient(base), signer, "enr-echo", file, &stderr, func(time.Duration) <-chan time.Time {
				cancel()
				return nil
			})
			mu.Lock()
			sent := append([]string(nil), proofs...)
			mu.Unlock()
			if len(sent) == 0 || len(sent[0]) < 16 {
				t.Fatalf("the control: the stand-in saw no Proof header (%q)", sent)
			}
			for what, text := range map[string]string{
				"":              renewErr.Error(),
				"/pod-refusal":  podRefusal("agent-secrets enroll", "renew", podIdentity{}, base, renewErr),
				"/renew-stderr": stderr.String(),
			} {
				capture := "agent-secrets.answer-echo/" + c.name + what
				redacttest.RequireNone(t, capture, text)
				for _, proof := range sent {
					if strings.Contains(text, proof) {
						t.Errorf("REDACT-ASSERT %s: the output prints the proof its request carried: %q", capture, text)
						break
					}
				}
			}
		})
	}
}

// TestARenewalRefusalPrintsNoCredentialItsPagesReferencesHide: a front door in the broker's place
// answers a renewal with an HTML page, no envelope, that writes a credential's URL with its
// separators as HTML character references, named, decimal and hexadecimal. The renewal's error,
// the --pod refusal line and renewLoop's stderr line print none of the credential: the rule decodes
// the references of a URL's separators before it reads a field (redact.Remote), so doStatus's page
// is read as a reader of the page sees it. Captures agent-secrets.page-references/<case>,
// /<case>/pod-refusal and /<case>/renew-stderr.
func TestARenewalRefusalPrintsNoCredentialItsPagesReferencesHide(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, page string }{
		{"named", "<p>Sign in again at https:&sol;&sol;" + redacttest.User + "&colon;" + redacttest.Password + "&commat;issuer.example&sol;path&quest;access_token&equals;" + redacttest.Token + "</p>"},
		{"decimal", "<p>Sign in again at https:&#47;&#47;" + redacttest.User + "&#58;" + redacttest.Password + "&#64;issuer.example&#47;path&#63;access_token&#61;" + redacttest.Token + "</p>"},
		{"hex", "<p>Sign in again at https:&#x2F;&#x2f;" + redacttest.User + "&#x3A;" + redacttest.Password + "&#x40;issuer.example&#x2F;path&#x3F;access_token&#x3D;" + redacttest.Token + "</p>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, c.page)
			}))
			defer server.Close()
			base := cipheraddr.New(server.URL)
			signer := &fileSigner{key: key, enrollmentID: "enr-page"}
			_, renewErr := newClient(base).RenewEnrollment(context.Background(), signer, "enr-page", "")
			if renewErr == nil {
				t.Fatal("the renewal answered no error")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			renewLoop(ctx, newClient(base), signer, "enr-page", "", &stderr, func(time.Duration) <-chan time.Time {
				cancel()
				return nil
			})
			for what, text := range map[string]string{
				"":              renewErr.Error(),
				"/pod-refusal":  podRefusal("agent-secrets enroll", "renew", podIdentity{}, base, renewErr),
				"/renew-stderr": stderr.String(),
			} {
				redacttest.RequireNone(t, "agent-secrets.page-references/"+c.name+what, text)
			}
		})
	}
}

// TestARenewalAnswerThatDoesNotDecodePrintsNoPodTokenItEchoes: an answer in the broker's place
// takes a pod's renewal, 200, and echoes the pod token its body carried into lease_expires_at,
// which RenewEnrollment decodes as a time. time.Time's error quotes the value; the renewal's error
// and renewLoop's stderr line name the member that did not decode and hold no part of the token:
// the client reads the answer through redact.ReadAnswer, whose Decode quotes no value. Captures
// agent-secrets.decode-echo and agent-secrets.decode-echo/renew-stderr.
func TestARenewalAnswerThatDoesNotDecodePrintsNoPodTokenItEchoes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"lease_expires_at":%q}`, "renewed with "+string(body))
	}))
	defer server.Close()
	base := cipheraddr.New(server.URL)
	signer := &fileSigner{key: key, enrollmentID: "enr-decode"}
	_, renewErr := newClient(base).RenewEnrollment(context.Background(), signer, "enr-decode", redacttest.Token)
	if renewErr == nil {
		t.Fatal("the renewal took an answer whose lease_expires_at is no time")
	}
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(redacttest.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	renewLoop(ctx, newClient(base), signer, "enr-decode", file, &stderr, func(time.Duration) <-chan time.Time {
		cancel()
		return nil
	})
	redacttest.RequireNone(t, "agent-secrets.decode-echo", renewErr.Error())
	redacttest.RequireNone(t, "agent-secrets.decode-echo/renew-stderr", stderr.String())
	if !strings.Contains(renewErr.Error(), "lease_expires_at") {
		t.Errorf("the renewal's error is %q, want it to name the member that did not decode", renewErr)
	}
}
