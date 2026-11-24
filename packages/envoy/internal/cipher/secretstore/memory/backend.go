package memory

import (
	"context"
	"errors"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// BROKER_MEMORY_SECRETS_FILE: with BROKER_SECRETS_STORE=memory, the JSON file the broker reads the
// agent secrets from, {"secrets": [{"name", "tags", "value", "off_key", "created_at",
// "deleted_at"}, ...]}, which it follows, so an edit is a write it serves at the next read. Local
// development and tests alone: memory is the one development store.
const fileVariable = "BROKER_MEMORY_SECRETS_FILE"

// SettingsFields is the memory store's own fields of GET /v1/settings.
type SettingsFields struct {
	// With memory, the path of the JSON file BROKER_MEMORY_SECRETS_FILE names, which the broker
	// follows and agent-secrets writes; absent otherwise.
	File string `json:"memory_secrets_file,omitempty"`
}

// Backend is the memory store's registry entry.
type Backend struct{}

func (Backend) Name() string                  { return Name }
func (Backend) ID() string                    { return "memory" }
func (Backend) ServiceSDKs() []string         { return nil }
func (Backend) Development() bool             { return true }
func (Backend) RereadBudget() ratelimit.Limit { return secretstore.DefaultRereadBudget }
func (Backend) Variables() []string           { return []string{fileVariable} }

// config is the memory store's configuration: the prefix and the file.
type config struct{ prefix, file string }

func (c config) Store() string  { return Name }
func (c config) Prefix() string { return c.prefix }
func (c config) Settings() map[string]string {
	return settingsfields.Of(SettingsFields{File: c.file})
}

// ParseConfig reads BROKER_MEMORY_SECRETS_FILE, which the memory store requires.
func (Backend) ParseConfig(prefix string, getenv func(string) string) (secretstore.Config, error) {
	path := getenv(fileVariable)
	if path == "" {
		return nil, errors.New(fileVariable + " is required with BROKER_SECRETS_STORE=" + Name)
	}
	return config{prefix: prefix, file: path}, nil
}

// OpenStore opens the file cfg names (Open).
func (Backend) OpenStore(_ context.Context, cfg secretstore.Config, _ secretstore.OpenEnv) (secretstore.Opened, error) {
	store, err := Open(cfg.(config).file)
	if err != nil {
		return secretstore.Opened{}, err
	}
	return secretstore.Opened{Store: store, Credentials: "memory secrets file"}, nil
}
