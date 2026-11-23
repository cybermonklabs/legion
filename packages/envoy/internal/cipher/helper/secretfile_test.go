// packages/envoy/internal/cipher/helper/secretfile_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// wantNamedNotPrinted fails t unless text names each of named and holds none of secrets.
func wantNamedNotPrinted(t *testing.T, what, text string, named []string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds %q: %s", what, secret, text)
		}
	}
	for _, name := range named {
		if !strings.Contains(text, name) {
			t.Errorf("%s does not name %q: %s", what, name, text)
		}
	}
}

// underAFile is a path whose parent directory is a regular file, so every call on it fails
// "not a directory".
func underAFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, name)
}

// TestTheMachineFileIsNamedBySettingNeverByPath: AGENT_SECRETS_MACHINE_FILE is where a person who
// pasted a secret in place of a path put it, so no error about the machine file holds its path,
// and none holds the parser's words about its content either (a time error quotes the value it
// could not read). Each names the setting instead, and still says what went wrong and the fix.
func TestTheMachineFileIsNamedBySettingNeverByPath(t *testing.T) {
	const secret = "hunter2pw-mfile-Q7x3"
	const expirySecret = "tokexp-9Zq4w"
	dir := filepath.Join(t.TempDir(), secret)
	good := testMachineFile(t, "cred-1", "refresh-1")
	path := filepath.Join(dir, "machine.json")
	if err := good.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	named := []string{"AGENT_SECRETS_MACHINE_FILE"}

	t.Run("a file others can read", func(t *testing.T) {
		mode := filepath.Join(dir, "mode.json")
		if err := os.WriteFile(mode, data, 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := LoadMachineFile(mode)
		if err == nil {
			t.Fatal("LoadMachineFile of a 0640 file = nil")
		}
		wantNamedNotPrinted(t, "the refusal", err.Error(), append(named, "chmod 600", "0640"), secret)
	})

	bodies := map[string]string{
		"not JSON":                 "{",
		"trailing data":            string(data) + "garbage",
		"a key that is not PKCS#8": strings.Replace(string(data), `"key":"`, `"key":"AAAA`, 1),
		"no refresh token":         strings.Replace(string(data), `"refresh-1"`, `""`, 1),
	}
	fields["expires_at"] = expirySecret
	expiry, _ := json.Marshal(fields)
	bodies["an expiry that is a secret"] = string(expiry)
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadMachineFile(bad)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("LoadMachineFile = %v; want a refusal", err)
			}
			wantNamedNotPrinted(t, "the refusal", err.Error(), named, secret, expirySecret)
		})
	}

	t.Run("a directory that is a file", func(t *testing.T) {
		under := underAFile(t, filepath.Join(t.TempDir(), secret), "machine.json")
		_, err := LoadMachineFile(under)
		if err == nil {
			t.Fatal("LoadMachineFile under a file = nil")
		}
		wantNamedNotPrinted(t, "the read error", err.Error(), append(named, "not a directory"), secret)
		err = good.Save(under)
		if err == nil {
			t.Fatal("Save under a file = nil")
		}
		wantNamedNotPrinted(t, "the save error", err.Error(), append(named, "not a directory"), secret)
	})

	t.Run("a directory nothing can be removed from", func(t *testing.T) {
		locked := filepath.Join(t.TempDir(), secret)
		lockedFile := filepath.Join(locked, "machine.json")
		if err := good.Save(lockedFile); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, ".machine-1.json.tmp"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		unwritable(t, lockedFile)
		err := RemoveMachineFile(lockedFile)
		if err == nil {
			t.Fatal("RemoveMachineFile in a read-only directory = nil")
		}
		wantNamedNotPrinted(t, "the remove error", err.Error(), append(named, "permission denied"), secret)
		err = RemoveLeftoverMachineTemps(lockedFile)
		if err == nil {
			t.Fatal("RemoveLeftoverMachineTemps in a read-only directory = nil")
		}
		wantNamedNotPrinted(t, "the temp cleanup error", err.Error(), append(named, "permission denied"), secret)
	})

	t.Run("a start on a file that does not parse", func(t *testing.T) {
		bad := filepath.Join(dir, "start.json")
		if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		b := &Broker{URL: cipheraddr.New("http://127.0.0.1:1"), MachineFile: bad}
		t.Cleanup(b.Close)
		err := b.Start(context.Background())
		if err == nil {
			t.Fatal("Start on an unreadable machine file = nil")
		}
		wantNamedNotPrinted(t, "the start error", err.Error(), named, secret)
	})

	// The control: a file that is not there is still os.ErrNotExist, and a whole login still
	// loads, from the same directory.
	if _, err := LoadMachineFile(filepath.Join(dir, "absent.json")); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("LoadMachineFile of a file that is not there = %v; want os.ErrNotExist, saying so", err)
	}
	if loaded, err := LoadMachineFile(path); err != nil || loaded.CredentialID != "cred-1" {
		t.Errorf("LoadMachineFile of a whole login = %+v, %v", loaded, err)
	}
}

// TestTheBrokersMachineFileLogLinesNameTheSettingNeverThePath: the lines the helper logs about
// its machine file (the resume, a save it could not make, a file it could not remove, temporary
// files it could not clean up) name the file by AGENT_SECRETS_MACHINE_FILE, and neither their
// machine_file field nor their error holds the path. A login resumed from a directory the helper
// can read but not write fails each of those, and each line still says why.
func TestTheBrokersMachineFileLogLinesNameTheSettingNeverThePath(t *testing.T) {
	const secret = "hunter2pw-mlog-R4c9"
	f := newFakeBroker(t)
	dir := filepath.Join(t.TempDir(), secret)
	path := filepath.Join(dir, "machine.json")
	first := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: path, HTTP: f.srv.Client()}
	t.Cleanup(first.Close)
	if _, err := first.Login(context.Background(), "helper-host"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return first.LoginStatus().State == "issued" })
	if err := os.WriteFile(filepath.Join(dir, ".machine-1.json.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unwritable(t, path)

	var out syncBuffer
	resumed := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: first.OperatorFile, MachineFile: path, HTTP: f.srv.Client(),
		Log: slog.New(slog.NewJSONHandler(&out, nil)), refreshRetry: time.Hour}
	t.Cleanup(resumed.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := resumed.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stuck := waitForRecord(t, &out, unsavedStuckMsg)
	cleanup := waitForRecord(t, &out, "removing the temporary files an interrupted machine file save left")
	resume := waitForRecord(t, &out, "the helper resumed the machine login it kept")
	for what, rec := range map[string]map[string]any{"the stuck save": stuck, "the temp cleanup": cleanup, "the resume": resume} {
		line, _ := json.Marshal(rec)
		wantNamedNotPrinted(t, what, string(line), []string{"AGENT_SECRETS_MACHINE_FILE"}, secret)
		if field, _ := rec["machine_file"].(string); !strings.Contains(field, "AGENT_SECRETS_MACHINE_FILE") {
			t.Errorf("%s's machine_file is %q; want the setting that names it", what, field)
		}
	}
	// The control: the errors still say what the system answered.
	for what, rec := range map[string]map[string]any{"the stuck save": stuck, "the temp cleanup": cleanup} {
		if text, _ := rec["error"].(string); !strings.Contains(text, "permission denied") {
			t.Errorf("%s's error %q does not say why", what, text)
		}
	}
	wantNamedNotPrinted(t, "the whole log", out.String(), nil, secret)
}

// TestTheOperatorFileIsNamedBySettingNeverByPath: a machine login reads the operator file for
// its login_hint, and an error reading it names AGENT_SECRETS_OPERATOR_FILE and the system's
// reason, never the path. That error is what the login op answers on the socket.
func TestTheOperatorFileIsNamedBySettingNeverByPath(t *testing.T) {
	const secret = "hunter2pw-opfile-K2m8"
	dir := filepath.Join(t.TempDir(), secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeBroker(t)
	for name, c := range map[string]struct{ file, reason string }{
		"a file that is not there": {filepath.Join(dir, "operator"), "no such file or directory"},
		"an empty file":            {empty, "is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: c.file, MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
			t.Cleanup(b.Close)
			_, err := b.Login(context.Background(), "helper-host")
			if err == nil {
				t.Fatal("Login = nil")
			}
			wantNamedNotPrinted(t, "the login error", err.Error(), []string{"AGENT_SECRETS_OPERATOR_FILE", c.reason}, secret)
		})
	}
	// The control: an operator file with a login in it, in the same directory, lets the login start.
	b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: filepath.Join(dir, "operator"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
	t.Cleanup(b.Close)
	if err := os.WriteFile(b.OperatorFile, []byte("ada@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, err := b.Login(context.Background(), "helper-host"); err != nil || code == "" {
		t.Fatalf("Login with an operator file = %q, %v", code, err)
	}
}

// TestTheIdentityKeyIsNamedNeverByPath: the helper's identity key lives under HOME, and no
// error about it holds its path, nor the key parser's words: each names the identity key and
// still says what went wrong and the fix.
func TestTheIdentityKeyIsNamedNeverByPath(t *testing.T) {
	const secret = "hunter2pw-idkey-P5r1"
	dir := filepath.Join(t.TempDir(), secret)
	path := filepath.Join(dir, "identity-key.pem")
	key, made, err := LoadOrMakeIdentityKey(path)
	if err != nil || !made || key == nil {
		t.Fatalf("the first LoadOrMakeIdentityKey = %v, %v, %v; want a key it made", key, made, err)
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	named := []string{"identity key"}
	write := func(t *testing.T, name string, data []byte, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, c := range map[string]struct {
		path   func(t *testing.T) string
		reason string
	}{
		"a key others can read": {func(t *testing.T) string { return write(t, "mode.pem", pemBytes, 0o644) }, "chmod 600"},
		"no PEM block":          {func(t *testing.T) string { return write(t, "nopem.pem", []byte("not a key"), 0o600) }, "PRIVATE KEY"},
		"a block that is no PKCS#8 key": {func(t *testing.T) string {
			return write(t, "der.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not der")}), 0o600)
		}, "remove it"},
		"a directory that is a file": {func(t *testing.T) string {
			return underAFile(t, filepath.Join(t.TempDir(), secret), "identity-key.pem")
		}, "not a directory"},
		"a directory it cannot write": {func(t *testing.T) string {
			locked := filepath.Join(t.TempDir(), secret)
			if err := os.MkdirAll(locked, 0o700); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(locked, "identity-key.pem")
			unwritable(t, p)
			return p
		}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := LoadOrMakeIdentityKey(c.path(t))
			if err == nil {
				t.Fatal("LoadOrMakeIdentityKey = nil")
			}
			wantNamedNotPrinted(t, "the error", err.Error(), append(named, c.reason), secret, "x509:", "asn1:")
		})
	}
	// The control: the key it kept loads back, unmade.
	again, made, err := LoadOrMakeIdentityKey(path)
	if err != nil || made || !again.Equal(key) {
		t.Fatalf("the second LoadOrMakeIdentityKey = made %v, %v; want the key the first kept", made, err)
	}
}

// TestTheSessionRecordsAreNamedBySettingNeverByPath: the records file holds named sessions'
// signing keys, so no error about it holds its path, and none holds the decoder's words about
// its content (a time error quotes the value it could not read). The read error Recover logs and
// the error Save returns, which the helper logs, name AGENT_SECRETS_STATE_FILE instead.
func TestTheSessionRecordsAreNamedBySettingNeverByPath(t *testing.T) {
	const secret = "hunter2pw-records-W3n6"
	const goneSecret = "tokgone-4Hs8"
	dir := filepath.Join(t.TempDir(), secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	named := []string{"AGENT_SECRETS_STATE_FILE"}

	bad := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(bad, []byte(`[{"pid":1,"runtime_id":"rt-1","gone_at":"`+goneSecret+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRecords(bad)
	if err == nil {
		t.Fatal("LoadRecords of a record whose gone_at is no time = nil")
	}
	wantNamedNotPrinted(t, "the decode error", err.Error(), named, secret, goneSecret)

	var out syncBuffer
	s := &Server{Registry: NewRegistry(bad), Broker: &Broker{URL: cipheraddr.New("http://127.0.0.1:1")},
		Hostname: "testhost", PeerOf: PeerOf, Log: slog.New(slog.NewJSONHandler(&out, nil)), MinRenew: time.Second}
	t.Cleanup(s.Broker.Close)
	s.Recover(context.Background())
	line := waitForRecord(t, &out, "reading recorded sessions")
	text, _ := json.Marshal(line)
	wantNamedNotPrinted(t, "Recover's log line", string(text), named, secret, goneSecret)

	under := underAFile(t, filepath.Join(t.TempDir(), secret), "sessions.json")
	_, err = LoadRecords(under)
	if err == nil {
		t.Fatal("LoadRecords under a file = nil")
	}
	wantNamedNotPrinted(t, "the read error", err.Error(), append(named, "not a directory"), secret)
	reg := NewRegistry(under)
	sess, err := newSession(4242, 99, "h:4242:99", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Add(sess)
	err = reg.Save()
	if err == nil {
		t.Fatal("Save under a file = nil")
	}
	wantNamedNotPrinted(t, "the save error", err.Error(), append(named, "not a directory"), secret)

	// The control: records saved in the same directory load back.
	good := filepath.Join(dir, "good.json")
	reg = NewRegistry(good)
	reg.Add(sess)
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	if recs, err := LoadRecords(good); err != nil || len(recs) != 1 || recs[0].RuntimeID != "h:4242:99" {
		t.Fatalf("LoadRecords of saved records = %+v, %v", recs, err)
	}
}

// TestARecordedKeyThatDoesNotParseIsFixedText: a record's key is the records file's content, so
// the error for one that does not decode is the helper's own words, not the base64 or x509
// parser's.
func TestARecordedKeyThatDoesNotParseIsFixedText(t *testing.T) {
	for name, encoded := range map[string]string{
		"not base64": "hunter2pw!not*base64",
		"not PKCS#8": base64.StdEncoding.EncodeToString([]byte("hunter2pw-der-not-pkcs8")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Record{RuntimeID: "rt-1", Key: encoded}.PrivateKey()
			if err == nil {
				t.Fatal("PrivateKey = nil")
			}
			wantNamedNotPrinted(t, "the key error", err.Error(), []string{"rt-1", "key"}, "hunter2pw", "illegal base64", "x509:", "asn1:")
		})
	}
	// The control: a recorded key reads back.
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := (Record{RuntimeID: "rt-1", Key: encoded}).PrivateKey(); err != nil || !back.Equal(key) {
		t.Fatalf("PrivateKey of a recorded key = %v", err)
	}
}

// TestABrokerAnswerTheHelperCannotDecodePrintsThroughWords: a 2xx answer whose body the helper
// cannot decode is the broker's text, and the decoder repeats it (a time error quotes the value
// it could not read). Beside an address that carries a credential, the broker, or whoever
// answers for it, can repeat that credential there, so the error prints the decoder's text as
// the address lets it (cipheraddr.Address.Words): withheld beside that address, whole beside a
// clean one, for the enrolment and the renew alike.
func TestABrokerAnswerTheHelperCannotDecodePrintsThroughWords(t *testing.T) {
	var lease string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"enrollment_id": "enr-1", "lease_expires_at": lease})
	}))
	t.Cleanup(srv.Close)
	credentialed := strings.Replace(srv.URL, "://", "://tokuser9f3:hunter2pw-lease-8Vt2@", 1)
	lease = "the lease of " + credentialed
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signIns := &stubSignIns{authority: &cipherclient.Authority{Key: key, CredentialID: "credential-1", Operator: "ada@example.com"}}
	calls := func(t *testing.T, address string) map[string]error {
		t.Helper()
		b := &Broker{URL: cipheraddr.New(address), HTTP: srv.Client(), SignIns: signIns}
		t.Cleanup(b.Close)
		sess, err := newSession(os.Getpid(), 1, "example-host:1234:77", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, enrollErr := b.EnrollIdentity(context.Background(), sess.identity())
		_, renewErr := b.renewIdentity(context.Background(), identity{EnrollmentID: "enr-1", Key: key})
		return map[string]error{"the enrolment": enrollErr, "the renew": renewErr}
	}

	t.Run("an address carrying a credential", func(t *testing.T) {
		for call, err := range calls(t, credentialed) {
			if err == nil {
				t.Fatalf("%s took an answer it cannot decode", call)
			}
			wantNamedNotPrinted(t, call+"'s error", err.Error(), []string{cipheraddr.WordsNotPrinted}, "tokuser9f3", "hunter2pw-lease-8Vt2", "the lease of")
		}
	})
	t.Run("a clean address", func(t *testing.T) {
		for call, err := range calls(t, srv.URL) {
			if err == nil {
				t.Fatalf("%s took an answer it cannot decode", call)
			}
			if !strings.Contains(err.Error(), "parsing time") || !strings.Contains(err.Error(), "the lease of") {
				t.Errorf("%s's error %q does not carry the decoder's words beside a clean address", call, err)
			}
		}
	})
}
