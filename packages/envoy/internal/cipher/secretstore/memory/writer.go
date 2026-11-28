package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// ErrDenied is the memory store's access denial: what a test's DenyNext answers, and what
// AccessDenied and DefiniteRefusal read.
var ErrDenied = errors.New("the memory store refused the call")

// errAnswerLost is what a call answers after LoseNext: it was applied, and its answer lost.
var errAnswerLost = errors.New("the answer to a call the memory store applied was lost")

// recoveryWindow is how long a deleted memory secret stays restorable before it may be gone for
// good: AWS Secrets Manager's shortest recovery window, 7 days, so a local run says what a
// deployment on AWS would.
const recoveryWindow = 7 * 24 * time.Hour

// readPermission is what reading a memory secret's tags or value needs.
const readPermission = "read access to the memory secrets file"

// Writer is the memory store's writer: the Store it opened over the memory secrets file, which it
// writes. Each write takes the file's lock, reads it, changes it, writes a whole new file beside
// it and renames that over it, so a reader never sees half a file. FailNext, Fail and Calls reach
// its calls too: create, set, tags, delete and restore.
type Writer struct {
	*Store
	// prefix is the agent secrets' prefix the broker's settings name, from which a printed command
	// names a secret as a session asks for it.
	prefix string
	// mu guards lose, deny and hang, the test hooks of the next calls.
	mu   sync.Mutex
	lose map[string]bool
	deny map[string]string
	hang map[string]bool
	// applied is Hooks.Applied as the writer was opened with it.
	applied func(call string)
}

// Hooks are what a test sees of the writers OpenWriter opens.
type Hooks struct {
	// Opened is handed each writer OpenWriter opens.
	Opened func(w *Writer)
	// Applied, when set, runs once a write of kind call ("create", "set", "tags", "delete" or
	// "restore") has changed the file, before the call answers, so a test can be the writer that
	// changes the secret between this one's write and its read-back.
	Applied func(call string)
}

var (
	hooksMu sync.Mutex
	hooks   Hooks
)

// SwapHooks makes h the hooks of every writer opened from now on and answers the function that
// puts the previous ones back.
func SwapHooks(h Hooks) (restore func()) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	previous := hooks
	hooks = h
	return func() {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		hooks = previous
	}
}

// Kind is the memory store as a line names its kind: its name, "memory", as the CLI's lines
// printed it for a store they did not know.
func (Backend) Kind() string { return "memory" }

// Place is "file <path>", the memory secrets file the settings name, "" for none.
func (Backend) Place(s secretstore.Settings) string {
	if path := s.Fields[settingsFile]; path != "" {
		return "file " + s.Words(path)
	}
	return ""
}

// HelpRows is none: the memory store reads no environment variable of the CLI's.
func (Backend) HelpRows() string { return "" }

// ProfileHint is none: the memory store signs in nobody, so it has no clause of the --profile
// refusal.
func (Backend) ProfileHint() (secretstore.ProfileHint, bool) { return secretstore.ProfileHint{}, false }

// settingsFile is the memory store's settings field naming its file.
const settingsFile = "memory_secrets_file"

// OpenWriter opens the writer over the file the broker's settings name (memory_secrets_file).
func (Backend) OpenWriter(_ context.Context, s secretstore.Settings, _ secretstore.WriterOptions) (secretstore.Writer, error) {
	path := s.Fields[settingsFile]
	if path == "" {
		return nil, errors.New("the broker's settings name no memory_secrets_file, so there is no memory store to write")
	}
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	w := &Writer{Store: store, prefix: s.Prefix, lose: map[string]bool{}, deny: map[string]string{}, hang: map[string]bool{}}
	hooksMu.Lock()
	opened := hooks.Opened
	w.applied = hooks.Applied
	hooksMu.Unlock()
	if opened != nil {
		opened(w)
	}
	return w, nil
}

// LoseNext makes the next call of kind call apply and then answer that its answer was lost.
func (w *Writer) LoseNext(call string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lose[call] = true
}

// DenyNext makes the next call of kind call refused as access denied, the service's own text
// being message, which no error this writer answers prints.
func (w *Writer) DenyNext(call, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deny[call] = message
}

// HangNext makes the next call of kind call answer nothing until its context ends, and then the
// context's error, having done nothing: a store call no store answers, as a network store's can
// be. The file store itself never hangs; this is how a test reaches the caller's deadline.
func (w *Writer) HangNext(call string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hang[call] = true
}

// takeHooks answers whether the next call of kind call is to be denied, with what, whether its
// answer is to be lost, and whether it hangs, clearing all three.
func (w *Writer) takeHooks(call string) (denied bool, message string, lost, hang bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	message, denied = w.deny[call]
	delete(w.deny, call)
	lost = w.lose[call]
	delete(w.lose, call)
	hang = w.hang[call]
	delete(w.hang, call)
	return denied, message, lost, hang
}

// start counts one call of kind call as the store counts its reads, answers the error a test set
// for it, refuses it when DenyNext asked, and waits for ctx to end when HangNext asked; lost
// reports whether its answer is to be lost.
func (w *Writer) start(ctx context.Context, call string) (lost bool, err error) {
	w.Store.mu.Lock()
	err = w.Store.begin(call)
	w.Store.mu.Unlock()
	if err != nil {
		return false, err
	}
	denied, message, lost, hang := w.takeHooks(call)
	if hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if denied {
		return false, secretstore.Failure{Call: call, Words: "the memory store refused the call", Err: fmt.Errorf("%w: %s", ErrDenied, message)}
	}
	return lost, nil
}

// didApply runs the Applied hook, where one is set, for a write of kind call that changed the file.
func (w *Writer) didApply(call string) {
	if w.applied != nil {
		w.applied(call)
	}
}

// lostAnswer is a call's answer once it was applied and its answer lost.
func lostAnswer(call string) error {
	return secretstore.Failure{Call: call, Words: "the memory store applied the call and its answer was lost", Err: errAnswerLost}
}

// edit changes the file under its lock: it reads the secrets, calls change with them, and, when
// change answers no error, writes them back whole, then has the store read the file again.
func (w *Writer) edit(change func(secrets []Secret) ([]Secret, error)) error {
	unlock, err := lockFile(w.path + ".lock")
	if err != nil {
		return fmt.Errorf("lock %s: %w", w.path, err)
	}
	defer unlock()
	data, err := os.ReadFile(w.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", w.path, err)
	}
	secrets, err := parseFile(w.path, data)
	if err != nil {
		return err
	}
	secrets, err = change(secrets)
	if err != nil {
		return err
	}
	if err := writeWhole(w.path, secrets); err != nil {
		return err
	}
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	return w.Store.follow()
}

// writeWhole writes secrets as path's whole new content: a file beside it, mode 0600, renamed over
// it, so a reader of path reads the old file or the new one and never part of either.
func writeWhole(path string, secrets []Secret) error {
	entries := make([]fileSecret, 0, len(secrets))
	for _, s := range secrets {
		entries = append(entries, fileSecret{Secret: s})
	}
	data, err := json.MarshalIndent(file{Secrets: entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// find is the index of the secret named id in secrets, -1 for none.
func find(secrets []Secret, id string) int {
	for i, s := range secrets {
		if s.Name == id {
			return i
		}
	}
	return -1
}

// tagEtag is tags as one comparable string, which a tag write is checked against.
func tagEtag(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%q=%q;", k, tags[k])
	}
	return b.String()
}

// Identity is "memory store: <file>".
func (w *Writer) Identity() string { return "memory store: " + w.path }

// ChooseAnother says how to write another memory store.
func (w *Writer) ChooseAnother() string {
	return "choose another by pointing BROKER_MEMORY_SECRETS_FILE at another file"
}

// RerunPrefix is none: a rerun writes the same file.
func (w *Writer) RerunPrefix() string { return "" }

// PersonEmail refuses: the memory store signs in nobody.
func (w *Writer) PersonEmail(context.Context) (string, error) {
	return "", errors.New("the memory store signs in no person")
}

// AccessDenied and DefiniteRefusal are a call the memory store refused (ErrDenied), which it
// refuses before acting.
func (w *Writer) AccessDenied(err error) bool    { return errors.Is(err, ErrDenied) }
func (w *Writer) DefiniteRefusal(err error) bool { return errors.Is(err, ErrDenied) }

// MissingPermission is none: a memory refusal names no permission.
func (w *Writer) MissingPermission(error) string { return "" }

func (w *Writer) TagReadPermission() string   { return readPermission }
func (w *Writer) ValueReadPermission() string { return readPermission }

// Close holds nothing open.
func (w *Writer) Close() error { return nil }

// PrepareCreate is the create of id with tags, which the memory store makes with its value.
func (w *Writer) PrepareCreate(id string, tags map[string]string) secretstore.Create {
	return &create{w: w, id: id, tags: maps.Clone(tags)}
}

// create is one memory create: it checks the name in Begin and makes the secret, value and all,
// in Store.
type create struct {
	w    *Writer
	id   string
	tags map[string]string
	// lost is set when Store's answer was lost, so Unsettled names what to check.
	lost bool
}

// heldName refuses a name the store holds, a secret scheduled for deletion included.
func heldName(id string) error {
	return secretstore.Failure{Call: "create", Words: "the memory store already holds a secret named " + id}
}

// Begin refuses a held name, before the value is read; it makes nothing.
func (c *create) Begin(context.Context) (secretstore.DiscardOutcome, error) {
	c.w.Store.mu.Lock()
	err := c.w.Store.follow()
	_, held := c.w.Store.secrets[c.id]
	c.w.Store.mu.Unlock()
	if err != nil {
		return secretstore.MadeNothing, err
	}
	if held {
		return secretstore.MadeNothing, heldName(c.id)
	}
	return secretstore.MadeNothing, nil
}

// Store makes the secret with value, unless another writer made one of its name meanwhile, under
// secretstore.CallDeadline as every store's create call is. A call that runs out of time may or
// may not have made the secret, which the error says, naming the command that tells which.
func (c *create) Store(ctx context.Context, value string) error {
	ctx, cancel := secretstore.WithCallDeadline(ctx)
	defer cancel()
	lost, err := c.w.start(ctx, "create")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w; the memory store did not answer within %s, so the store may hold %s, made with its value, or no %s at all; %s says which",
				err, secretstore.CallDeadlineText(), c.name(), c.name(), c.show())
		}
		return err
	}
	now := time.Now().UTC()
	err = c.w.edit(func(secrets []Secret) ([]Secret, error) {
		if find(secrets, c.id) >= 0 {
			return nil, heldName(c.id)
		}
		return append(secrets, Secret{Name: c.id, Tags: c.tags, Value: value, CreatedAt: &now}), nil
	})
	if err != nil {
		return err
	}
	c.w.didApply("create")
	if lost {
		c.lost = true
		return lostAnswer("create")
	}
	return nil
}

// name is the NAME a session asks for the secret this create makes (policy.DisplayName).
func (c *create) name() string { return policy.DisplayName(c.w.prefix, c.id) }

// show is the command that says what the store holds of the secret, in backquotes.
func (c *create) show() string {
	return "`" + secretstore.Command("", "secret", "show", c.name()) + "`"
}

// Discard has nothing to remove: Begin made nothing.
func (c *create) Discard(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

// Unsettled names the check of a create whose answer was lost.
func (c *create) Unsettled() string {
	if !c.lost {
		return ""
	}
	return "the memory store may hold " + c.name() + "; check it with " + c.show()
}

// MadeBeforeValue is false: the memory store makes a secret with its value.
func (c *create) MadeBeforeValue() bool { return false }

// Set writes a new value of a secret the store holds.
func (w *Writer) Set(ctx context.Context, id, value string) error {
	lost, err := w.start(ctx, "set")
	if err != nil {
		return err
	}
	if err := w.edit(func(secrets []Secret) ([]Secret, error) {
		i := find(secrets, id)
		if i < 0 || secrets[i].DeletedAt != nil {
			return nil, secretstore.ErrNotFound
		}
		secrets[i].Value = value
		return secrets, nil
	}); err != nil {
		return err
	}
	w.didApply("set")
	if lost {
		return lostAnswer("set")
	}
	return nil
}

// CurrentTags reads a held secret's tags, with their etag and its source.
func (w *Writer) CurrentTags(ctx context.Context, id string) (secretstore.HeldTags, error) {
	ls, err := w.Describe(ctx, id, secretstore.ReadOptions{})
	if err != nil {
		return secretstore.HeldTags{}, err
	}
	return secretstore.HeldTags{Tags: ls.Tags, Etag: tagEtag(ls.Tags), Source: ls.Source}, nil
}

// SetTags writes tags, keeping every other tag, when the secret's tags are still the ones held
// was read with; another write since refuses it with ErrTagsChanged, writing nothing. check is
// unused: the memory store's tag write is conditional.
func (w *Writer) SetTags(ctx context.Context, id string, held secretstore.HeldTags, tags map[string]string, _ secretstore.TagCheck) error {
	return w.writeTags(ctx, id, held, func(t map[string]string) { maps.Copy(t, tags) })
}

// RemoveTags removes keys, keeping every other tag, on the same condition as SetTags.
func (w *Writer) RemoveTags(ctx context.Context, id string, held secretstore.HeldTags, keys []string, _ secretstore.TagCheck) error {
	return w.writeTags(ctx, id, held, func(t map[string]string) {
		for _, k := range keys {
			delete(t, k)
		}
	})
}

// writeTags applies change to the secret's tags when they still match held.
func (w *Writer) writeTags(ctx context.Context, id string, held secretstore.HeldTags, change func(map[string]string)) error {
	lost, err := w.start(ctx, "tags")
	if err != nil {
		return err
	}
	if err := w.edit(func(secrets []Secret) ([]Secret, error) {
		i := find(secrets, id)
		if i < 0 || secrets[i].DeletedAt != nil {
			return nil, secretstore.ErrNotFound
		}
		if held.Etag != "" && tagEtag(secrets[i].Tags) != held.Etag {
			return nil, secretstore.ErrTagsChanged
		}
		tags := maps.Clone(secrets[i].Tags)
		if tags == nil {
			tags = map[string]string{}
		}
		change(tags)
		secrets[i].Tags = tags
		return secrets, nil
	}); err != nil {
		return err
	}
	w.didApply("tags")
	if lost {
		return secretstore.TagsUnknown{Err: lostAnswer("tags")}
	}
	return nil
}

// Delete schedules the secret for deletion and answers the end of its recovery window.
func (w *Writer) Delete(ctx context.Context, id string) (time.Time, error) {
	lost, err := w.start(ctx, "delete")
	if err != nil {
		return time.Time{}, err
	}
	now := time.Now().UTC()
	if err := w.edit(func(secrets []Secret) ([]Secret, error) {
		i := find(secrets, id)
		if i < 0 || secrets[i].DeletedAt != nil {
			return nil, secretstore.ErrNotFound
		}
		secrets[i].DeletedAt = &now
		return secrets, nil
	}); err != nil {
		return time.Time{}, err
	}
	w.didApply("delete")
	if lost {
		return time.Time{}, lostAnswer("delete")
	}
	return now.Add(recoveryWindow), nil
}

// Restore brings back a secret scheduled for deletion.
func (w *Writer) Restore(ctx context.Context, id string) error {
	lost, err := w.start(ctx, "restore")
	if err != nil {
		return err
	}
	if err := w.edit(func(secrets []Secret) ([]Secret, error) {
		i := find(secrets, id)
		if i < 0 || secrets[i].DeletedAt == nil {
			return nil, secretstore.ErrNotFound
		}
		secrets[i].DeletedAt = nil
		return secrets, nil
	}); err != nil {
		return err
	}
	w.didApply("restore")
	if lost {
		return lostAnswer("restore")
	}
	return nil
}
