package onepassword

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// opVault is the vault id the op client's tests name, as testdata/op's outputs do: no real
// vault's.
const opVault = "vault0000000000000000000aa"

// withoutBase is item with no client record, so two Items compare by what they say.
func withoutBase(item Item) Item {
	item.base = nil
	return item
}

// fakeOp is the op the process tests put first on PATH: it writes its arguments, one a line, and
// its standard input under OP_FAKE_RECORD, then fails as op 2.41.1 fails for an item the vault does
// not hold when OP_FAKE_MISSING names one, and otherwise prints the file OP_FAKE_ANSWER names.
const fakeOp = `#!/bin/sh
printf '%s\n' "$@" > "$OP_FAKE_RECORD/args"
cat > "$OP_FAKE_RECORD/stdin"
if [ -n "$OP_FAKE_MISSING" ]; then
  echo "[ERROR] 2026/11/23 09:30:00 \"$OP_FAKE_MISSING\" isn't an item in the \"cipher-example\" vault. Specify the item with its UUID, name, or domain." >&2
  exit 1
fi
cat "$OP_FAKE_ANSWER"
`

// putFakeOpOnPath writes fakeOp as op into a directory of the test's own, puts that directory
// first on PATH, and answers the directory its records are written to.
func putFakeOpOnPath(t *testing.T, answer string) string {
	t.Helper()
	bin, record := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "op"), []byte(fakeOp), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(answer)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OP_FAKE_RECORD", record)
	t.Setenv("OP_FAKE_ANSWER", abs)
	t.Setenv("OP_FAKE_MISSING", "")
	return record
}

// recorded reads the arguments and standard input the fake op wrote under record.
func recorded(t *testing.T, record string) (args []string, stdin string) {
	t.Helper()
	a, err := os.ReadFile(filepath.Join(record, "args"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.ReadFile(filepath.Join(record, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(a), "\n"), "\n"), string(s)
}

// runProcess is a RunChild that runs the command, as the CLI's tracker does less its process
// group.
func runProcess(cmd *exec.Cmd) error { return cmd.Run() }

// TestOpParsesItsJSON: the outputs of op 2.41.1 under testdata/op - `op item list` and `op item
// get` of two items, one archived and one whose cipher section the 1Password app made with an id
// of its own - parse to the Overviews and Items they hold, through op run as a process with the
// commands and standard input the client gives it.
func TestOpParsesItsJSON(t *testing.T) {
	client := NewOpClient(opVault, runProcess)
	ctx := context.Background()

	record := putFakeOpOnPath(t, filepath.Join("testdata", "op", "item-list.json"))
	overviews, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantOverviews := []Overview{
		{ID: "item000000000000000000000a", Title: "cipher-deel-api-key", State: StateActive, UpdatedAt: time.Date(2026, 11, 21, 9, 30, 0, 250_000_000, time.UTC)},
		{ID: "item000000000000000000000b", Title: "cipher-gone-key", State: StateArchived, UpdatedAt: time.Date(2026, 11, 22, 17, 45, 10, 0, time.UTC)},
	}
	if !reflect.DeepEqual(overviews, wantOverviews) {
		t.Errorf("List:\n got %+v\nwant %+v", overviews, wantOverviews)
	}
	if args, stdin := recorded(t, record); !slices.Equal(args, []string{"item", "list", "--vault", opVault, "--include-archive", "--format", "json"}) || stdin != "" {
		t.Errorf("List ran op %q with standard input %q", args, stdin)
	}

	record = putFakeOpOnPath(t, filepath.Join("testdata", "op", "item-get.json"))
	items, err := client.Get(ctx, "item000000000000000000000a", "item000000000000000000000b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wantItems := []Item{
		{
			ID: "item000000000000000000000a", Title: "cipher-deel-api-key", State: StateActive, Version: 3,
			CreatedAt: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 21, 9, 30, 0, 250_000_000, time.UTC),
			Credential: "deel-value", Cipher: map[string]string{"owner": "ada@example.com", "tier": "agent", "type": "github-app"},
		},
		{
			ID: "item000000000000000000000b", Title: "cipher-gone-key", State: StateArchived, Version: 2,
			CreatedAt: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 22, 17, 45, 10, 0, time.UTC),
			Cipher: map[string]string{"owner": "grace@example.com"},
		},
	}
	for i := range items {
		items[i] = withoutBase(items[i])
	}
	if !reflect.DeepEqual(items, wantItems) {
		t.Errorf("Get:\n got %+v\nwant %+v", items, wantItems)
	}
	args, stdin := recorded(t, record)
	if !slices.Equal(args, []string{"item", "get", "-", "--vault", opVault, "--reveal", "--include-archive", "--format", "json"}) {
		t.Errorf("Get ran op %q", args)
	}
	var asked []map[string]string
	if err := json.Unmarshal([]byte(stdin), &asked); err != nil || !reflect.DeepEqual(asked, []map[string]string{{"id": "item000000000000000000000a"}, {"id": "item000000000000000000000b"}}) {
		t.Errorf("Get gave op standard input %q (%v), want the two ids as a JSON list", stdin, err)
	}
}

// TestOpGetOfAMissingItemIsNotFound: op's refusal of an id the vault does not hold is
// secretstore.ErrNotFound.
func TestOpGetOfAMissingItemIsNotFound(t *testing.T) {
	putFakeOpOnPath(t, filepath.Join("testdata", "op", "item-get.json"))
	missing := "item000000000000000000000z"
	t.Setenv("OP_FAKE_MISSING", missing)
	_, err := NewOpClient(opVault, runProcess).Get(context.Background(), missing)
	if !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get(%s) = %v, want ErrNotFound", missing, err)
	}
}

// opCall is one command a recording RunChild was handed: its arguments after op's path, and what
// it read on its standard input.
type opCall struct {
	args  []string
	stdin string
}

// recordingRun is a RunChild that runs nothing: it records each command and prints answer on its
// standard output.
func recordingRun(calls *[]opCall, answer []byte) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		var stdin []byte
		if cmd.Stdin != nil {
			var err error
			if stdin, err = io.ReadAll(cmd.Stdin); err != nil {
				return err
			}
		}
		*calls = append(*calls, opCall{args: cmd.Args[1:], stdin: string(stdin)})
		_, err := cmd.Stdout.Write(answer)
		return err
	}
}

// firstOpItem is testdata/op/item-get.json's first item, as op prints it.
func firstOpItem(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "op", "item-get.json"))
	if err != nil {
		t.Fatal(err)
	}
	var first json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&first); err != nil {
		t.Fatal(err)
	}
	return first
}

// TestOpCommandsCarryNoValue: the op client's create and put give op the credential and every
// cipher field on its standard input, as an item template, and none of them among its
// arguments, which other processes of the machine can read; and each command is the one the
// client documents.
func TestOpCommandsCarryNoValue(t *testing.T) {
	putFakeOpOnPath(t, os.DevNull)
	var calls []opCall
	client := NewOpClient(opVault, recordingRun(&calls, firstOpItem(t)))
	ctx := context.Background()
	item := Item{
		ID: "item000000000000000000000a", Title: "cipher-deel-api-key", Version: 3, Credential: "s3cret-credential-value",
		Cipher: map[string]string{"owner": "ada@example.com", "tier": "agent", "requesters": "grace@example.com", CreateField: "0123456789abcdef0123456789abcdef"},
	}
	if _, err := client.Create(ctx, item); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := client.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := client.Archive(ctx, item.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	want := [][]string{
		{"item", "create", "--vault", opVault, "--format", "json", "-"},
		{"item", "edit", item.ID, "--vault", opVault, "--format", "json"},
		{"item", "delete", item.ID, "--vault", opVault, "--archive"},
	}
	if len(calls) != len(want) {
		t.Fatalf("op ran %d times, want %d: %+v", len(calls), len(want), calls)
	}
	values := []string{item.Credential}
	for _, v := range item.Cipher {
		values = append(values, v)
	}
	for i, call := range calls {
		if !slices.Equal(call.args, want[i]) {
			t.Errorf("command %d is op %q, want op %q", i, call.args, want[i])
		}
		for _, arg := range call.args {
			for _, v := range values {
				if strings.Contains(arg, v) {
					t.Errorf("op %q carries the value %q among its arguments", call.args, v)
				}
			}
		}
		if i == 2 {
			continue
		}
		for _, v := range values {
			if !strings.Contains(call.stdin, v) {
				t.Errorf("op %q read no %q on its standard input: %s", call.args, v, call.stdin)
			}
		}
	}
}

// TestOpPutKeepsTheFieldsCipherDoesNotOwn: a Put of an item op read writes op's own JSON of it
// back with the credential and the cipher fields changed - a tag's field set, a removed tag's
// field dropped, a new tag's field added in the cipher section - and every other key and field as
// op answered it.
func TestOpPutKeepsTheFieldsCipherDoesNotOwn(t *testing.T) {
	putFakeOpOnPath(t, os.DevNull)
	var calls []opCall
	answer := firstOpItem(t)
	client := NewOpClient(opVault, recordingRun(&calls, answer))
	ctx := context.Background()
	items, err := client.Get(ctx, "item000000000000000000000a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	item := items[0]
	item.Credential = "new-value"
	item.Cipher = map[string]string{"owner": "grace@example.com", "tier": "agent", "requesters": "ada@example.com"}
	if _, err := client.Put(ctx, item); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var sent, read map[string]json.RawMessage
	if err := json.Unmarshal([]byte(calls[1].stdin), &sent); err != nil {
		t.Fatalf("the Put's standard input is no JSON object: %v\n%s", err, calls[1].stdin)
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatal(err)
	}
	for key, value := range read {
		if key == "fields" {
			continue
		}
		if !bytes.Equal(compact(t, sent[key]), compact(t, value)) {
			t.Errorf("the Put changed key %s: %s, op answered %s", key, sent[key], value)
		}
	}
	var fields []struct {
		ID      string `json:"id"`
		Section *struct {
			ID string `json:"id"`
		} `json:"section"`
		Type  string `json:"type"`
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(sent["fields"], &fields); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range fields {
		section := ""
		if f.Section != nil {
			section = f.Section.ID
		}
		got[section+"/"+f.ID+"/"+f.Type+"/"+f.Label] = f.Value
	}
	want := map[string]string{
		"/username/STRING/username":                  "deploy-bot",
		"/credential/CONCEALED/credential":           "new-value",
		"/type/MENU/type":                            "bearer",
		"cipher/cipher-owner/STRING/owner":           "grace@example.com",
		"cipher/cipher-tier/STRING/tier":             "agent",
		"cipher/cipher-requesters/STRING/requesters": "ada@example.com",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the Put's fields:\n got %v\nwant %v", got, want)
	}
}

// compact is raw with no insignificant space, so two encodings of one value compare equal.
func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var out bytes.Buffer
	if len(raw) == 0 {
		return nil
	}
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
