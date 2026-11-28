// packages/envoy/cmd/cipher/memory_rig_test.go
//
// The memory store as the secret forms' tests write it: a memory secrets file in the test's own
// directory, which the fake broker follows (memoryServedBy) and agent-secrets writes, and the
// hooks of every writer the forms open over it (memory.SwapHooks).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// memorySettings are a broker's settings on the memory store whose file is path, under
// policytest's prefix, as a broker of this release that reads the type tag answers them.
func memorySettings(path string) Settings {
	return Settings{Store: memory.Name, SecretsPrefix: policytest.Prefix, SecretTypeTag: true, Fields: map[string]string{"memory_secrets_file": path}}
}

// memoryTypeStore is the memory store as typeStore: the file at path, and every writer the forms
// opened over it, each refusing what the rig refuses at the time it opens and from then on.
type memoryTypeStore struct{ rig *memoryRig }

// memoryRig is the state a memoryTypeStore shares with the writers it hooks.
type memoryRig struct {
	path string

	mu             sync.Mutex
	writers        []*memory.Writer
	refuseReads    error
	refuseDescribe error
	// tagWrites counts the tag writes that changed the file; loseTagAt and refuseTagAt are the
	// one whose answer is lost and the one refused (loseTagWriteAnswer, refuseTagWrite), 0 for none.
	tagWrites, loseTagAt, refuseTagAt int
	afterWrite                        func()
}

// armTagWrite sets the hook of the tag write after the tagWrites so far on w: the writer's hooks
// are one-shot, so each is set just before the write it is for. The caller holds r.mu.
func (r *memoryRig) armTagWrite(w *memory.Writer) {
	switch r.tagWrites + 1 {
	case r.loseTagAt:
		w.LoseNext("tags")
	case r.refuseTagAt:
		w.DenyNext("tags", "the memory store refused the tag write")
	}
}

// newMemoryTypeStore makes an empty memory secrets file in t's directory and hooks every writer
// opened over it for the rest of t.
func newMemoryTypeStore(t *testing.T) memoryTypeStore {
	t.Helper()
	rig := &memoryRig{path: filepath.Join(t.TempDir(), "secrets.json")}
	writeMemoryFile(t, rig.path, nil)
	t.Cleanup(memory.SwapHooks(memory.Hooks{
		Opened: func(w *memory.Writer) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.writers = append(rig.writers, w)
			if rig.refuseReads != nil {
				w.Fail("read", rig.refuseReads)
			}
			if rig.refuseDescribe != nil {
				w.Fail("describe", rig.refuseDescribe)
			}
			rig.armTagWrite(w)
		},
		Applied: func(call string) {
			rig.mu.Lock()
			if call == "tags" {
				rig.tagWrites++
				rig.armTagWrite(rig.writers[len(rig.writers)-1])
			}
			after := rig.afterWrite
			rig.mu.Unlock()
			if after != nil && (call == "create" || call == "set" || call == "tags") {
				after()
			}
		},
	}))
	return memoryTypeStore{rig}
}

// memoryFile is the memory secrets file's shape.
type memoryFile struct {
	Secrets []memory.Secret `json:"secrets"`
}

// readMemoryFile is the secrets of the memory secrets file at path.
func readMemoryFile(t *testing.T, path string) []memory.Secret {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f memoryFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f.Secrets
}

// writeMemoryFile writes secrets as the whole memory secrets file at path.
func writeMemoryFile(t *testing.T, path string, secrets []memory.Secret) {
	t.Helper()
	if secrets == nil {
		secrets = []memory.Secret{}
	}
	data, err := json.Marshal(memoryFile{Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// secret is the secret name as the file holds it, false where it holds none.
func (s memoryTypeStore) secret(t *testing.T, name string) (memory.Secret, bool) {
	t.Helper()
	id := policytest.ID(name)
	for _, secret := range readMemoryFile(t, s.rig.path) {
		if secret.Name == id {
			return secret, true
		}
	}
	return memory.Secret{}, false
}

func (s memoryTypeStore) put(t *testing.T, name string, tags map[string]string, value string) {
	t.Helper()
	id := policytest.ID(name)
	secrets := readMemoryFile(t, s.rig.path)
	held := memory.Secret{Name: id, Tags: maps.Clone(tags), Value: value}
	for i := range secrets {
		if secrets[i].Name == id {
			secrets[i] = held
			writeMemoryFile(t, s.rig.path, secrets)
			return
		}
	}
	writeMemoryFile(t, s.rig.path, append(secrets, held))
}

func (s memoryTypeStore) retag(t *testing.T, name string, set map[string]string, remove ...string) {
	t.Helper()
	id := policytest.ID(name)
	secrets := readMemoryFile(t, s.rig.path)
	for i := range secrets {
		if secrets[i].Name != id {
			continue
		}
		tags := maps.Clone(secrets[i].Tags)
		if tags == nil {
			tags = map[string]string{}
		}
		maps.Copy(tags, set)
		for _, key := range remove {
			delete(tags, key)
		}
		secrets[i].Tags = tags
		writeMemoryFile(t, s.rig.path, secrets)
		return
	}
	t.Fatalf("the other writer's retag: the memory store holds no %s", id)
}

func (s memoryTypeStore) afterEachWrite(f func()) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.afterWrite = f
}

func (s memoryTypeStore) has(t *testing.T, name string) bool {
	_, ok := s.secret(t, name)
	return ok
}

func (s memoryTypeStore) value(t *testing.T, name string) string {
	secret, _ := s.secret(t, name)
	return secret.Value
}

func (s memoryTypeStore) tags(t *testing.T, name string) map[string]string {
	secret, _ := s.secret(t, name)
	return secret.Tags
}

// refuseValueReads makes every value read of each writer opened from now on answer
// memory.ErrDenied when denied, and a failure to read the file otherwise.
func (s memoryTypeStore) refuseValueReads(denied bool) {
	err := errors.New("the memory secrets file could not be read")
	if denied {
		err = memory.ErrDenied
	}
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseReads = err
}

func (s memoryTypeStore) valueReads() int { return s.rig.calls("read") }

func (s memoryTypeStore) readPermissions() []string {
	return []string{"read access to the memory secrets file"}
}

func (s memoryTypeStore) calls() int { return s.rig.calls("") }

func (s memoryTypeStore) tagWrites() int { return s.rig.calls("tags") }

// refuseTagReads makes every tag read of each writer opened from now on answer memory.ErrDenied,
// whose words name no permission.
func (s memoryTypeStore) refuseTagReads() {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseDescribe = memory.ErrDenied
}

// loseTagWriteAnswer makes the nth tag write from now on apply and then lose its answer
// (memory.Writer.LoseNext), and refuseTagWrite makes it refused as access denied (DenyNext).
func (s memoryTypeStore) loseTagWriteAnswer(n int) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.loseTagAt = s.rig.tagWrites + n
	if len(s.rig.writers) > 0 {
		s.rig.armTagWrite(s.rig.writers[len(s.rig.writers)-1])
	}
}

func (s memoryTypeStore) refuseTagWrite(n int) {
	s.rig.mu.Lock()
	defer s.rig.mu.Unlock()
	s.rig.refuseTagAt = s.rig.tagWrites + n
	if len(s.rig.writers) > 0 {
		s.rig.armTagWrite(s.rig.writers[len(s.rig.writers)-1])
	}
}

func (s memoryTypeStore) setValue(t *testing.T, name, value string) {
	t.Helper()
	id := policytest.ID(name)
	secrets := readMemoryFile(t, s.rig.path)
	for i := range secrets {
		if secrets[i].Name == id {
			secrets[i].Value = value
			writeMemoryFile(t, s.rig.path, secrets)
			return
		}
	}
	t.Fatalf("the other writer's value: the memory store holds no %s", id)
}

func (s memoryTypeStore) tagReadPermission() string { return "read access to the memory secrets file" }

func (s memoryTypeStore) storeName() string { return "memory file " + s.rig.path }

func (s memoryTypeStore) madeBeforeValue() bool { return false }

// calls is how many calls of kind call ("" for every kind) the writers opened so far were asked.
func (r *memoryRig) calls(call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, w := range r.writers {
		total += w.Calls(call)
	}
	return total
}

// memoryServedBy answers a reread as the broker would from the memory secrets file at path, which
// it follows: absent for a secret it does not hold or one scheduled for deletion, no-current-value
// for one with no value, and otherwise what its tags answer (servedAs).
func memoryServedBy(path string) func(name string) Reread {
	return func(name string) Reread {
		store, err := memory.Open(path)
		if err != nil {
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		}
		listing, err := store.Describe(context.Background(), policytest.ID(name), secretstore.ReadOptions{})
		switch {
		case err != nil:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case !listing.HasCurrentValue:
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return servedAs(name, listing.Tags)
	}
}
