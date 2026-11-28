package gcp

import (
	"context"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc"
)

// Hooks are how the GCP writer reaches Google, which a test swaps (SwapHooks).
type Hooks struct {
	// CredentialsFile loads a credentials file (loadCredentialsFile).
	CredentialsFile func(ctx context.Context, path string) (*google.Credentials, error)
	// MetadataToken answers the machine's own credentials (metadataToken).
	MetadataToken func(ctx context.Context) (*google.Credentials, error)
	// MetadataPrincipal names the machine's own account, best effort (metadataPrincipal).
	MetadataPrincipal func(ctx context.Context) string
	// Userinfo reads the email of the account behind creds (userinfo).
	Userinfo func(ctx context.Context, creds *google.Credentials) (string, error)
	// ClientDial builds the Secret Manager client, every call through interceptor (dialClient).
	ClientDial func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error)
	// FindGcloud is the path of the gcloud a gcloud source runs (lookPathGcloud).
	FindGcloud func() (string, error)
}

// hooks are the Hooks every OpenWriter opens with.
var hooks = Hooks{
	CredentialsFile: loadCredentialsFile, MetadataToken: metadataToken, MetadataPrincipal: metadataPrincipal,
	Userinfo: userinfo, ClientDial: dialClient, FindGcloud: lookPathGcloud,
}

// SwapHooks puts h's hooks in place of the ones OpenWriter opens with, each one h leaves nil
// keeping the hook in place, and answers restore, which puts the earlier hooks back.
func SwapHooks(h Hooks) (restore func()) {
	before := hooks
	if h.CredentialsFile != nil {
		hooks.CredentialsFile = h.CredentialsFile
	}
	if h.MetadataToken != nil {
		hooks.MetadataToken = h.MetadataToken
	}
	if h.MetadataPrincipal != nil {
		hooks.MetadataPrincipal = h.MetadataPrincipal
	}
	if h.Userinfo != nil {
		hooks.Userinfo = h.Userinfo
	}
	if h.ClientDial != nil {
		hooks.ClientDial = h.ClientDial
	}
	if h.FindGcloud != nil {
		hooks.FindGcloud = h.FindGcloud
	}
	return func() { hooks = before }
}

// CurrentHooks are the hooks OpenWriter opens with now, for a test hook that wraps the one in place.
func CurrentHooks() Hooks { return hooks }
