package onepassword

import (
	"context"
	"sync"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// Hooks are how the 1Password writer reaches 1Password, which a test swaps (SwapHooks).
type Hooks struct {
	// SDKClient starts the SDK signed in with a service account token (NewSDKClient).
	SDKClient func(ctx context.Context, token, vault string) (Client, error)
	// OpClient is the client that runs op (NewOpClient).
	OpClient func(vault string, run secretstore.RunChild) Client
	// Whoami reads whom op is signed in as (opWhoami).
	Whoami func(ctx context.Context, run secretstore.RunChild) (OpAccount, error)
	// Read, when set, is what every value read of the writers OpenWriter opens goes through: it
	// is handed the read and answers in its place, so a test can count value reads or refuse them.
	// A 1Password item's value comes with the item's read, so a value read is no call of its own.
	Read func(ctx context.Context, source string, read func(ctx context.Context, source string) (string, error)) (string, error)
}

var (
	hooksMu sync.Mutex
	hooks   = Hooks{SDKClient: NewSDKClient, OpClient: NewOpClient, Whoami: opWhoami}
)

// SwapHooks puts h's hooks in place of the ones OpenWriter opens with, each one h leaves nil
// keeping the hook in place, and answers restore, which puts the earlier hooks back.
func SwapHooks(h Hooks) (restore func()) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	before := hooks
	if h.SDKClient != nil {
		hooks.SDKClient = h.SDKClient
	}
	if h.OpClient != nil {
		hooks.OpClient = h.OpClient
	}
	if h.Whoami != nil {
		hooks.Whoami = h.Whoami
	}
	if h.Read != nil {
		hooks.Read = h.Read
	}
	return func() {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		hooks = before
	}
}

// CurrentHooks are the hooks OpenWriter opens with now, for a test hook that wraps the one in place.
func CurrentHooks() Hooks {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	return hooks
}
