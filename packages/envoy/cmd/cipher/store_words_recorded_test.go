// packages/envoy/cmd/cipher/store_words_recorded_test.go
//
// The three texts of this CLI that name the secret stores - the --profile refusal, the store rows
// of the usage's environment table, and the source a line names for each settings body - held to
// what main printed before the stores moved into packages of their own. An AWS or a GCP deployment
// upgrades with no change to what its people and scripts read. testdata/recorded-on-main/ was
// written by this file at main 67bcd7c78393, in a worktree of that commit, from the root of the
// checkout that holds this file:
//
//	git worktree add --detach <tmp>/main 67bcd7c78393
//	cp packages/envoy/cmd/cipher/store_words_recorded_test.go <tmp>/main/packages/envoy/cmd/cipher/
//	go test -C <tmp>/main/packages/envoy ./cmd/cipher/ -run RecordedOnMain -args -record
//	cp <tmp>/main/packages/envoy/cmd/cipher/testdata/recorded-on-main/* packages/envoy/cmd/cipher/testdata/recorded-on-main/
//	git worktree remove --force <tmp>/main
//
// Never write the records again from the code they check: a record written by the code it checks
// compares that code with itself and proves nothing. A change to main that adds, drops or changes
// one of these texts for the AWS or the GCP store fails this test in its own pull request: that
// change edits the record there, in the form -record writes, which the test holds source-words.json
// to, and names itself here with the lines of the records that moved; it changes what a
// deployment's CLI prints, so that pull request's review decides whether it may. The usage record
// holds every row from AWS_PROFILE to the blank line above "exit codes:", and the refusal record the
// whole refusal, so a later store that adds its own row or clause changes them too: that change may
// write those two again with -record, and its review must then find, in the records' diff, only that
// store's lines added and every AWS and GCP line as it was.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
)

var recordStoreWords = flag.Bool("record", false, "write testdata/recorded-on-main/ instead of comparing; run only on main, as store_words_recorded_test.go's head comment says")

// recordedOnMain is the directory of this file's records.
const recordedOnMain = "testdata/recorded-on-main"

// writeRecord writes data to the record name under recordedOnMain.
func writeRecord(t *testing.T, name string, data []byte) {
	t.Helper()
	path := filepath.Join(recordedOnMain, name)
	if err := os.MkdirAll(recordedOnMain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes to %s", len(data), path)
}

// readRecord answers the record name under recordedOnMain, failing t when it cannot be read or
// holds nothing, which would compare nothing.
func readRecord(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(recordedOnMain, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatalf("UNKNOWN: %s is empty, so the check compares nothing", path)
	}
	return data
}

// firstDifference says where got first differs from want, the record, so a failure names what
// moved before it prints both texts whole: the line, from 1, the byte of that line where they part,
// and each side's line from 20 bytes before that byte to 40 after it, or "absent" for a line one
// side does not have.
func firstDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := "", ""
		gotHas, wantHas := i < len(gotLines), i < len(wantLines)
		if gotHas {
			g = gotLines[i]
		}
		if wantHas {
			w = wantLines[i]
		}
		if gotHas == wantHas && g == w {
			continue
		}
		at := 0
		for at < len(g) && at < len(w) && g[at] == w[at] {
			at++
		}
		from := max(at-20, 0)
		side := func(line string, has bool) string {
			if !has {
				return "absent"
			}
			to := min(at+40, len(line))
			shown := fmt.Sprintf("%q", line[from:to])
			if from > 0 {
				shown = "…" + shown
			}
			if to < len(line) {
				shown += "…"
			}
			return shown
		}
		return fmt.Sprintf("line %d differs from byte %d: %s, recorded on main %s", i+1, at, side(g, gotHas), side(w, wantHas))
	}
	return "no line differs"
}

// TestProfileRefusalRecordedOnMain: a secret form given --profile exits 2 and prints, byte for
// byte, the refusal main printed, which names the variables each store's own tools read.
func TestProfileRefusalRecordedOnMain(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at store_words_recorded_test.go:120); it signals the test process or changes its signal handling (main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it sets a package variable the package's other tests read (clientConfig set at store_words_recorded_test.go:121).
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { clientConfig = nil })
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list", "--profile", "x"}, &stdout, &stderr)
	// 2 is the code main exits with here, written as a number: a change to exitUsageError moves
	// the exit code, and this test fails on it.
	if code != 2 {
		t.Fatalf("secret list --profile x exited %d (stderr %q), want 2", code, stderr.String())
	}
	if *recordStoreWords {
		writeRecord(t, "profile-refusal.txt", stderr.Bytes())
		return
	}
	if want := readRecord(t, "profile-refusal.txt"); !bytes.Equal(stderr.Bytes(), want) {
		t.Errorf("secret list --profile x printed a refusal other than main's: %s\nprinted\n%q\nrecorded on main\n%q", firstDifference(stderr.String(), string(want)), stderr.String(), want)
	}
}

// usageStoreRows is the store rows of usage()'s environment table: the lines from the one that
// starts "  AWS_PROFILE " through the last line before the blank line above "exit codes:", each
// ending in a newline.
func usageStoreRows(t *testing.T) string {
	t.Helper()
	lines := strings.Split(usage(), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  AWS_PROFILE ") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("UNKNOWN: no store rows found in usage()")
	}
	end := -1
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "exit codes:") {
			end = i - 1
			break
		}
	}
	if end <= start || lines[end] != "" {
		t.Fatal("UNKNOWN: no blank line above exit codes: after the store rows of usage()")
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}

// TestUsageStoreRowsRecordedOnMain: the usage's environment table prints the store rows main
// printed, byte for byte.
func TestUsageStoreRowsRecordedOnMain(t *testing.T) {
	t.Parallel()
	rows := usageStoreRows(t)
	if *recordStoreWords {
		writeRecord(t, "usage-store-rows.txt", []byte(rows))
		return
	}
	if want := string(readRecord(t, "usage-store-rows.txt")); rows != want {
		t.Errorf("usage()'s store rows are not main's: %s\nprinted\n%s\nrecorded on main\n%s", firstDifference(rows, want), rows, want)
	}
}

// sourceWords is one settings body's source as a record holds it: the body, whether the address
// that answered it prints its fields (Settings.words), and the source's String and storeName.
type sourceWords struct {
	Row       string          `json:"row"`
	Settings  json.RawMessage `json:"settings"`
	Clean     bool            `json:"clean"`
	String    string          `json:"string"`
	StoreName string          `json:"store_name"`
}

// sourceWordsBodies are the GET /v1/settings bodies whose source the record holds: each AWS case of
// account and region, a broker from before the store field, GCP with and without a project, and a
// store this release does not know.
var sourceWordsBodies = []struct{ row, body string }{
	{"aws with account and region", `{"store":"aws-secrets-manager","secrets_prefix":"example/agent-secrets/","kms_key_arn":"arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab","aws_account_id":"111122223333","aws_region":"us-east-1","secret_type_tag":true}`},
	{"aws with account alone", `{"store":"aws-secrets-manager","secrets_prefix":"example/agent-secrets/","aws_account_id":"111122223333","secret_type_tag":true}`},
	{"aws with region alone", `{"store":"aws-secrets-manager","secrets_prefix":"example/agent-secrets/","aws_region":"us-east-1","secret_type_tag":true}`},
	{"aws with neither", `{"store":"aws-secrets-manager","secrets_prefix":"example/agent-secrets/","secret_type_tag":true}`},
	{"no store, from a broker before the field", `{"secrets_prefix":"example/agent-secrets/","kms_key_arn":"arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab","aws_account_id":"111122223333","aws_region":"us-east-1"}`},
	{"gcp with a project", `{"store":"gcp-secret-manager","secrets_prefix":"projects/000000000000/secrets/agent-secrets-","kms_key_name":"projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets","gcp_project":"000000000000","secret_type_tag":true}`},
	{"gcp with no project", `{"store":"gcp-secret-manager","secrets_prefix":"projects/000000000000/secrets/agent-secrets-","kms_key_name":"projects/example/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets","secret_type_tag":true}`},
	{"a store this release does not know", `{"store":"example-store","secrets_prefix":"example/agent-secrets/","secret_type_tag":true}`},
}

// TestSourceWordsRecordedOnMain: for each settings body, as a clean address answered it and as
// one that prints no field of its answer (a URL that carries a credential), the source a session's
// lines print (String) and the store the secret forms name (storeName) are main's.
func TestSourceWordsRecordedOnMain(t *testing.T) {
	t.Parallel()
	addresses := map[bool]cipheraddr.Address{
		true:  cipheraddr.New("https://broker.example"),
		false: cipheraddr.New(userinfoURL("https", "demouser", "demopass-not-a-secret", "broker.example")),
	}
	var got []sourceWords
	for _, row := range sourceWordsBodies {
		for _, clean := range []bool{true, false} {
			address := addresses[clean]
			if address.Clean() != clean {
				t.Fatalf("the address for clean=%t reports Clean() %t", clean, address.Clean())
			}
			var settings Settings
			if err := json.Unmarshal([]byte(row.body), &settings); err != nil {
				t.Fatalf("%s: decode %s: %v", row.row, row.body, err)
			}
			settings.answeredBy = address
			src := brokerSource(address).withSettings(settings)
			got = append(got, sourceWords{Row: row.row, Settings: json.RawMessage(row.body), Clean: clean, String: src.String(), StoreName: src.storeName()})
		}
	}
	if *recordStoreWords {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeRecord(t, "source-words.json", append(raw, '\n'))
		return
	}
	raw := readRecord(t, "source-words.json")
	var want []sourceWords
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode source-words.json: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("UNKNOWN: source-words.json records no row, so the check compares nothing")
	}
	if again, err := json.MarshalIndent(want, "", "  "); err != nil || !bytes.Equal(append(again, '\n'), raw) {
		t.Fatal("source-words.json holds an entry or a form -record does not write (a field written by hand), which the comparison would not read; write it as -record does")
	}
	type key struct {
		row   string
		clean bool
	}
	read := map[key]bool{}
	for _, g := range got {
		read[key{g.Row, g.Clean}] = true
	}
	recorded := map[key]sourceWords{}
	for _, w := range want {
		k := key{w.Row, w.Clean}
		if _, twice := recorded[k]; twice {
			t.Errorf("%s (clean %t): source-words.json records this row more than once, and the comparison reads one of them", w.Row, w.Clean)
		}
		if !read[k] {
			t.Errorf("%s (clean %t): source-words.json records this row, and the test reads no such row", w.Row, w.Clean)
		}
		recorded[k] = w
	}
	for _, g := range got {
		w, ok := recorded[key{g.Row, g.Clean}]
		if !ok {
			t.Errorf("%s (clean %t): no row recorded on main", g.Row, g.Clean)
			continue
		}
		if !sameJSON(g.Settings, w.Settings) {
			t.Errorf("%s (clean %t): the test reads the body %s, recorded on main from %s", g.Row, g.Clean, g.Settings, w.Settings)
		}
		if g.String != w.String {
			t.Errorf("%s (clean %t): String() %q, recorded on main %q", g.Row, g.Clean, g.String, w.String)
		}
		if g.StoreName != w.StoreName {
			t.Errorf("%s (clean %t): storeName() %q, recorded on main %q", g.Row, g.Clean, g.StoreName, w.StoreName)
		}
	}
}

// sameJSON reports whether a and b are one JSON text but for the space between tokens.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}
