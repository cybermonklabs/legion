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
	"slices"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/redact"
)

// storeInterceptor is the gRPC interceptor every Secret Manager call this client makes goes
// through (gcpClientDial installs it): a call's failure is storeFailure's, named by the call's
// method, so no call site wraps a refusal by hand, and a call a later change adds cannot forget
// to. It asks gRPC for the call's peer (grpc.Peer), which gRPC fills only once the call went out
// on a connection it made, so storeFailure knows whether anything answered.
func (b *gcpBackend) storeInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	var answered peer.Peer
	err := invoker(ctx, method, req, reply, cc, append(slices.Clip(opts), grpc.Peer(&answered))...)
	return b.storeFailure(path.Base(method), answered.Addr != nil, err)
}

// storeFailure is err, the failure of the Secret Manager call method, as a message prints it: the
// call and the gRPC code Secret Manager answered, and the permission its message says is missing,
// where it names one this client's calls need (gcpMissingPermission), never the rest of its
// message, which the server writes what it likes in, an echo of what the call sent among it; a
// call that did not reach Secret Manager by the class of gRPC's own failure (storeUnreached); and a
// failure that is no status at all by its class (redact.Class). reached is whether the call went
// out on a connection (storeInterceptor). An Unauthenticated whose cause was this client's own
// token source failing prints that failure (storeTokens): gRPC writes it into the status message,
// and its words are this program's own (redactedTokens, gcloudToken, googleidentity). A failure
// with a status is a storeRefusal, whose status holds the call's code and these words alone, so
// status.Code, isAccessDenied, gcpDefiniteRefusal and gax's retries still read the code, and gax,
// which builds its APIError from that status and prints the status's details beside its text,
// prints none of the server's.
func (b *gcpBackend) storeFailure(method string, reached bool, err error) error {
	if err == nil {
		return nil
	}
	call := "Secret Manager " + method
	answer, ok := status.FromError(err)
	if !ok {
		return redact.Masked(err, call+" failed: "+redact.Class(err).Error())
	}
	words := call + " answered " + answer.Code().String() + " (its message is not printed)"
	if class, unreached := storeUnreached(answer, reached); unreached {
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

// storeUnreached is the class of a call's failure where the call never went out on a connection
// (reached false) and gRPC failed it Unavailable, and whether it is one. Whether anything answered
// is the transport's own fact, the call's peer (storeInterceptor), never the status's text: a
// server can answer Unavailable with any words, gRPC's own among them, and an answer is no call
// that did not reach Secret Manager. gRPC hands the client no error but that text for a
// connection it could not make - its picker (picker_wrapper.go) and its transport's error mapping
// (rpc_util.go) turn the dial's error into a status of code Unavailable and that error's text,
// with no details and no cause - so the class is read from the text, which only gRPC itself wrote
// where nothing was reached: "connection error: desc = ..." for a failed connection and "name
// resolver error: ..." for a name its resolver could not turn into an address. That text names the
// address it dialed and the system's reason, so it prints as the class redact.Class gives the same
// failure of any other request, in its words: redact.ClassTLS (with x509UnknownAuthority where the
// text says it), redact.ClassNameResolution, redact.ClassRefused, redact.ClassTimedOut, or
// redact.ClassRequestFailed for any other connection that failed. A connection that broke after
// the call went out on it has a peer, and reads as Secret Manager's own Unavailable.
func storeUnreached(answer *status.Status, reached bool) (string, bool) {
	text := answer.Message()
	switch {
	case reached || answer.Code() != codes.Unavailable:
		return "", false
	case strings.HasPrefix(text, "name resolver error: "):
		return redact.ClassNameResolution, true
	case !strings.HasPrefix(text, "connection error: desc = "):
		return "", false
	case strings.Contains(text, "transport: authentication handshake failed"):
		if strings.Contains(text, x509UnknownAuthority) {
			return redact.ClassTLS + ": " + x509UnknownAuthority, true
		}
		return redact.ClassTLS, true
	case strings.Contains(text, "no such host"):
		return redact.ClassNameResolution, true
	case strings.Contains(text, "connection refused"):
		return redact.ClassRefused, true
	case strings.Contains(text, "i/o timeout"):
		return redact.ClassTimedOut, true
	}
	return redact.ClassRequestFailed, true
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

// gcpCallPermissions are the IAM permissions each Secret Manager call this client's forms make
// needs, by the call's method: every method the forms call on the Secret Manager client. An
// AddSecretVersion and an AccessSecretVersion also use the agent-secrets key, through Secret
// Manager's own service agent, not the caller: its refusal names that agent's missing Cloud KMS
// permission, which the key's grant gives. TestIamPermissions needs no permission: it answers which
// of the ones it names the caller holds. TestEveryCallTheFormsMakeHasItsPermissions holds that every
// method the forms call is here.
var gcpCallPermissions = map[string][]string{
	"ListSecrets":         {"secretmanager.secrets.list"},
	"GetSecret":           {"secretmanager.secrets.get"},
	"CreateSecret":        {"secretmanager.secrets.create"},
	"UpdateSecret":        {"secretmanager.secrets.update"},
	"DeleteSecret":        {"secretmanager.secrets.delete"},
	"AddSecretVersion":    {"secretmanager.versions.add", "cloudkms.cryptoKeyVersions.useToEncrypt"},
	"AccessSecretVersion": {"secretmanager.versions.access", "cloudkms.cryptoKeyVersions.useToDecrypt"},
	"GetSecretVersion":    {"secretmanager.versions.get"},
	"ListSecretVersions":  {"secretmanager.versions.list"},
	"TestIamPermissions":  nil,
}

// gcpPermissions are the IAM permissions a message names by name (gcpMissingPermission): every one
// some call of gcpCallPermissions needs, and no other.
var gcpPermissions = permissionsOf(gcpCallPermissions)

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
