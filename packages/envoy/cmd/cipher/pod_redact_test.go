//go:build unix

// packages/envoy/cmd/cipher/pod_redact_test.go
//
// What `enroll --pod` and `renew --pod-token-file` print when a file that holds a credential, or
// a broker answer, cannot be read: the flag that names the file, never its path, since a flag
// meant for a credential's path is where a person who pasted the credential itself puts it; the
// system's reason, never the parser's words about the file's content; and a broker answer's
// decoder error as cipheraddr.Address.Words lets it print. Every secret here is made up.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestAPodTokenFileIsNamedByItsFlagNeverItsPath: a token pasted where --pod-token-file wants the
// token's file, and a file that is there but blank, are refused by enroll --pod and by renew
// naming --pod-token-file, and neither the pasted token nor the file's path is printed - nor, for
// enroll, recorded in enrollment.error. A file that holds a token enrols, and the renewal command
// the enrolment prints names that file.
func TestAPodTokenFileIsNamedByItsFlagNeverItsPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:55); it signals the test process or changes its signal handling (main.cmdRenew, signal.NotifyContext at main.go:858); it sets a package variable the package's other tests read (main.cmdRenew, main.renewContext, main.chainSessionContext, main.buildChainSigner, main.selectSigner, main.signerChain, main.machineChainSource, main.registerMachine, main.registerMachineSession, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	const pasted = "hunter2pw-podtoken-7f3c91"
	blankDir := filepath.Join(t.TempDir(), "hunter2pw-tokendir-0d4e55")
	if err := os.MkdirAll(blankDir, 0o700); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(blankDir, "token")
	if err := os.WriteFile(blank, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ name, path, secret string }{
		{"a token pasted in place of its file", pasted, pasted},
		{"a blank file", blank, "hunter2pw-tokendir-0d4e55"},
	} {
		t.Run("enroll: "+row.name, func(t *testing.T) {
			broker, calls := servePodBroker(t, "never-reached")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			out := t.TempDir()
			// A key from an earlier run, so that a refusal is recorded in enrollment.error too.
			if _, err := enrollSessionKey(out); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", row.path, "--out", out}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			recorded, err := os.ReadFile(filepath.Join(out, enrollmentErrorFile))
			if err != nil {
				t.Fatalf("read %s: %v", enrollmentErrorFile, err)
			}
			for what, text := range map[string]string{"stderr": stderr.String(), enrollmentErrorFile: string(recorded)} {
				wantNamedNotPrinted(t, what, text, []string{"--pod-token-file"}, row.secret)
			}
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
		})
		t.Run("renew: "+row.name, func(t *testing.T) {
			broker, calls := servePodBroker(t, testEnrollmentID)
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
			var stderr bytes.Buffer
			if code := cmdRenew([]string{"--pod-token-file", row.path}, io.Discard, &stderr); code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			wantNamedNotPrinted(t, "stderr", stderr.String(), []string{"--pod-token-file"}, row.secret)
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
		})
	}
	t.Run("control: a file that holds a token", func(t *testing.T) {
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		broker, _ := servePodBroker(t, "enrollment-of-the-pod")
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr.String())
		}
		if want := "agent-secrets renew --pod-token-file " + shellWord(tokenFile); !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name the renewal command %q", stderr.String(), want)
		}
	})
}

// TestKeyPemInTheOutDirectoryIsNamedByItsFlagNeverItsPath: a key.pem that enroll --pod cannot
// read, or whose content is no session key, is refused naming key.pem in the directory --out
// names, with the system's reason or fixed text and never the parser's words, and neither the
// directory's path nor the key's bytes are printed or recorded in enrollment.error. A key.pem
// that holds a key is still the key the enrolment keeps.
func TestKeyPemInTheOutDirectoryIsNamedByItsFlagNeverItsPath(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:152).
	const secret = "hunter2pw-outdir-3a9e42"
	leaked := []string{secret, "hunter2pw-keybytes-1e8f60", "x509:", "asn1:"}
	for _, row := range []struct {
		name  string
		write func(t *testing.T, key string)
		want  string
	}{
		{"key.pem is a directory", func(t *testing.T, key string) {
			if err := os.Mkdir(key, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "read: is a directory"},
		{"key.pem is not PEM", func(t *testing.T, key string) {
			if err := os.WriteFile(key, []byte("hunter2pw-keybytes-1e8f60\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a PEM file"},
		{"key.pem is no PKCS#8 key", func(t *testing.T, key string) {
			block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("hunter2pw-keybytes-1e8f60")})
			if err := os.WriteFile(key, block, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a PKCS#8 private key"},
		{"key.pem is a PKCS#8 key that is not ECDSA", func(t *testing.T, key string) {
			_, ed, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalPKCS8PrivateKey(ed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not an ECDSA private key"},
	} {
		t.Run(row.name, func(t *testing.T) {
			tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
			broker, _ := servePodBroker(t, "enrollment-of-the-pod")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			out := filepath.Join(t.TempDir(), secret)
			if err := os.MkdirAll(out, 0o700); err != nil {
				t.Fatal(err)
			}
			row.write(t, filepath.Join(out, keyFile))
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			recorded, err := os.ReadFile(filepath.Join(out, enrollmentErrorFile))
			if err != nil {
				t.Fatalf("read %s: %v", enrollmentErrorFile, err)
			}
			for what, text := range map[string]string{"stderr": stderr.String(), enrollmentErrorFile: string(recorded)} {
				wantNamedNotPrinted(t, what, text, []string{"key.pem in the directory --out", row.want}, leaked...)
			}
		})
	}
	t.Run("control: key.pem holds a key", func(t *testing.T) {
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		broker, _ := servePodBroker(t, "enrollment-of-the-pod")
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		out := filepath.Join(t.TempDir(), "keys")
		key, err := enrollSessionKey(out)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr.String())
		}
		again, err := enrollSessionKey(out)
		if err != nil || !again.Equal(key) {
			t.Fatalf("the enrolment did not keep the key.pem that was there (%v)", err)
		}
	})
}

// TestPodTokenClaimsThatCannotBeReadSayFixedText: a token whose claims are not base64url, or not
// JSON, is refused in fixed text naming --pod-token-file, never in the decoder's words about the
// token, and the token is not printed. A token whose claims name a pod still enrols.
func TestPodTokenClaimsThatCannotBeReadSayFixedText(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:207).
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	for _, row := range []struct {
		name, token, want, decoder string
	}{
		{"claims that are not base64url", header + ".hunter2pw!claims!5b1d07.c2ln", "not base64url", "illegal base64"},
		{"claims that are not JSON", header + "." + enc.EncodeToString([]byte(`{"sub": hunter2pw-claims-5b1d07}`)) + ".c2ln", "not JSON", "invalid character"},
	} {
		t.Run(row.name, func(t *testing.T) {
			tokenFile := writePodToken(t, row.token)
			broker, calls := servePodBroker(t, "never-reached")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			wantNamedNotPrinted(t, "stderr", stderr.String(), []string{"--pod-token-file", row.want}, row.token, "hunter2pw", row.decoder)
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
		})
	}
	t.Run("control: claims that name a pod", func(t *testing.T) {
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		broker, _ := servePodBroker(t, "enrollment-of-the-pod")
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr.String())
		}
	})
}

// TestAPodBrokerAnswerItCannotReadPrintsThroughWords: a token login or an enrolment the broker
// answers 2xx with a body the client cannot decode is refused with the decoder's error as
// cipheraddr.Address.Words lets it print: whole beside a clean AGENT_SECRETS_URL, and
// WordsNotPrinted beside one that carries a credential, where whoever answers can repeat that
// credential in any text of its answer. The credential itself is never printed or recorded.
func TestAPodBrokerAnswerItCannotReadPrintsThroughWords(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:264).
	const credential = "tokuser9f3:hunter2pw-broker-2c6d"
	for _, route := range []string{"/v1/launcher-credentials/token", "/v1/enrollments"} {
		for _, clean := range []bool{false, true} {
			name := route + " beside an address that carries a credential"
			if clean {
				name = route + " beside a clean address (control)"
			}
			t.Run(name, func(t *testing.T) {
				tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
				broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "application/json")
					answer := map[string]any{"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour)}
					if r.URL.Path == "/v1/enrollments" {
						w.WriteHeader(http.StatusCreated)
						answer = map[string]any{"enrollment_id": "enrollment-of-the-pod"}
					}
					if r.URL.Path == route {
						// A number where the answer's id is a string: a body this client cannot read.
						answer = map[string]any{"credential_id": 7, "enrollment_id": 7}
					}
					_ = json.NewEncoder(w).Encode(answer)
				}))
				defer broker.Close()
				address := broker.URL
				if !clean {
					address = strings.Replace(address, "://", "://"+credential+"@", 1)
				}
				t.Setenv("AGENT_SECRETS_URL", address)
				out := t.TempDir()
				var stdout, stderr bytes.Buffer
				if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 1 {
					t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
				}
				texts := map[string]string{"stderr": stderr.String()}
				if route == "/v1/enrollments" {
					recorded, err := os.ReadFile(filepath.Join(out, enrollmentErrorFile))
					if err != nil {
						t.Fatalf("read %s: %v", enrollmentErrorFile, err)
					}
					texts[enrollmentErrorFile] = string(recorded)
				}
				for what, text := range texts {
					// No decoder error of these answers can quote the address: both answers are
					// decoded into string fields alone, and encoding/json's error for them names
					// the JSON kind and the struct field (UnmarshalTypeError) or one character
					// (SyntaxError). So this check guards how the rest of the line names the broker
					// (podRefusal, refusalText), and the Words checks below guard the decoder's words.
					if strings.Contains(text, "hunter2pw-broker-2c6d") || strings.Contains(text, "tokuser9f3") {
						t.Errorf("%s %q holds the address's credential", what, text)
					}
					decoded := strings.Contains(text, "cannot unmarshal")
					withheld := strings.Contains(text, cipheraddr.WordsNotPrinted)
					if clean && (!decoded || withheld) {
						t.Errorf("%s %q: beside a clean address want the decoder's words, not %q", what, text, cipheraddr.WordsNotPrinted)
					}
					if !clean && (decoded || !withheld) {
						t.Errorf("%s %q: beside an address that is not clean want %q in place of the decoder's words", what, text, cipheraddr.WordsNotPrinted)
					}
				}
			})
		}
	}
}

// TestARefusalThatCannotBeRecordedNamesTheDirectoryByItsFlag: beside an earlier run's key.pem,
// enroll --pod records each refusal in enrollment.error too, and when that record cannot be
// written the line saying so names the directory by --out and the system's reason, never the
// directory's path. A directory that takes the record holds the refusal, with no such line: the
// control.
func TestARefusalThatCannotBeRecordedNamesTheDirectoryByItsFlag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:326).
	const secret = "hunter2pw-recorddir-5c2a71"
	for _, blocked := range []bool{true, false} {
		name := "a directory where the record goes"
		if !blocked {
			name = "control: a directory that takes the record"
		}
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), secret)
			// A key from an earlier run, so that a refusal is recorded in enrollment.error too.
			if _, err := enrollSessionKey(out); err != nil {
				t.Fatal(err)
			}
			if blocked {
				if err := os.Mkdir(filepath.Join(out, enrollmentErrorFile), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			broker, calls := servePodBroker(t, "never-reached")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			missing := filepath.Join(t.TempDir(), "no-token-here")
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", missing, "--out", out}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
			if blocked {
				// os.Rename refuses a directory where the file goes as EEXIST.
				wantNamedNotPrinted(t, "stderr", stderr.String(), []string{"record that refusal in " + enrollmentErrorFile + " in " + outDir, "rename: file exists"}, secret)
				return
			}
			recorded, err := os.ReadFile(filepath.Join(out, enrollmentErrorFile))
			if err != nil || !strings.Contains(string(recorded), "--pod-token-file") || strings.Contains(stderr.String(), "record that refusal") {
				t.Errorf("%s = %q (%v), stderr %q; want the refusal recorded and no line about recording it", enrollmentErrorFile, recorded, err, stderr.String())
			}
		})
	}
}

// TestAnEarlierRefusalThatCannotBeRemovedNamesTheDirectoryByItsFlag: once enroll --pod has
// written the enrollment id it removes an earlier run's enrollment.error, and when that removal
// fails the line saying so names the directory by --out and the system's reason, never the
// directory's path. An earlier refusal that can be removed is removed, with no such line: the
// control.
func TestAnEarlierRefusalThatCannotBeRemovedNamesTheDirectoryByItsFlag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_redact_test.go:379).
	const secret = "hunter2pw-removedir-8e6b14"
	for _, blocked := range []bool{true, false} {
		name := "a directory that is not empty where the earlier refusal is"
		if !blocked {
			name = "control: an earlier refusal that can be removed"
		}
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), secret)
			earlier := filepath.Join(out, enrollmentErrorFile)
			if blocked {
				// os.Remove refuses a directory that is not empty.
				if err := os.MkdirAll(filepath.Join(earlier, "inside"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(out, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(earlier, []byte("an earlier refusal\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
			broker, _ := servePodBroker(t, "enrollment-of-the-pod")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, stderr.String())
			}
			// The renewal command a successful enrolment prints names the directory by its path on
			// purpose (enrolPod), so only the removal's own line is read.
			removal := ""
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.Contains(line, "remove the earlier refusal") {
					removal = line
				}
			}
			if blocked {
				wantNamedNotPrinted(t, "the removal's line", removal, []string{"remove the earlier refusal in " + enrollmentErrorFile + " in " + outDir, "remove: directory not empty"}, secret)
				return
			}
			if _, err := os.Stat(earlier); !errors.Is(err, os.ErrNotExist) || removal != "" {
				t.Errorf("the earlier refusal after the enrolment: %v, stderr %q; want it removed and no line about removing it", err, stderr.String())
			}
		})
	}
}
