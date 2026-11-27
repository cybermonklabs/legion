package ciphertoken

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/fileerr"
)

// AccountTokenCacheTTL is how long an AccountToken reuses the bytes it last read. Opening the file
// on every call would cost a read per request for a credential that changes a few times a day: the
// kubelet rewrites a projected token in place well before it expires, and the broker refuses one
// that lives longer than an hour from its issue, so bytes this fresh are never near expiry and a
// rewrite reaches the broker within this window of it.
const AccountTokenCacheTTL = 30 * time.Second

// Account is the caller's own platform identity, read fresh at call time: the bearer every call
// this package makes to the secrets broker presents, from which the broker resolves the calling
// service (BROKER_API_ACCOUNTS). It is an interface because the two callers hold two kinds of
// identity and the broker takes either: Dispatch and a listener in a pod hold a projected
// Kubernetes ServiceAccount token in a file (AccountToken), and a listener on a Compute Engine
// VM holds no Kubernetes token at all and asks its metadata server for the machine's own Google
// identity token instead (GoogleAccount).
//
// Read is called on the path of one introspection, so an implementation caches what it read: the
// credentials behind both implementations rotate, which is why neither is read once at boot.
type Account interface {
	// Read is the bearer to present, or an error that never reached the broker: a deployment's
	// own credential it could not read is never an empty bearer for the broker to refuse.
	Read() (string, error)
}

// AccountToken is the Account of a caller holding a projected Kubernetes ServiceAccount token in
// a file: Dispatch's own (DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE) and a listener's in a pod
// (ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE). The broker resolves the calling service from it
// (BROKER_API_ACCOUNTS). One reader serves every call a program makes — the credential-request
// UI's relay (internal/dispatch/cipher) and this package's introspection — so a projected
// token is cached once rather than once per caller.
//
// A projected token rotates, so the path is read at call time and never once at boot.
type AccountToken struct {
	// Setting names the setting that holds Path, as an error names the file: the setting's name,
	// never the path, which is where a token pasted in place of the path would print.
	Setting string
	// Path is the file the token is read from.
	Path string
	// TTL is how long Read reuses what it read, AccountTokenCacheTTL for every reader
	// NewAccountToken builds. A test that proves a rewritten file is picked up lapses the cache
	// by zeroing it.
	TTL time.Duration

	mu     sync.Mutex
	cached string
	readAt time.Time
}

// NewAccountToken is a reader of the projected ServiceAccount token at path, which the setting
// named setting holds and which Read opens at call time, caching what it reads for
// AccountTokenCacheTTL.
func NewAccountToken(setting, path string) *AccountToken {
	return &AccountToken{Setting: setting, Path: path, TTL: AccountTokenCacheTTL}
}

// AccountTokenError is a call that never reached the broker because Dispatch could not read its
// own ServiceAccount token from Path: the file is missing or unreadable, or holds nothing. It is
// its own type because what is wrong is Dispatch's deployment, not the broker, and a route must
// say so rather than send the operator to a broker that was never asked.
type AccountTokenError struct {
	// Setting names the setting that holds Path.
	Setting string
	// Path is the token file Dispatch tried to read.
	Path string
	// Err is why it could not: the read's own error, or errEmptyAccountToken.
	Err error
}

// Error names the file by the setting that holds it, and why it could not be read in fileerr's
// words, or that it is empty, never Path: the setting holds what a person wrote, and a token
// pasted in place of the path would print. A reader built with no setting names neither.
func (e *AccountTokenError) Error() string {
	head := "brokertoken: read the ServiceAccount token"
	if e.Setting != "" {
		head += " " + e.Setting + " names"
	}
	if errors.Is(e.Err, errEmptyAccountToken) {
		return head + ": " + e.Err.Error()
	}
	return head + ": its file cannot be read: " + fileerr.Class(e.Err)
}

func (e *AccountTokenError) Unwrap() error { return e.Err }

// errEmptyAccountToken is an AccountTokenError's reason when the file reads but holds nothing.
var errEmptyAccountToken = errors.New("the file is empty")

// Read is the bearer a call presents: Path's trimmed contents, read again once the copy it cached
// is older than TTL. A file that cannot be read, or that holds nothing, is an *AccountTokenError
// and no request at all, never an empty bearer for the broker to refuse: a projected token
// Dispatch cannot read is Dispatch's own misconfiguration. The error names the file's setting and
// why, never the path.
func (a *AccountToken) Read() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != "" && time.Since(a.readAt) < a.TTL {
		return a.cached, nil
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", &AccountTokenError{Setting: a.Setting, Path: a.Path, Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &AccountTokenError{Setting: a.Setting, Path: a.Path, Err: errEmptyAccountToken}
	}
	a.cached, a.readAt = token, time.Now()
	return token, nil
}
