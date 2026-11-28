package gcp

import (
	"context"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/grpc"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// HeldForForTest is gcpHeldFor, how long Read trusts a metadata verdict, for the tests outside the
// package that time it.
const HeldForForTest = gcpHeldFor

// MetadataAttemptsForTest is gcpMetadataAttempts, how many times a metadata call is made before an
// UNAVAILABLE answer is returned, for the tests outside the package that count them.
const MetadataAttemptsForTest = gcpMetadataAttempts

// SetClockForTest has s's held verdicts (Read's metadata cache) read the time from now.
func SetClockForTest(s Store, now func() time.Time) { s.held.now = now }

// The writer's internals its external tests call directly.
var (
	CredentialsFilePrincipal = credentialsFilePrincipal
	CredentialsFileRefusal   = credentialsFileRefusal
	LoadCredentialsFile      = loadCredentialsFile
	CallPermissions          = gcpCallPermissions
	Permissions              = gcpPermissions
)

// CheckSettingsForTest is checkSettingsKMSKeyName over the settings s.
func CheckSettingsForTest(s secretstore.Settings) error {
	return checkSettingsKMSKeyName(settingsOf(s))
}

// GcloudLoginsForTest is gcloudLogins (`gcloud auth list`) of the gcloud at path, run through run.
func GcloudLoginsForTest(ctx context.Context, path string, run secretstore.RunChild) error {
	_, _, err := gcloudLogins(ctx, gcloudCommand{path: path, run: run})
	return err
}

// GcloudSettingsForTest is readGcloudSettings (`gcloud config list`) of the gcloud at path, run
// through run.
func GcloudSettingsForTest(ctx context.Context, path string, run secretstore.RunChild) error {
	_, err := readGcloudSettings(ctx, gcloudCommand{path: path, run: run})
	return err
}

// RunGcloudForTest is runGcloud of the gcloud at path with args, run through run.
func RunGcloudForTest(ctx context.Context, path string, run secretstore.RunChild, args ...string) error {
	_, err := runGcloud(ctx, gcloudCommand{path: path, run: run}, args...)
	return err
}

// GcloudTokenForTest is the token source a gcloud source signs account in with (gcloudToken),
// the gcloud at path run through run.
func GcloudTokenForTest(ctx context.Context, path string, run secretstore.RunChild, account string) oauth2.TokenSource {
	return gcloudToken{ctx: ctx, gcloud: gcloudCommand{path: path, run: run}, account: account}
}

// InterceptForTest is the interceptor a writer's Secret Manager client calls through
// (storeInterceptor), for a writer that signed in with no credential.
func InterceptForTest(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return (&writer{}).storeInterceptor(ctx, method, req, reply, cc, invoker, opts...)
}
