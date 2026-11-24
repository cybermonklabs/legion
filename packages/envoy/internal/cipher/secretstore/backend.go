package secretstore

import (
	"context"
	"log/slog"

	"github.com/sjawhar/envoy/internal/ratelimit"
)

// Config is one store's configuration, as its Backend parsed it from the broker's environment.
type Config interface {
	// Store is the store's name, BROKER_SECRETS_STORE's value (Backend.Name).
	Store() string
	// Prefix is BROKER_SECRETS_PREFIX, the namespace every agent secret's name starts with.
	Prefix() string
	// Settings is the store's own fields of GET /v1/settings, by JSON name, each "" left out:
	// settingsfields.Of of the store's SettingsFields struct.
	Settings() map[string]string
}

// OpenEnv is what cipherd hands every store it opens.
type OpenEnv struct {
	// Log is the broker's logger, which a store's SDK logs through where it logs at all.
	Log *slog.Logger
}

// Opened is a store Backend.OpenStore opened: the Store, the credentials it reads with as the
// broker's boot line names them, and Close, which releases its client and may be nil.
type Opened struct {
	Store       Store
	Credentials string
	Close       func() error
}

// Backend is a store's entry in the registry (secretstore/backends): what cipherd and the CLI know
// of a store beyond its Store. No code outside the store's own package and the registry names a
// store; each reads it through this.
type Backend interface {
	// Name is the store's BROKER_SECRETS_STORE value and the store field of GET /v1/settings.
	Name() string
	// ID is the store's short name: its package's and its registry file's name, the suffix of the
	// build tag that leaves it out (cipher_no_<id>), and the subtest name of every test that runs
	// on each store.
	ID() string
	// ServiceSDKs are the import path prefixes no package but the store's own may import: its
	// service's SDK.
	ServiceSDKs() []string
	// Development is true for a store that serves tests and local runs alone, with which cipherd
	// accepts its development-only settings.
	Development() bool
	// RereadBudget is the whole fleet's budget of rereads on this store (DefaultRereadBudget, or a
	// budget the store's own quota sets).
	RereadBudget() ratelimit.Limit
	// Variables are the BROKER_* variables only this store reads; config.Load refuses each with
	// another store.
	Variables() []string
	// ParseConfig reads the store's configuration from getenv, prefix being
	// BROKER_SECRETS_PREFIX, and refuses one the store cannot read with, naming the variable.
	ParseConfig(prefix string, getenv func(string) string) (Config, error)
	// OpenStore opens the store cfg, a Config this Backend parsed, describes.
	OpenStore(ctx context.Context, cfg Config, env OpenEnv) (Opened, error)
}
