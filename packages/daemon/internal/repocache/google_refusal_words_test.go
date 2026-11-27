package repocache

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestABucketRefusalPrintsNoBearerItEchoes: whatever answers in Cloud Storage's or IAM
// Credentials' place refuses a production bucket's request 403 with a message that quotes the
// bearer the request carried, the metadata server's token. googleapi's error prints that message,
// and the bucket's error, which the cache logs (listed_packs.go), holds no run of the bearer: each
// call is read by the rule with every credential its requests carried redacted (oauth2call.Scope,
// over the client's oauth2call.RefusalClient). The rows: sign (iamSigner.Sign, whose error sign's
// carries), list, read (put's read of the object), read-again (put's read of it after its upload
// lost to another, 412), upload and delete. The twin shows the Storage client's own error of a
// refused list holds the bearer, so the rows can see one printed. Captures
// repocache.google-refusal/<case>.
func TestABucketRefusalPrintsNoBearerItEchoes(t *testing.T) {
	content := randomContent(t, 16<<10)
	name := packObject(content, "pack")
	local := writeLocal(t, content)
	put := func(ctx context.Context, b *bucket) error {
		_, err := b.put(ctx, name, local)
		return err
	}
	notFound := func(w http.ResponseWriter) {
		googleRefusal(w, "read", http.StatusNotFound, "No such object: "+testBucket+"/"+name)
	}

	t.Run("twin", func(t *testing.T) {
		b, err := googleStandIn(t, fakegcs.DefaultEmail, refuseRoute("list"))
		if err != nil {
			t.Fatalf("open the bucket against the stand-in: %v", err)
		}
		_, err = b.handle.Objects(testwait.Context(t), &storage.Query{Prefix: b.prefix}).Next()
		if err == nil || !strings.Contains(err.Error(), wordsBearer) {
			t.Fatalf("the Storage client's own error of the refused list is %v, which holds no echo of the bearer, so the rows cannot see one printed", err)
		}
	})

	for _, c := range []struct {
		name string
		// answer makes the row's stand-in answer, fresh for each run.
		answer func() func(w http.ResponseWriter, r *http.Request, route string) bool
		call   func(ctx context.Context, b *bucket) error
	}{
		{"sign", func() func(http.ResponseWriter, *http.Request, string) bool { return refuseRoute("signBlob") }, func(ctx context.Context, b *bucket) error {
			_, err := b.sign(ctx, name)
			return err
		}},
		{"list", func() func(http.ResponseWriter, *http.Request, string) bool { return refuseRoute("list") }, func(ctx context.Context, b *bucket) error {
			_, _, err := b.list(ctx)
			return err
		}},
		{"read", func() func(http.ResponseWriter, *http.Request, string) bool { return refuseRoute("read") }, put},
		{"read-again", func() func(http.ResponseWriter, *http.Request, string) bool {
			var reads atomic.Int32
			return func(w http.ResponseWriter, r *http.Request, route string) bool {
				switch {
				case route == "read" && reads.Add(1) == 1:
					notFound(w)
				case route == "upload":
					googleRefusal(w, route, http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold.")
				case route == "read":
					echoRefusal(w, r, route)
				default:
					return false
				}
				return true
			}
		}, put},
		{"upload", func() func(http.ResponseWriter, *http.Request, string) bool {
			return func(w http.ResponseWriter, r *http.Request, route string) bool {
				switch route {
				case "read":
					notFound(w)
				case "upload":
					echoRefusal(w, r, route)
				default:
					return false
				}
				return true
			}
		}, put},
		{"delete", func() func(http.ResponseWriter, *http.Request, string) bool { return refuseRoute("delete") }, func(ctx context.Context, b *bucket) error {
			return b.delete(ctx, name, 1)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := googleStandIn(t, fakegcs.DefaultEmail, c.answer())
			if err != nil {
				t.Fatalf("open the bucket against the stand-in: %v", err)
			}
			err = c.call(testwait.Context(t), b)
			if err == nil {
				t.Fatal("the call the stand-in refused succeeded")
			}
			oauth2calltest.RequireNoPiece(t, "repocache.google-refusal/"+c.name, err.Error(), wordsBearer)
			if !strings.Contains(err.Error(), "403") {
				t.Errorf("the refusal %q names no status 403", err)
			}
		})
	}
}

// TestABucketCallFollowsNoRedirect: whatever answers in Cloud Storage's (list) or IAM Credentials'
// (sign) place answers a production bucket's request with a redirect to a stand-in on the same host
// name at another port (redirectstest.Sent), the Location quoting the bearer the request carried.
// oauth2.Transport sets the bearer on every request it sends, a redirect's among them, so a client
// that followed would send the stand-in the bearer whatever host the Location named. The stand-in
// is sent nothing, and the call fails naming the redirect's status: the bucket's client follows no
// redirect (redirects.Refused). Each call is bounded by 5 s, since a Storage client that followed
// retries the stand-in's 500 until its context ends. The twin shows a client that follows the
// redirect sends the stand-in the bearer. Captures repocache.redirect-followed/<case>.
func TestABucketCallFollowsNoRedirect(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	t.Run("twin-bearer", func(t *testing.T) {
		echo := redirectstest.EchoBearer(t, func(bearer string) string { return redirectstest.To(target, bearer) })
		request, err := http.NewRequest(http.MethodGet, echo+"/storage/v1/b/"+testBucket+"/o", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+wordsBearer)
		redirectstest.RequireSentOn(t, request, wordsBearer, sent)
	})
	for _, c := range []struct {
		name, route string
		call        func(ctx context.Context, b *bucket) error
	}{
		{"list", "list", func(ctx context.Context, b *bucket) error {
			_, _, err := b.list(ctx)
			return err
		}},
		{"sign", "signBlob", func(ctx context.Context, b *bucket) error {
			_, err := b.sign(ctx, packObject([]byte("a pack to sign"), "pack"))
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := googleStandIn(t, fakegcs.DefaultEmail, func(w http.ResponseWriter, r *http.Request, route string) bool {
				if route != c.route {
					return false
				}
				w.Header().Set("Location", redirectstest.To(target, echoedBearer(r)))
				w.WriteHeader(http.StatusFound)
				return true
			})
			if err != nil {
				t.Fatalf("open the bucket against the stand-in: %v", err)
			}
			ctx, cancel := context.WithTimeout(testwait.Context(t), 5*time.Second)
			defer cancel()
			err = c.call(ctx, b)
			text := sent()
			oauth2calltest.RequireNoPiece(t, "repocache.redirect-followed/"+c.name, text, wordsBearer)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), "302") {
				t.Errorf("the call answered %v, want it refused naming the status 302", err)
			}
		})
	}
}
