package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Hooks are how the AWS writer reaches AWS, which a test swaps (SwapHooks).
type Hooks struct {
	// Clients loads the sign-in the command's environment gives, pinned to region, and answers the
	// Secrets Manager and STS clients over it and the credential sources the SDK resolved it from
	// (loadClients).
	Clients func(ctx context.Context, region string) (WriterAPI, STSAPI, []aws.CredentialSource, error)
}

// hooks are the Hooks every OpenWriter opens with.
var hooks = Hooks{Clients: loadClients}

// SwapHooks puts h's hooks in place of the ones OpenWriter opens with, each one h leaves nil
// keeping the hook in place, and answers restore, which puts the earlier hooks back.
func SwapHooks(h Hooks) (restore func()) {
	before := hooks
	if h.Clients != nil {
		hooks.Clients = h.Clients
	}
	return func() { hooks = before }
}

// CurrentHooks are the hooks OpenWriter opens with now, for a test hook that wraps the one in place.
func CurrentHooks() Hooks { return hooks }
