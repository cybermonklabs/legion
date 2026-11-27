package repocache

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/compute/metadata"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestAMetadataRefusalPrintsNoCredentialAURLCarries: the metadata server refuses the bucket's
// read of its account (email) or of its token (token) 403 with a page that quotes a URL carrying a
// credential in its query. metadata's error quotes the page, and the bucket's error, which the
// cache logs, holds none of the credential: the account's read is read by the rule
// (oauth2call.Scope), and the token's by the token source the bucket's client asks
// (oauth2call.TokenSource) and then by the scope of the call that asked. The metadata request
// carries no credential of its own; the rule's reading of the page's URL keeps the one it quotes
// out. The twin shows metadata's own error holds it. Captures repocache.metadata-refusal/<case>.
func TestAMetadataRefusalPrintsNoCredentialAURLCarries(t *testing.T) {
	refusing := func(refused string) func(http.ResponseWriter, *http.Request, string) bool {
		return func(w http.ResponseWriter, _ *http.Request, route string) bool {
			if route != refused {
				return false
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "refused; see http://metadata.example/refusal?access_token="+wordsQueryToken)
			return true
		}
	}

	t.Run("twin", func(t *testing.T) {
		_, _ = googleStandIn(t, fakegcs.DefaultEmail, refusing("metadata-email"))
		_, err := metadata.NewWithOptions(&metadata.Options{}).EmailWithContext(testwait.Context(t), "default")
		if err == nil || !strings.Contains(err.Error(), wordsQueryToken) {
			t.Fatalf("metadata's own error of the refused read is %v, which holds no credential, so the rows cannot see one printed", err)
		}
	})
	t.Run("email", func(t *testing.T) {
		_, err := googleStandIn(t, fakegcs.DefaultEmail, refusing("metadata-email"))
		if err == nil {
			t.Fatal("the bucket opened with its account's read refused")
		}
		oauth2calltest.RequireNoPiece(t, "repocache.metadata-refusal/email", err.Error(), wordsQueryToken)
	})
	t.Run("token", func(t *testing.T) {
		b, err := googleStandIn(t, fakegcs.DefaultEmail, refusing("metadata-token"))
		if err != nil {
			t.Fatalf("open the bucket against the stand-in: %v", err)
		}
		_, _, err = b.list(testwait.Context(t))
		if err == nil {
			t.Fatal("the list succeeded with the token's read refused")
		}
		oauth2calltest.RequireNoPiece(t, "repocache.metadata-refusal/token", err.Error(), wordsQueryToken)
	})
}

// TestTheMetadataServersAccountPrintsByTheRule: the metadata server names the cache's account
// with user information before its '@' (user:password@...), words the rule does not print as they
// came. The bucket refuses the account at its open in words that name none of it, where it took
// it as the account its addresses are signed as and printed it in each refusal of a signature
// (sign as <account> through IAM Credentials). The twin shows metadata's answer holds both.
// Captures repocache.metadata-account.
func TestTheMetadataServersAccountPrintsByTheRule(t *testing.T) {
	account := wordsUser + ":" + wordsPassword + "@example-project.iam.gserviceaccount.com"
	refuseSigning := func(w http.ResponseWriter, _ *http.Request, route string) bool {
		if route != "signBlob" {
			return false
		}
		googleRefusal(w, route, http.StatusForbidden, "Permission 'iam.serviceAccounts.signBlob' denied on resource (or it may not exist).")
		return true
	}

	t.Run("twin", func(t *testing.T) {
		_, _ = googleStandIn(t, account, refuseSigning)
		email, err := metadata.NewWithOptions(&metadata.Options{}).EmailWithContext(testwait.Context(t), "default")
		if err != nil || !strings.Contains(email, wordsUser) || !strings.Contains(email, wordsPassword) {
			t.Fatalf("the metadata server named the account %q (%v), which holds no credential, so the row cannot see one printed", email, err)
		}
	})
	t.Run("account", func(t *testing.T) {
		b, err := googleStandIn(t, account, refuseSigning)
		if err == nil {
			_, err = b.sign(testwait.Context(t), packObject([]byte("a pack to sign"), "pack"))
		}
		if err == nil {
			t.Fatal("the bucket opened and signed as the account the metadata server named")
		}
		oauth2calltest.RequireNoPiece(t, "repocache.metadata-account", err.Error(), wordsUser, wordsPassword)
	})
}
