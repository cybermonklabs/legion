package onepassword

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// opClient is Client over the op CLI (1Password CLI 2), on one vault: the person's own op, which
// signs in as op does - with OP_SERVICE_ACCOUNT_TOKEN, or the account OP_ACCOUNT or op's own
// configuration names - since the SDK's sign-in through the 1Password app needs cgo, which no
// release build has. Each call is one op command with JSON output, run through run.
type opClient struct {
	vault string
	run   secretstore.RunChild
}

// NewOpClient is the client that runs the first op on PATH on vault, each command through run,
// which starts it and waits for it (the CLI's one tracker of its child processes). The commands:
//
//	op item list --vault <vault> --include-archive --format json
//	op item get - --vault <vault> --reveal --include-archive --format json   (the ids, a JSON list, on stdin)
//	op item create --vault <vault> --format json -                           (an item template on stdin)
//	op item edit <id> --vault <vault> --format json                          (the item's JSON on stdin)
//	op item delete <id> --vault <vault> --archive
//
// No value is ever an argument of op, which any process of the machine can read: a credential
// and every cipher field go on op's standard input, as an item JSON template (`op item create
// --help` and `op item edit --help`, op 2.41.1).
func NewOpClient(vault string, run secretstore.RunChild) Client {
	return opClient{vault: vault, run: run}
}

// opError is an op command that failed: the command, run's error (op's exit status), and what op
// wrote on its standard error. The error's text names the command and the status, never op's
// message, which can quote the template a write gave it.
type opError struct {
	command string
	err     error
	stderr  string
}

func (e *opError) Error() string { return e.command + ": " + e.err.Error() }

func (e *opError) Unwrap() error { return e.err }

// What op 2.41.1 writes on its standard error for an item the vault does not hold: `"<item>"
// isn't an item in the "<vault>" vault.` And, provisional until the live run on the test vault
// records op's own texts, the words of a refusal of the sign-in and of one at 1Password's request
// limit, matched case-insensitively.
const opMissingItem = "isn't an item"

var (
	opDeniedWords  = []string{"forbidden", "permission", "not allowed", "unauthorized"}
	opLimitedWords = []string{"rate limit", "too many requests"}
)

// op runs op with args (runOp) through the client's runner.
func (c opClient) op(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return runOp(ctx, c.run, stdin, args...)
}

// runOp runs op with args through run, stdin on its standard input (nothing, /dev/null, when nil:
// never this command's own standard input, which can be a secret's value on its way in), and
// answers what it wrote on its standard output. A failure is an opError, marked ErrDenied or
// ErrRateLimited where op's standard error says so (opDeniedWords, opLimitedWords), whose text
// names the command and op's exit status alone.
func runOp(ctx context.Context, run secretstore.RunChild, stdin []byte, args ...string) ([]byte, error) {
	path, err := exec.LookPath("op")
	if err != nil {
		return nil, errors.New("no op is on PATH: install the 1Password CLI, then sign in with it")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := run(cmd); err != nil {
		failed := &opError{command: opCommand(args), err: err, stderr: stderr.String()}
		said := strings.ToLower(failed.stderr)
		switch {
		case containsAny(said, opLimitedWords):
			return nil, fmt.Errorf("%w: %w", ErrRateLimited, failed)
		case containsAny(said, opDeniedWords):
			return nil, fmt.Errorf("%w: %w", ErrDenied, failed)
		}
		return nil, failed
	}
	return stdout.Bytes(), nil
}

// opCommand is the command args run, as an error names it: "op" and the words before the first
// flag or argument that names an item ("op item list", "op whoami").
func opCommand(args []string) string {
	words := []string{"op"}
	for i, a := range args {
		if strings.HasPrefix(a, "-") || i == 2 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// containsAny reports whether s holds one of words.
func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// opObject is one JSON object op answers, kept whole, so a write gives op back every key this
// package does not read as op answered it.
type opObject = map[string]json.RawMessage

// opItem is the keys of op's item JSON this package reads.
type opItem struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Version   uint32     `json:"version"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Sections  []opObject `json:"sections"`
	Fields    []opObject `json:"fields"`
}

// opState is the State of op's item state: no state, or ACTIVE, for an active item, and
// ARCHIVED for one in the Archive; any other is an error.
func opState(id, state string) (State, error) {
	switch state {
	case "", "ACTIVE":
		return StateActive, nil
	case "ARCHIVED":
		return StateArchived, nil
	}
	return "", fmt.Errorf("op answered item %s in state %q, which this build does not know", id, state)
}

// opString is the string at key of o, "" where it holds none.
func opString(o opObject, key string) string {
	var s string
	_ = json.Unmarshal(o[key], &s)
	return s
}

// opSectionID is the id of the section field o is in, "" for none.
func opSectionID(o opObject) string {
	var section struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(o["section"], &section)
	return section.ID
}

// opJSON is v as JSON. Every v this package gives it encodes: strings, a version number, and
// maps and lists of them or of JSON op answered, which its decoder has already read as JSON.
func opJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// opFields reads and makes op's fields.
var opFields = fieldOps[opObject]{
	isCredential: func(f opObject) bool { return opString(f, "id") == credentialField && opSectionID(f) == "" },
	sectionOf:    opSectionID,
	labelOf:      func(f opObject) string { return opString(f, "label") },
	valueOf:      func(f opObject) string { return opString(f, "value") },
	withValue: func(f opObject, value string) opObject {
		f = maps.Clone(f)
		f["value"] = opJSON(value)
		return f
	},
	newCredential: func(value string) opObject {
		return opObject{"id": opJSON(credentialField), "type": opJSON("CONCEALED"), "label": opJSON(credentialField), "value": opJSON(value)}
	},
	newCipher: func(section, label, value string) opObject {
		return opObject{
			"id": opJSON(cipherFieldPrefix + label), "section": opJSON(map[string]string{"id": section}),
			"type": opJSON("STRING"), "label": opJSON(label), "value": opJSON(value),
		}
	},
}

// opCipherSections is the ids of the sections labelled cipherSection.
func opCipherSections(sections []opObject) []string {
	var ids []string
	for _, s := range sections {
		if opString(s, "label") == cipherSection {
			ids = append(ids, opString(s, "id"))
		}
	}
	return ids
}

// opItemOf is the Item of raw, one item's JSON as op answers it, with raw as its base.
func opItemOf(raw json.RawMessage) (Item, error) {
	var item opItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return Item{}, fmt.Errorf("op answered an item this build cannot read: %w", err)
	}
	state, err := opState(item.ID, item.State)
	if err != nil {
		return Item{}, err
	}
	credential, cipher := readFields(item.Fields, opCipherSections(item.Sections), opFields)
	return Item{
		ID: item.ID, Title: item.Title, State: state, Version: item.Version, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		Credential: credential, Cipher: cipher, base: bytes.Clone(raw),
	}, nil
}

// opValues is every item op printed on out: op prints one JSON value an item, or a list of them.
func opValues(out []byte) ([]json.RawMessage, error) {
	var values []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var value json.RawMessage
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			return values, nil
		} else if err != nil {
			return nil, fmt.Errorf("op printed output this build cannot read: %w", err)
		}
		if trimmed := bytes.TrimSpace(value); len(trimmed) > 0 && trimmed[0] == '[' {
			var list []json.RawMessage
			if err := json.Unmarshal(trimmed, &list); err != nil {
				return nil, fmt.Errorf("op printed output this build cannot read: %w", err)
			}
			values = append(values, list...)
			continue
		}
		values = append(values, value)
	}
}

// List runs op item list on the vault, archived items included.
func (c opClient) List(ctx context.Context) ([]Overview, error) {
	out, err := c.op(ctx, nil, "item", "list", "--vault", c.vault, "--include-archive", "--format", "json")
	if err != nil {
		return nil, err
	}
	values, err := opValues(out)
	if err != nil {
		return nil, err
	}
	overviews := make([]Overview, 0, len(values))
	for _, raw := range values {
		var o opItem
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, fmt.Errorf("op item list answered an item this build cannot read: %w", err)
		}
		state, err := opState(o.ID, o.State)
		if err != nil {
			return nil, err
		}
		overviews = append(overviews, Overview{ID: o.ID, Title: o.Title, State: state, UpdatedAt: o.UpdatedAt})
	}
	return overviews, nil
}

// Get runs one op item get for every id, given as a JSON list of {"id": <id>} on op's standard
// input, and answers the items in the order of ids.
func (c opClient) Get(ctx context.Context, ids ...string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	asked := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		asked = append(asked, map[string]string{"id": id})
	}
	out, err := c.op(ctx, opJSON(asked), "item", "get", "-", "--vault", c.vault, "--reveal", "--include-archive", "--format", "json")
	var failed *opError
	if errors.As(err, &failed) && strings.Contains(failed.stderr, opMissingItem) {
		return nil, fmt.Errorf("op item get found no item of %s in vault %s: %w", strings.Join(ids, ", "), c.vault, secretstore.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	values, err := opValues(out)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Item, len(values))
	for _, raw := range values {
		item, err := opItemOf(raw)
		if err != nil {
			return nil, err
		}
		byID[item.ID] = item
	}
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		item, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("op item get answered no item %s of vault %s: %w", id, c.vault, secretstore.ErrNotFound)
		}
		items = append(items, item)
	}
	return items, nil
}

// Create runs op item create with item as an API Credential template on op's standard input.
func (c opClient) Create(ctx context.Context, item Item) (Item, error) {
	template := opWrite(opObject{"category": opJSON("API_CREDENTIAL")}, item)
	out, err := c.op(ctx, opJSON(template), "item", "create", "--vault", c.vault, "--format", "json", "-")
	if err != nil {
		return Item{}, err
	}
	return opItemOf(out)
}

// Put runs op item edit with item written onto the JSON op answered for it (opWrite) on op's
// standard input: op takes an item's own JSON as the template of its edit.
func (c opClient) Put(ctx context.Context, item Item) (Item, error) {
	var top opObject
	if raw, ok := item.base.([]byte); ok {
		if err := json.Unmarshal(raw, &top); err != nil {
			return Item{}, fmt.Errorf("the item op answered is no JSON object: %w", err)
		}
	} else {
		top = opObject{"id": opJSON(item.ID), "category": opJSON("API_CREDENTIAL")}
	}
	top["version"] = opJSON(item.Version)
	out, err := c.op(ctx, opJSON(opWrite(top, item)), "item", "edit", item.ID, "--vault", c.vault, "--format", "json")
	if err != nil {
		return Item{}, err
	}
	return opItemOf(out)
}

// Archive runs op item delete --archive on the item.
func (c opClient) Archive(ctx context.Context, id string) error {
	_, err := c.op(ctx, nil, "item", "delete", id, "--vault", c.vault, "--archive")
	return err
}

// opWrite is top, an item's JSON, with item's title, and its credential and cipher fields written
// over top's own (mergeFields), a section labelled cipher added where item has cipher fields and
// top none.
func opWrite(top opObject, item Item) opObject {
	top = maps.Clone(top)
	var sections, fields []opObject
	_ = json.Unmarshal(top["sections"], &sections)
	_ = json.Unmarshal(top["fields"], &fields)
	ids := opCipherSections(sections)
	if len(ids) == 0 && len(item.Cipher) > 0 {
		taken := make([]string, 0, len(sections))
		for _, s := range sections {
			taken = append(taken, opString(s, "id"))
		}
		id := newSectionID(taken)
		sections = append(sections, opObject{"id": opJSON(id), "label": opJSON(cipherSection)})
		ids = []string{id}
	}
	section := ""
	if len(ids) > 0 {
		section = ids[0]
	}
	top["title"] = opJSON(item.Title)
	if len(sections) > 0 {
		top["sections"] = opJSON(sections)
	}
	top["fields"] = opJSON(mergeFields(fields, ids, section, item, opFields))
	return top
}
