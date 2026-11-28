// Package memory is the secret store that tests and local runs read: secrets held in the process,
// with no cloud's shape, filled by a test (New, Put) or from a JSON file the store follows (Open,
// BROKER_MEMORY_SECRETS_FILE). It is the one development store: cipherd accepts its
// development-only settings with it alone (Backend.Development).
package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Name is BROKER_SECRETS_STORE's value for the memory store.
const Name = "memory"

// sourcePrefix opens every source the memory store answers.
const sourcePrefix = "memory:"

// maxNameLength is the longest full name a memory secret may carry, the longest Secrets Manager
// gives one, so a name that serves locally is one AWS could hold.
const maxNameLength = 512

// Secret is one secret the memory store holds, and one entry of its file.
type Secret struct {
	// Name is the secret's full name, the prefix included.
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
	// Value is its current value; "" is a secret with no value.
	Value string `json:"value"`
	// OffKey marks it off the agent-secrets key, which the policy refuses to serve.
	OffKey bool `json:"off_key,omitempty"`
	// CreatedAt is when it was created, unset where nobody says.
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// DeletedAt, when set, is when it was scheduled for deletion: List leaves it out and Describe
	// and Read answer ErrNotFound.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Store is the memory store: safe for concurrent use, and, from Open, its file.
type Store struct {
	mu      sync.Mutex
	secrets map[string]Secret
	// version numbers each held secret's value: serial as it stood when the value was last
	// written with other bytes than the secret held, so a listing's Version moves when, and only
	// when, the value's bytes do.
	version map[string]uint64
	serial  uint64
	// path is the file an Open store follows, and file the bytes its secrets were last read from;
	// empty for a New store.
	path string
	file []byte
}

// New holds secrets.
func New(secrets ...Secret) *Store {
	s := &Store{secrets: map[string]Secret{}, version: map[string]uint64{}}
	for _, secret := range secrets {
		s.hold(secret)
	}
	return s
}

// Source is the source the memory store answers for the secret whose full name is id.
func Source(id string) string { return sourcePrefix + id }

// Put holds secret, or replaces the one of its name; a value of other bytes than the one it
// replaces gets a new Version.
func (s *Store) Put(secret Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold(secret)
}

// Get answers the secret held at the full name id, with tags of its own, and false for a name the
// store does not hold: what the last Put held, or an Open store's file as its last call read it.
// A test that retags a secret reads it here and Puts it back.
func (s *Store) Get(id string) (Secret, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, ok := s.secrets[id]
	secret.Tags = maps.Clone(secret.Tags)
	return secret, ok
}

// Delete removes the secret whose full name is id outright.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, id)
	delete(s.version, id)
}

// hold makes secret the one of its name, numbering its value against the one that name held. The
// caller holds s.mu, or is building s.
func (s *Store) hold(secret Secret) {
	held, had := s.secrets[secret.Name]
	if _, numbered := s.version[secret.Name]; !had || !numbered || held.Value != secret.Value {
		s.serial++
		s.version[secret.Name] = s.serial
	}
	s.secrets[secret.Name] = secret
}

// listing is the Listing of the secret held at id. The caller holds s.mu.
func (s *Store) listing(id string) secretstore.Listing {
	secret := s.secrets[id]
	tags := maps.Clone(secret.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	ls := secretstore.Listing{ID: id, Source: Source(id), Tags: tags, HasCurrentValue: secret.Value != ""}
	onKey := !secret.OffKey
	ls.OnKey = func(context.Context) (bool, error) { return onKey, nil }
	if ls.HasCurrentValue {
		ls.Version = "memory-" + strconv.FormatUint(s.version[id], 10)
	}
	_, labelled := tags[secretstore.CreateLabel]
	ls.Unfilled = labelled && !ls.HasCurrentValue
	if secret.CreatedAt != nil {
		ls.Created = *secret.CreatedAt
	}
	return ls
}

// List answers every secret whose full name starts with prefix, leaving out one scheduled for
// deletion.
func (s *Store) List(_ context.Context, prefix string) ([]secretstore.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.follow(); err != nil {
		return nil, err
	}
	var listings []secretstore.Listing
	for id, secret := range s.secrets {
		if !strings.HasPrefix(id, prefix) || secret.DeletedAt != nil {
			continue
		}
		listings = append(listings, s.listing(id))
	}
	return listings, nil
}

// Describe answers the secret whose full name is id, or ErrNotFound for one the store does not
// hold and one scheduled for deletion.
func (s *Store) Describe(_ context.Context, id string) (secretstore.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.follow(); err != nil {
		return secretstore.Listing{}, err
	}
	secret, ok := s.secrets[id]
	if !ok || secret.DeletedAt != nil {
		return secretstore.Listing{}, secretstore.ErrNotFound
	}
	return s.listing(id), nil
}

// Read answers the value of the secret source names: ErrNotFound for a source no memory secret has,
// one scheduled for deletion and one with no value.
func (s *Store) Read(_ context.Context, source string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.follow(); err != nil {
		return "", err
	}
	id, ok := strings.CutPrefix(source, sourcePrefix)
	if !ok {
		return "", secretstore.ErrNotFound
	}
	secret, held := s.secrets[id]
	if !held || secret.DeletedAt != nil || secret.Value == "" {
		return "", secretstore.ErrNotFound
	}
	return secret.Value, nil
}

// errNameRule is ValidName's refusal.
var errNameRule = errors.New("a memory secret's name is 1 to 512 printable ASCII bytes with no space")

// ValidName refuses a full name of no bytes or more than 512, or with a byte outside 0x21-0x7e.
func (s *Store) ValidName(id string) error {
	if len(id) == 0 || len(id) > maxNameLength {
		return fmt.Errorf("under the prefix it makes a %d-byte name: %w", len(id), errNameRule)
	}
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return fmt.Errorf("under the prefix it makes a name with the byte %#x: %w", id[i], errNameRule)
		}
	}
	return nil
}

// ListLag is none: the store answers each write at once.
func (s *Store) ListLag() time.Duration { return 0 }
