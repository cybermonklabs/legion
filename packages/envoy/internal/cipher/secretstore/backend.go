package secretstore

import (
	"context"
	"log/slog"
	"time"

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
	// Database is the broker's Postgres, which every replica shares: where a store keeps what all
	// of them count together, such as the 1Password store's daily request budget. cipherd hands
	// every store the one its database answers (store.Store.SecretStoreDatabase).
	Database Database
	// Now is the broker's clock.
	Now func() time.Time
}

// Database is the broker's Postgres as a store reads and writes it: one transaction at a time,
// over plain Go values. It names no database driver's type, so a store package, and the
// agent-secrets CLI that links every store, links no driver.
type Database interface {
	// InTx runs fn in one transaction, committed when fn returns nil and rolled back otherwise.
	InTx(ctx context.Context, fn func(Tx) error) error
}

// Tx is one transaction of a Database.
type Tx interface {
	// Exec runs a statement with args.
	Exec(ctx context.Context, sql string, args ...any) error
	// QueryRow runs a query of one row with args; its Scan reads the row's columns into dest.
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Row is one row a Tx's QueryRow answered.
type Row interface {
	Scan(dest ...any) error
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

	// Kind is the store's kind as the agent-secrets CLI's lines name it ("AWS Secrets Manager").
	Kind() string
	// Place is where the broker's settings s put the agent secrets, as a line names it after
	// Kind ("account <id> in <region>"), "" where they name nowhere.
	Place(s Settings) string
	// OpenWriter checks s, then signs in as the CLI's caller and opens the store's writer.
	OpenWriter(ctx context.Context, s Settings, opts WriterOptions) (Writer, error)
	// HelpRows are this store's rows of the CLI's environment table, laid out as they print
	// (two spaces, the name padded to column 29, wrapped lines indented to it), each line ending
	// in \n; "" for none.
	HelpRows() string
	// ProfileHint is this store's clause of the CLI's --profile refusal; false for none.
	ProfileHint() (ProfileHint, bool)
}
