package secretstoretest_test

import (
	"context"
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

// mutantVariable names the mutant TestStoreContractOnMutant's store carries; unset is "none".
const mutantVariable = "CONTRACT_MUTANT"

// refPrefix is the reference store's prefix.
const refPrefix = "ref/agent-secrets/"

// refStore is the reference store of the contract's own test: the smallest store that keeps every
// case, with one mutant at a time that breaks one of them. It holds each fixture by its full name.
type refStore struct {
	mu      sync.Mutex
	mutant  string
	held    map[string]secretstoretest.Fixture
	serial  int
	version map[string]int
}

func newRefStore(mutant string) *refStore {
	return &refStore{mutant: mutant, held: map[string]secretstoretest.Fixture{}, version: map[string]int{}}
}

func (s *refStore) put(f secretstoretest.Fixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := refPrefix + f.Slug
	if f.Outside {
		id = "x" + id
	}
	before, had := s.held[id]
	if !had || before.Value != f.Value && s.mutant != "version-stays" {
		s.serial++
		s.version[id] = s.serial
	}
	s.held[id] = f
}

func (s *refStore) remove(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, refPrefix+slug)
}

// listing is id's Listing; the caller holds s.mu.
func (s *refStore) listing(id string, opts secretstore.ReadOptions) secretstore.Listing {
	f := s.held[id]
	tags := maps.Clone(f.Tags)
	source := "ref:" + id
	if s.mutant == "mint-source" {
		source = "mint:" + id
	}
	ls := secretstore.Listing{
		ID: id, Source: source, Tags: tags, HasCurrentValue: f.Value != "",
		Unfilled: f.Unfilled && f.Value == "",
	}
	off := f.OffKey && s.mutant != "off-key-on"
	ls.OnKey = func(context.Context) (bool, error) { return !off, nil }
	if ls.HasCurrentValue {
		ls.Version = fmt.Sprintf("v%d", s.version[id])
	}
	if f.Deleted {
		ls.Deleted = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	}
	if opts.Versions {
		ls.Versions = map[string][]string{}
		if ls.Version != "" {
			ls.Versions[ls.Version] = []string{"CURRENT"}
		}
	}
	return ls
}

func (s *refStore) List(_ context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var listings []secretstore.Listing
	for id, held := range s.held {
		if !strings.HasPrefix(id, prefix) || held.Deleted && !opts.IncludeDeleted && s.mutant != "lists-deleted" {
			continue
		}
		listings = append(listings, s.listing(id, opts))
	}
	return listings, nil
}

func (s *refStore) Describe(_ context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.held[id]
	if !ok || held.Deleted && !opts.IncludeDeleted && s.mutant != "describes-deleted" {
		return secretstore.Listing{}, secretstore.ErrNotFound
	}
	ls := s.listing(id, opts)
	if s.mutant == "describe-differs" {
		ls.Tags["described"] = "yes"
	}
	return ls, nil
}

func (s *refStore) Read(_ context.Context, source string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, id, _ := strings.Cut(source, ":")
	held, ok := s.held[id]
	if !ok || held.Value == "" || held.Deleted && s.mutant != "reads-deleted" {
		return "", secretstore.ErrNotFound
	}
	return held.Value, nil
}

func (s *refStore) ValidName(id string) error {
	if len(id) > 512 {
		return fmt.Errorf("a %d-byte name, past 512", len(id))
	}
	return nil
}

func (s *refStore) ListLag() time.Duration { return 0 }

// TestStoreContractOnMutant runs the contract on the reference store with the mutant that
// CONTRACT_MUTANT names, none when it is unset: a plain run of the package passes it, and
// TestStoreContractOnMutants runs it once per mutant in a child.
func TestStoreContractOnMutant(t *testing.T) {
	mutant := os.Getenv(mutantVariable)
	if mutant == "" {
		mutant = "none"
	}
	secretstoretest.StoreContract(t, "ref", func(t *testing.T) secretstoretest.StoreRig {
		s := newRefStore(mutant)
		return secretstoretest.StoreRig{
			Store: s, Prefix: refPrefix, OffKey: true, Unfilled: true,
			Put:    func(_ *testing.T, f secretstoretest.Fixture) { s.put(f) },
			Remove: func(_ *testing.T, slug string) { s.remove(slug) },
		}
	})
}

// TestStoreContractOnMutants: each mutant of the reference store fails the contract case whose
// property it breaks, and the store with no mutant passes every case, so the contract can fail
// and fails where it should.
func TestStoreContractOnMutants(t *testing.T) {
	for mutant, breaks := range map[string]string{
		"none":              "",
		"lists-deleted":     "a secret scheduled for deletion is absent unless the read asks for it",
		"describes-deleted": "a secret scheduled for deletion is absent unless the read asks for it",
		"reads-deleted":     "a secret scheduled for deletion is absent unless the read asks for it",
		"version-stays":     "a new value moves Version",
		"mint-source":       "no source starts with mint:",
		"off-key-on":        "OnKey answers the agent-secrets key",
		"describe-differs":  "Describe answers what List answers for one name",
	} {
		t.Run(mutant, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestStoreContractOnMutant$", "-test.count=1")
			cmd.Env = append(os.Environ(), mutantVariable+"="+mutant)
			out, err := cmd.CombinedOutput()
			if breaks == "" {
				if err != nil {
					t.Fatalf("the reference store with no mutant failed the contract: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the reference store with mutant %s passed the contract; want it to fail %q\n%s", mutant, breaks, out)
			}
			if want := "--- FAIL: TestStoreContractOnMutant/ref/" + strings.ReplaceAll(breaks, " ", "_"); !strings.Contains(string(out), want) {
				t.Fatalf("mutant %s: the output does not hold %q\n%s", mutant, want, out)
			}
		})
	}
}
