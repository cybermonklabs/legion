package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// file is the shape of the memory secrets file BROKER_MEMORY_SECRETS_FILE names:
// {"secrets": [Secret, ...]}.
type file struct {
	Secrets []Secret `json:"secrets"`
}

// Open reads path, a memory secrets file, into a store that follows it: each call reads the file
// again and, when its bytes have changed, answers from it, so an edit to the file is a write the
// next read sees. A value an edit leaves as it was keeps its Version, and one it changes moves it.
// A secret Put directly stands until the file next changes. An error names path and, for a file
// that does not parse, only the byte offset and field it failed at, never the file's content: no
// secret value reaches an error message, local development included.
func Open(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	secrets, err := parseFile(path, data)
	if err != nil {
		return nil, err
	}
	s := New(secrets...)
	s.path, s.file = path, data
	return s, nil
}

// parseFile reads data, the content of path, as a memory secrets file.
func parseFile(path string, data []byte) ([]Secret, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return nil, fmt.Errorf("%s: not JSON (at byte %d)", path, syntax.Offset)
		case errors.As(err, &typeErr):
			return nil, fmt.Errorf("%s: %s is not a %s (at byte %d)", path, typeErr.Field, typeErr.Type, typeErr.Offset)
		case err.Error() == `json: unknown field "kms_key_id"`:
			return nil, fmt.Errorf("%s: kms_key_id is a field of the fake secrets file the memory store replaced; take it out (off_key: true marks a secret off the agent-secrets key)", path)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return nil, fmt.Errorf("%s: %s", path, strings.TrimPrefix(err.Error(), "json: "))
		default:
			return nil, fmt.Errorf("%s: not a memory secrets file", path)
		}
	}
	for i, s := range f.Secrets {
		if s.Name == "" {
			return nil, fmt.Errorf("%s: secrets[%d] has no name", path, i)
		}
	}
	return f.Secrets, nil
}

// follow reads an Open store's file again and, when its bytes have changed since they were last
// read, takes its secrets from it, each value numbered against the one its name held before, so a
// value the edit changed gets a new Version and one it left alone keeps its own. A file it cannot
// read or parse fails the call. The caller holds s.mu.
func (s *Store) follow() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	if bytes.Equal(data, s.file) {
		return nil
	}
	secrets, err := parseFile(s.path, data)
	if err != nil {
		return err
	}
	previous := s.secrets
	s.secrets = make(map[string]Secret, len(secrets))
	for _, secret := range secrets {
		if held, had := previous[secret.Name]; had {
			s.secrets[secret.Name] = held
		}
		s.hold(secret)
	}
	for name := range s.version {
		if _, held := s.secrets[name]; !held {
			delete(s.version, name)
		}
	}
	s.file = data
	return nil
}
