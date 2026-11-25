package secretstoretest_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// writerMutantVariable names the mutant TestWriterContractOnMutant's writer carries; unset is
// "none".
const writerMutantVariable = "WRITER_CONTRACT_MUTANT"

var errRefDenied = errors.New("the reference store refused the call")

// refEntry is one secret of the reference writer. It keeps a list, not a map, so a mutant can
// make two secrets of one name.
type refEntry struct {
	name    string
	tags    map[string]string
	value   string
	version int
	deleted bool
}

// refWriter is the reference writer of the writer contract's own test: the smallest writer that
// keeps every case, with one mutant at a time that breaks one of them.
type refWriter struct {
	mu      sync.Mutex
	mutant  string
	entries []*refEntry
	serial  int
	lose    map[string]bool
	deny    map[string]string
}

func newRefWriter(mutant string) *refWriter {
	return &refWriter{mutant: mutant, lose: map[string]bool{}, deny: map[string]string{}}
}

// live is the first entry of id that is not deleted, or with deleted, any.
func (w *refWriter) entry(id string, deleted bool) *refEntry {
	for _, e := range w.entries {
		if e.name == id && (deleted || !e.deleted) {
			return e
		}
	}
	return nil
}

func (w *refWriter) start(call string) (lost bool, err error) {
	if message, denied := w.deny[call]; denied {
		delete(w.deny, call)
		failure := secretstore.Failure{Call: call, Words: "refused", Err: fmt.Errorf("%w: %s", errRefDenied, message)}
		if w.mutant == "prints-service-text" {
			return false, fmt.Errorf("%s: %w", message, failure)
		}
		return false, failure
	}
	lost = w.lose[call]
	delete(w.lose, call)
	return lost, nil
}

func (w *refWriter) listing(e *refEntry, opts secretstore.ReadOptions) secretstore.Listing {
	ls := secretstore.Listing{ID: e.name, Source: "ref:" + e.name, Tags: maps.Clone(e.tags), HasCurrentValue: e.value != ""}
	ls.OnKey = func(context.Context) (bool, error) { return true, nil }
	if ls.HasCurrentValue {
		ls.Version = fmt.Sprintf("v%d", e.version)
	}
	if e.deleted {
		ls.Deleted = time.Now()
	}
	return ls
}

func (w *refWriter) List(_ context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []secretstore.Listing
	for _, e := range w.entries {
		if strings.HasPrefix(e.name, prefix) && (!e.deleted || opts.IncludeDeleted) {
			out = append(out, w.listing(e, opts))
		}
	}
	return out, nil
}

func (w *refWriter) Describe(_ context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entry(id, opts.IncludeDeleted)
	if e == nil {
		return secretstore.Listing{}, secretstore.ErrNotFound
	}
	return w.listing(e, opts), nil
}

func (w *refWriter) Read(_ context.Context, source string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entry(strings.TrimPrefix(source, "ref:"), false)
	if e == nil || e.value == "" {
		return "", secretstore.ErrNotFound
	}
	return e.value, nil
}

func (w *refWriter) ValidName(string) error         { return nil }
func (w *refWriter) ListLag() time.Duration         { return 0 }
func (w *refWriter) Identity() string               { return "reference: test" }
func (w *refWriter) ChooseAnother() string          { return "choose another reference" }
func (w *refWriter) RerunPrefix() string            { return "" }
func (w *refWriter) Close() error                   { return nil }
func (w *refWriter) TagReadPermission() string      { return "read" }
func (w *refWriter) ValueReadPermission() string    { return "read" }
func (w *refWriter) MissingPermission(error) string { return "" }
func (w *refWriter) AccessDenied(err error) bool    { return errors.Is(err, errRefDenied) }
func (w *refWriter) DefiniteRefusal(err error) bool { return errors.Is(err, errRefDenied) }
func (w *refWriter) PersonEmail(context.Context) (string, error) {
	return "", errors.New("the reference signs in no person")
}

type refCreate struct {
	w    *refWriter
	id   string
	tags map[string]string
	lost bool
}

func (w *refWriter) PrepareCreate(id string, tags map[string]string) secretstore.Create {
	return &refCreate{w: w, id: id, tags: maps.Clone(tags)}
}

func (c *refCreate) Begin(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

func (c *refCreate) Store(_ context.Context, value string) error {
	w := c.w
	w.mu.Lock()
	defer w.mu.Unlock()
	lost, err := w.start("create")
	if err != nil {
		return err
	}
	if w.entry(c.id, true) != nil && w.mutant != "create-twice" {
		return errors.New("held")
	}
	w.serial++
	w.entries = append(w.entries, &refEntry{name: c.id, tags: c.tags, value: value, version: w.serial})
	if lost {
		if w.mutant == "lost-create-twice" {
			w.serial++
			w.entries = append(w.entries, &refEntry{name: c.id, tags: c.tags, value: value, version: w.serial})
		}
		c.lost = true
		return errors.New("answer lost")
	}
	return nil
}

func (c *refCreate) Discard(context.Context) (secretstore.DiscardOutcome, error) {
	return secretstore.MadeNothing, nil
}

func (c *refCreate) Unsettled() string {
	if c.lost {
		return "check " + c.id
	}
	return ""
}

func (c *refCreate) MadeBeforeValue() bool { return false }

func (w *refWriter) Set(_ context.Context, id, value string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	lost, err := w.start("set")
	if err != nil {
		return err
	}
	e := w.entry(id, false)
	if e == nil {
		return secretstore.ErrNotFound
	}
	w.serial++
	e.value, e.version = value, w.serial
	if lost {
		return errors.New("answer lost")
	}
	return nil
}

func etagOf(tags map[string]string) string {
	return fmt.Sprint(tags)
}

func (w *refWriter) CurrentTags(_ context.Context, id string) (secretstore.HeldTags, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entry(id, false)
	if e == nil {
		return secretstore.HeldTags{}, secretstore.ErrNotFound
	}
	return secretstore.HeldTags{Tags: maps.Clone(e.tags), Etag: etagOf(e.tags), Source: "ref:" + id}, nil
}

func (w *refWriter) writeTags(id string, held secretstore.HeldTags, change func(map[string]string)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	lost, err := w.start("tags")
	if err != nil {
		return err
	}
	e := w.entry(id, false)
	if e == nil {
		return secretstore.ErrNotFound
	}
	if held.Etag != "" && etagOf(e.tags) != held.Etag && w.mutant != "race-silent" {
		return secretstore.ErrTagsChanged
	}
	change(e.tags)
	if lost {
		return secretstore.TagsUnknown{Err: errors.New("answer lost")}
	}
	return nil
}

func (w *refWriter) SetTags(_ context.Context, id string, held secretstore.HeldTags, tags map[string]string, _ secretstore.TagCheck) error {
	return w.writeTags(id, held, func(t map[string]string) { maps.Copy(t, tags) })
}

func (w *refWriter) RemoveTags(_ context.Context, id string, held secretstore.HeldTags, keys []string, _ secretstore.TagCheck) error {
	return w.writeTags(id, held, func(t map[string]string) {
		for _, k := range keys {
			delete(t, k)
		}
	})
}

func (w *refWriter) Delete(_ context.Context, id string) (time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.start("delete"); err != nil {
		return time.Time{}, err
	}
	e := w.entry(id, false)
	if e == nil {
		return time.Time{}, secretstore.ErrNotFound
	}
	e.deleted = true
	return time.Now().Add(7 * 24 * time.Hour), nil
}

func (w *refWriter) Restore(_ context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.start("restore"); err != nil {
		return err
	}
	e := w.entry(id, true)
	if e == nil || !e.deleted {
		return secretstore.ErrNotFound
	}
	if w.mutant != "restore-noop" {
		e.deleted = false
	}
	return nil
}

// TestWriterContractOnMutant runs the writer contract on the reference writer with the mutant
// WRITER_CONTRACT_MUTANT names, none when unset: a plain run of the package passes it, and
// TestWriterContractOnMutants runs it once per mutant in a child.
func TestWriterContractOnMutant(t *testing.T) {
	mutant := os.Getenv(writerMutantVariable)
	if mutant == "" {
		mutant = "none"
	}
	secretstoretest.WriterContract(t, "ref", func(t *testing.T) secretstoretest.WriterRig {
		w := newRefWriter(mutant)
		return secretstoretest.WriterRig{
			Writer: w, Prefix: refPrefix,
			LoseNextAnswer: func(call string) { w.mu.Lock(); w.lose[call] = true; w.mu.Unlock() },
			DenyNext:       func(call, message string) { w.mu.Lock(); w.deny[call] = message; w.mu.Unlock() },
			Retag: func(t *testing.T, id string, tags map[string]string) {
				w.mu.Lock()
				defer w.mu.Unlock()
				maps.Copy(w.entry(id, false).tags, tags)
			},
		}
	})
}

// TestWriterContractOnMutants: each mutant of the reference writer fails the contract case whose
// property it breaks, and the writer with no mutant passes every case.
func TestWriterContractOnMutants(t *testing.T) {
	for mutant, breaks := range map[string]string{
		"none":                "",
		"create-twice":        "a create of a held name is refused and changes nothing",
		"lost-create-twice":   "a create whose answer is lost makes one secret",
		"race-silent":         "a raced tag write says so",
		"prints-service-text": "an access denial prints no service text",
		"restore-noop":        "a delete hides the secret and a restore brings it back",
	} {
		t.Run(mutant, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestWriterContractOnMutant$", "-test.count=1")
			cmd.Env = append(os.Environ(), writerMutantVariable+"="+mutant)
			out, err := cmd.CombinedOutput()
			if breaks == "" {
				if err != nil {
					t.Fatalf("the reference writer with no mutant failed the contract: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the reference writer with mutant %s passed the contract; want it to fail %q\n%s", mutant, breaks, out)
			}
			if want := "--- FAIL: TestWriterContractOnMutant/ref/" + strings.ReplaceAll(breaks, " ", "_"); !strings.Contains(string(out), want) {
				t.Fatalf("mutant %s: the output does not hold %q\n%s", mutant, want, out)
			}
		})
	}
}
