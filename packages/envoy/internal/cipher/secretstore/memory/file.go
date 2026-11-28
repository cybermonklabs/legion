package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// file is the shape of the memory secrets file BROKER_MEMORY_SECRETS_FILE names:
// {"secrets": [Secret, ...]}.
type file struct {
	Secrets []fileSecret `json:"secrets"`
}

// fileSecret is one entry of the file: a Secret, and kms_key_id, a field of the fake secrets file
// the memory store replaced, which the decoder reads so that parseFile can refuse it by name. It is
// a json.RawMessage, not a pointer, so that a literal null is held too: any kms_key_id is refused.
type fileSecret struct {
	Secret
	KMSKeyID json.RawMessage `json:"kms_key_id,omitempty"`
}

// Open reads path, a memory secrets file, into a store that follows it: each call reads the file
// again and, when its bytes have changed, answers from it, so an edit to the file is a write the
// next read sees. A value an edit leaves as it was keeps its Version, and one it changes moves it.
// A secret Put directly stands until the file next changes. An error names path and, for a file
// that does not parse, only the byte offset and field it failed at, never the file's content: no
// secret value reaches an error message, local development included.
func Open(path string) (*Store, error) { return openShown(path, path) }

// openShown is Open of path whose errors, then and on each later read, name the file as shown.
func openShown(path, shown string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fileFailure("read", shown, err)
	}
	secrets, err := parseFile(shown, data)
	if err != nil {
		return nil, err
	}
	s := New(secrets...)
	s.path, s.shown, s.file = path, shown, data
	return s, nil
}

// fileFailure is err, from the operation op on the file shown names, as a message prints it: the
// file as shown and the system's reason (redact.File), never the path the call was handed, which
// the error of every *fs.PathError and *os.LinkError repeats. It unwraps to err.
func fileFailure(op, shown string, err error) error {
	return fmt.Errorf("%s %s: %w", op, shown, redact.File(err))
}

// parseFile reads data as a memory secrets file, whose errors name the file as shown.
func parseFile(shown string, data []byte) ([]Secret, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return nil, fmt.Errorf("%s: not JSON (at byte %d)", shown, syntax.Offset)
		case errors.As(err, &typeErr):
			return nil, fmt.Errorf("%s: %s is not a %s (at byte %d)", shown, typeErr.Field, typeErr.Type, typeErr.Offset)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return nil, fmt.Errorf("%s: %s", shown, strings.TrimPrefix(err.Error(), "json: "))
		default:
			return nil, fmt.Errorf("%s: not a memory secrets file", shown)
		}
	}
	secrets := make([]Secret, 0, len(f.Secrets))
	for i, s := range f.Secrets {
		if len(s.KMSKeyID) != 0 {
			return nil, fmt.Errorf("%s: kms_key_id is a field of the fake secrets file the memory store replaced; take it out (off_key: true marks a secret off the agent-secrets key)", shown)
		}
		if s.Name == "" {
			return nil, fmt.Errorf("%s: secrets[%d] has no name", shown, i)
		}
		secrets = append(secrets, s.Secret)
	}
	return secrets, nil
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
		return fileFailure("read", s.shown, err)
	}
	if bytes.Equal(data, s.file) {
		return nil
	}
	secrets, err := parseFile(s.shown, data)
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
