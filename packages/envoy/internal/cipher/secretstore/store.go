// Package secretstore is the one seam between the broker and the store that holds the agent
// secrets: Store, the interface the policy lists and describes them through and a grant's value
// is read through, and Backend, a store's entry in the registry (secretstore/backends), which
// cipherd opens its store from. Each store is one package under this one (aws, gcp, memory), and
// secretstoretest holds the contract each of them runs.
package secretstore

import (
	"context"
	"errors"
	"time"

	"github.com/sjawhar/envoy/internal/ratelimit"
)

// ErrNotFound is a secret the store does not hold: Describe's answer for a name no secret carries,
// and Read's for a source the store no longer holds or that has no current value.
var ErrNotFound = errors.New("secret not found in the store")

// ErrValueNotText is what Read's error wraps for a current value that is not text the broker can
// hand a command - an empty value, a Secrets Manager SecretBinary with no SecretString, a Secret
// Manager payload that is not UTF-8 - so no release and no mint could use it. It is the value's
// fault and not the read's, so the policy refuses such a type secret as it refuses one its
// decoder refuses (policy.ReasonValueMalformed), rather than reading it again at every load.
var ErrValueNotText = errors.New("value is not a non-empty UTF-8 string")

// CreateLabel is the label `agent-secrets secret create` puts on a Secret Manager secret it makes
// before its value, set to a random value of that create's own. The broker reads only whether a
// secret carries it, never its value, and never as a tag the policy weighs: on Secret Manager it is
// a label, which no listing's Tags carries. On Secrets Manager no create writes it, since a create
// there makes the secret with its value; the AWS store reads a tag of that name the same way,
// which only another writer, such as a fake a test fills, can set.
const CreateLabel = "agent-secrets-create"

// DefaultName is the store a deployment that names none runs on (BROKER_SECRETS_STORE unset), and
// the store a broker from before the settings' store field holds the secrets in: AWS Secrets
// Manager.
const DefaultName = "aws-secrets-manager"

// DefaultRereadBudget is the whole fleet's budget of POST /v1/secrets/{name}/reread on a store
// that names none of its own (Backend.RereadBudget): four rereads a second, a burst of ten.
// internal/cipher/api divides it among the pods (RereadOverallFor), and says why it is this rate.
var DefaultRereadBudget = ratelimit.Limit{Every: 250 * time.Millisecond, Burst: 10}

// Reader answers a granted secret's value: what releasing a grant needs, and nothing more.
type Reader interface {
	// Read answers the current value of the secret source names (Listing.Source): ErrNotFound
	// for one the store no longer holds, one scheduled for deletion or one with no current value,
	// and an error wrapping ErrValueNotText for a current value that is not text.
	Read(ctx context.Context, source string) (string, error)
}

// ReadOptions asks List and Describe for more than the policy reads. The broker sets neither; the
// agent-secrets CLI's list and show do.
type ReadOptions struct {
	// IncludeDeleted also answers a secret scheduled for deletion, with Listing.Deleted set.
	IncludeDeleted bool
	// Versions fills Listing.Versions, which may cost a call of its own per secret.
	Versions bool
}

// Store is what the broker reads agent secrets with. List and Describe answer the metadata the
// policy decides from, and Read answers a granted value; nothing here writes a secret.
type Store interface {
	Reader
	// List answers every secret whose full name starts with prefix, byte for byte, in no promised
	// order. A secret scheduled for deletion is one the store does not hold unless
	// opts.IncludeDeleted asks for it: List leaves it out, Describe and Read answer ErrNotFound for
	// it.
	List(ctx context.Context, prefix string, opts ReadOptions) ([]Listing, error)
	// Describe answers one secret by its full name, or ErrNotFound when none exists or, unless
	// opts.IncludeDeleted asks for it, it is scheduled for deletion.
	Describe(ctx context.Context, id string, opts ReadOptions) (Listing, error)
	// ValidName refuses, before any call is made, a full name no secret of this store can carry:
	// one past its length limit, or outside its character set. The error names the rule and never
	// wraps a lookup's failure.
	ValidName(id string) error
	// ListLag is how long the store documents its metadata reads may answer a write's previous
	// state: the policy keeps what a single-name read settled, and re-describes it on every
	// reload, for that long.
	ListLag() time.Duration
}

// Listing is one secret's metadata, the same shape from List and Describe.
type Listing struct {
	// ID is the secret's full name under the prefix: its name in the store, the prefix included.
	ID string
	// Source is what Read takes: the store's own handle on the secret, such as an ARN or a
	// resource name. No store's source starts with "mint:", which opens a type secret's source
	// in the policy.
	Source string
	// Tags carry the policy's owner and tier, and every other tag the policy reads.
	Tags map[string]string
	// OnKey answers whether the secret is encrypted with the agent-secrets key the store was
	// built with; a store with no key of its own answers true but for a secret its writer marked
	// off the key. It may make a call (the AWS store resolves the key's aliases, at most once per
	// List or Describe), so the policy asks it only after every tag check, and a failed call fails
	// the whole load.
	OnKey func(ctx context.Context) (bool, error)
	// HasCurrentValue is whether a read would answer a value.
	HasCurrentValue bool
	// Version is the id of the version a Read answers now, "" when HasCurrentValue is false. It
	// comes from the call that answers HasCurrentValue, so it costs no call of its own, and it is
	// the store's own id, never a digest of the value. The policy decodes a type secret's value
	// once for each version and type it lists (policy.Current), so a write of a new value is
	// decoded at the next load and an unchanged one is read at no load.
	Version string
	// Created is when the store created the secret, and the zero time where its answer named none.
	// The policy reads it for an Unfilled secret alone: one created less than
	// policy.PendingValueWindow ago is a create still waiting for its value.
	Created time.Time
	// Unfilled is whether the secret carries CreateLabel and holds no version at all: a secret
	// `agent-secrets secret create` made before its value and has not given one. A secret with any
	// version (a disabled one included), or without the label, is not.
	Unfilled bool
	// LastChanged is when the secret was last changed; Deleted is the deletion time the store
	// names for a secret scheduled for deletion (when the delete ran, or when the store will
	// remove it), answered only to a read with ReadOptions.IncludeDeleted; and EarliestPurge is
	// the earliest moment the store may remove it for good, where that is not Deleted itself. Each
	// is the zero time where the store answers none.
	LastChanged, Deleted, EarliestPurge time.Time
	// Versions maps each version id to its labels or state, as the store names them; nil unless
	// the read set ReadOptions.Versions. Version, when set, is one of its keys.
	Versions map[string][]string
}
