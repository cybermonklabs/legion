// packages/envoy/cmd/agent-secrets/secret_gcp_failure.go
//
// How the GCP forms word a Secret Manager call that failed: one gRPC interceptor every call goes
// through, which names the call and the code Secret Manager answered, and the permission it says is
// missing, never the rest of its message, which whoever answers writes, or the class of the
// failure of a call that did not reach Secret Manager; and the token source the client signs with,
// which keeps its last failure for an Unauthenticated it caused.
package main

import (
	"context"
	"path"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/redact"
)

// storeInterceptor is the gRPC interceptor every Secret Manager call this client makes goes
// through (gcpClientDial installs it): a call's failure is storeFailure's, named by the call's
// method, so no call site wraps a refusal by hand, and a call a later change adds cannot forget
// to.
func (b *gcpBackend) storeInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return b.storeFailure(path.Base(method), invoker(ctx, method, req, reply, cc, opts...))
}

// storeFailure is err, the failure of the Secret Manager call method, as a message prints it: the
// call and the gRPC code Secret Manager answered, and the permission its message says is missing,
// where it names one this client's calls need (gcpMissingPermission), never the rest of its
// message, which the server writes what it likes in, an echo of what the call sent among it; a
// call that did not reach Secret Manager by the class of gRPC's own failure (storeUnreached); and a
// failure that is no status at all by its class (redact.Class). An Unauthenticated whose cause was
// this client's own token source failing prints that failure (storeTokens): gRPC writes it into
// the status message, and its words are this program's own (redactedTokens, gcloudToken,
// googleidentity). A failure with a status is a storeRefusal, whose status holds the call's code
// and these words alone, so status.Code, isAccessDenied, gcpDefiniteRefusal and gax's retries still
// read the code, and gax, which builds its APIError from that status and prints the status's
// details beside its text, prints none of the server's.
func (b *gcpBackend) storeFailure(method string, err error) error {
	if err == nil {
		return nil
	}
	call := "Secret Manager " + method
	answer, ok := status.FromError(err)
	if !ok {
		return redact.Masked(err, call+" failed: "+redact.Class(err).Error())
	}
	words := call + " answered " + answer.Code().String() + " (its message is not printed)"
	if class, unreached := storeUnreached(answer); unreached {
		words = call + " failed: it did not reach Secret Manager (" + class + ")"
	} else if answer.Code() == codes.Unauthenticated && b.tokens.failure() != nil {
		words = call + " answered Unauthenticated: " + b.tokens.failure().Error()
	} else if permission := gcpMissingPermission(answer.Message()); permission != "" {
		words = call + " answered " + answer.Code().String() + " (missing permission: " + permission + "; its message is not printed)"
	}
	return &storeRefusal{words: words, status: status.New(answer.Code(), words), err: err}
}

// x509UnknownAuthority is the x509 package's words for a certificate no root this machine trusts
// signed. gax stops retrying a call whose error says them (gax-go's invoke), since a CA missing
// here does not appear by waiting, so a call that failed so keeps them in its words.
const x509UnknownAuthority = "x509: certificate signed by unknown authority"

// storeUnreached is the class of a call's failure where the status is gRPC's own, Unavailable for
// a call that never reached Secret Manager, and whether it is one: gRPC words a failed connection
// "connection error: desc = ...", and a name its resolver could not turn into an address "name
// resolver error: ...". Its text names the address it dialed and the system's reason, so it prints
// as fixed words: a TLS handshake that failed (with x509UnknownAuthority where the text says it), a
// name that did not resolve, a connection that could not be made, or any other connection that
// failed. A status Secret Manager answered has text of its own and is no class here.
func storeUnreached(answer *status.Status) (string, bool) {
	text := answer.Message()
	switch {
	case answer.Code() != codes.Unavailable:
		return "", false
	case strings.HasPrefix(text, "name resolver error: "):
		return "the name did not resolve", true
	case !strings.HasPrefix(text, "connection error: desc = "):
		return "", false
	case strings.Contains(text, "transport: authentication handshake failed"):
		if strings.Contains(text, x509UnknownAuthority) {
			return "the TLS handshake failed: " + x509UnknownAuthority, true
		}
		return "the TLS handshake failed", true
	case strings.Contains(text, "no such host"):
		return "the name did not resolve", true
	case strings.Contains(text, "transport: Error while dialing"):
		return "could not connect", true
	}
	return "the connection failed", true
}

// storeRefusal is a Secret Manager call's failure as storeFailure words it: Error is the words,
// GRPCStatus a status of the call's code and those words with no details, and Unwrap the
// refusal itself.
type storeRefusal struct {
	words  string
	status *status.Status
	err    error
}

func (e *storeRefusal) Error() string              { return e.words }
func (e *storeRefusal) Unwrap() error              { return e.err }
func (e *storeRefusal) GRPCStatus() *status.Status { return e.status }

// gcpPermission is how Google's message names the IAM permission a refusal lacks: "Permission
// '<service>.<resource>.<verb>' denied", dotted words of letters.
var gcpPermission = regexp.MustCompile(`Permission '([a-z]{1,64}(?:\.[a-zA-Z]{1,64}){1,8})' denied`)

// gcpPermissions are the IAM permissions a message names by name (gcpMissingPermission): the ones
// the forms' calls need - Secret Manager's, and the agent-secrets key's Cloud KMS ones - and no other.
var gcpPermissions = map[string]bool{
	"secretmanager.secrets.list": true, "secretmanager.secrets.get": true, "secretmanager.secrets.create": true,
	"secretmanager.secrets.update": true, "secretmanager.secrets.delete": true, "secretmanager.versions.access": true,
	"secretmanager.versions.add": true, "secretmanager.versions.get": true, "secretmanager.versions.list": true,
	"cloudkms.cryptoKeyVersions.useToEncrypt": true, "cloudkms.cryptoKeyVersions.useToDecrypt": true,
}

// gcpMissingPermission is the IAM permission message, Google's own text, says is denied, where it
// names one in Google's shape (gcpPermission), else "": the permission where it is one this
// client's calls need (gcpPermissions), else permissionNotNamed. Nothing else of the message
// prints: it names the resource, and whoever answers at the store's address writes it, a text of a
// permission's shape among it; but the name of a permission this client needs is no secret, and a
// person must know which one to ask for.
func gcpMissingPermission(message string) string {
	match := gcpPermission.FindStringSubmatch(message)
	switch {
	case match == nil:
		return ""
	case gcpPermissions[match[1]]:
		return match[1]
	}
	return permissionNotNamed
}

// storeTokens is the token source the Secret Manager client signs its calls with, which keeps its
// last failure (failure) so a call gRPC refuses as Unauthenticated can say why in this program's
// own words (storeFailure). A token it fetches clears the failure.
type storeTokens struct {
	oauth2.TokenSource
	mu   sync.Mutex
	last error
}

func (s *storeTokens) Token() (*oauth2.Token, error) {
	token, err := s.TokenSource.Token()
	s.mu.Lock()
	s.last = err
	s.mu.Unlock()
	return token, err
}

// failure is the last failure of s's token source, nil after a token it fetched or for no s.
func (s *storeTokens) failure() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}
