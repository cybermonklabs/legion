package files_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	storage "google.golang.org/api/storage/v1"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// echoReplies are the answers a stand-in in a store's server's place gives a call it echoes the
// call's credential to: a redirect whose Location does not parse (unparsed), which net/http's error
// quotes whatever the client's redirect policy, since Go parses a Location before CheckRedirect is
// asked; a redirect the client follows, to a Location on a host nothing listens at that carries
// the credential in its query (elsewhere), which net/http's error for the followed request names;
// and a refusal whose words quote it (answer, which each test writes as its server would).
func echoReplies(t *testing.T, status int, answer func(w http.ResponseWriter, r *http.Request, credential string)) map[string]func(w http.ResponseWriter, r *http.Request, credential string) {
	dead := redirectstest.Dead(t)
	return map[string]func(w http.ResponseWriter, r *http.Request, credential string){
		"unparsed": func(w http.ResponseWriter, _ *http.Request, credential string) {
			w.Header().Set("Location", redirectstest.Unparsed(credential))
			w.WriteHeader(http.StatusFound)
		},
		"elsewhere": func(w http.ResponseWriter, _ *http.Request, credential string) {
			w.Header().Set("Location", redirectstest.To(dead, credential))
			w.WriteHeader(status)
		},
		"answer": answer,
	}
}

// requireNoEcho fails t where err, a store call's failure, is nil, holds a run of credential (under
// capture), or names the Location a stand-in wrote.
func requireNoEcho(t *testing.T, capture string, err error, credential string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call answered no error")
	}
	redacttest.RequireNoPiece(t, capture, err.Error(), credential)
	if strings.Contains(err.Error(), "moved") {
		t.Errorf("the error names the Location: %q", err)
	}
}

// TestAnS3CallPrintsNoEchoOfItsCredential: whatever answers at the bucket's address answers each
// call the S3 store makes, which carries the session token in X-Amz-Security-Token, with an echo of
// that token: an unparsed Location, a 307 elsewhere (the SDK's client follows a 307 and a 308), and
// a 403 whose request id and host id headers and message quote it. The store's error, which the
// version route and the health probe log and backfill-files prints, holds no run of the token and
// no word of the Location, and an answer's error still names its status. The SDK makes one attempt
// per call (AWS_MAX_ATTEMPTS=1): it retries a request that drew no answer after a random delay, and
// the health probe's two seconds can end inside that delay, where its error is the context's end
// and names nothing, so a row with retries could pass at the base. The twin shows the SDK's own
// error holds the echo. Captures files.s3-echo/<case>.
func TestAnS3CallPrintsNoEchoOfItsCredential(t *testing.T) {
	const token = "zq4mt-not-a-secret-RDX-7c1e"
	const sha = "4f3c2d1e0b9a8f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5"
	replies := echoReplies(t, http.StatusTemporaryRedirect, func(w http.ResponseWriter, _ *http.Request, credential string) {
		w.Header().Set("X-Amz-Request-Id", credential)
		w.Header().Set("X-Amz-Id-2", credential)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>denied to %s</Message></Error>`, credential)
	})
	// serve points the SDK at a stand-in that answers the call echoes picks with reply and every
	// other call as a bucket that holds no key would.
	serve := func(t *testing.T, echoes func(*http.Request) bool, reply func(http.ResponseWriter, *http.Request, string)) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case echoes(r):
				reply(w, r, r.Header.Get("X-Amz-Security-Token"))
			case r.Method == http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		t.Cleanup(server.Close)
		filestest.PointSDKAt(t, server.URL)
		t.Setenv("AWS_SESSION_TOKEN", token)
		t.Setenv("AWS_MAX_ATTEMPTS", "1")
	}
	bucketCall := func(r *http.Request) bool { return r.URL.Path == "/"+testBucket }
	t.Run("twin", func(t *testing.T) {
		serve(t, bucketCall, replies["unparsed"])
		cfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true }).HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String(testBucket)})
		if err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("the SDK's own error holds no echo of the session token, so the rows cannot see a leak: %v", err)
		}
	})
	for _, c := range []struct {
		name, reply string
		echoes      func(*http.Request) bool
		call        func(context.Context, *files.S3) error
	}{
		{"head-bucket/unparsed", "unparsed", bucketCall, func(ctx context.Context, s *files.S3) error { return s.Healthy(ctx) }},
		{"head-bucket/elsewhere", "elsewhere", bucketCall, func(ctx context.Context, s *files.S3) error { return s.Healthy(ctx) }},
		{"head-object/unparsed", "unparsed", func(r *http.Request) bool { return r.Method == http.MethodHead && r.URL.Path != "/"+testBucket }, func(ctx context.Context, s *files.S3) error {
			return s.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x"))
		}},
		{"put-object/unparsed", "unparsed", func(r *http.Request) bool { return r.Method == http.MethodPut }, func(ctx context.Context, s *files.S3) error {
			return s.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x"))
		}},
		{"get-object/unparsed", "unparsed", func(r *http.Request) bool { return r.Method == http.MethodGet }, func(ctx context.Context, s *files.S3) error {
			_, err := s.Get(ctx, sha)
			return err
		}},
		{"get-object/answer", "answer", func(r *http.Request) bool { return r.Method == http.MethodGet }, func(ctx context.Context, s *files.S3) error {
			_, err := s.Get(ctx, sha)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			serve(t, c.echoes, replies[c.reply])
			store, err := files.NewS3(context.Background(), testBucket)
			if err != nil {
				t.Fatalf("NewS3: %v", err)
			}
			err = c.call(context.Background(), store)
			requireNoEcho(t, "files.s3-echo/"+c.name, err, token)
			if c.reply == "answer" && !strings.Contains(err.Error(), "403") {
				t.Errorf("the error names no status: %q", err)
			}
		})
	}
}

// TestAGCSCallPrintsNoEchoOfItsBearer: whatever answers at GCS's address answers each call the GCS
// store makes, which carries the bearer, with an echo of it: an unparsed Location, a 302
// elsewhere (the client follows every redirect, and its transport puts the bearer on the followed
// request too), and a 403 whose JSON message, or a media download's plain-text body, quotes it.
// The store's error, which the version route and the health probe log and backfill-files prints,
// holds no run of the bearer and no word of the Location, and an answer's error still names its
// status. The twin shows the client's own error holds the echo. Captures files.gcs-echo/<case>.
func TestAGCSCallPrintsNoEchoOfItsBearer(t *testing.T) {
	const bearer = "kd7wp-not-a-secret-RDX-2b8a"
	const sha = "4f3c2d1e0b9a8f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5c4b3a2918f7e6d5"
	replies := echoReplies(t, http.StatusFound, func(w http.ResponseWriter, r *http.Request, credential string) {
		if r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "denied to %s", credential)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"code":403,"message":"denied to %s","errors":[{"message":"denied to %s","domain":"global","reason":"forbidden"}]}}`, credential, credential)
	})
	// serve answers the call echoes picks with reply and every other call as filestest's bucket
	// does, and answers the client options of a deployment's client pointed at it: the endpoint
	// and a bearer.
	serve := func(t *testing.T, echoes func(*http.Request) bool, reply func(http.ResponseWriter, *http.Request, string)) []option.ClientOption {
		fake := filestest.ServeGCS(t, testBucket)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if echoes(r) {
				reply(w, r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				return
			}
			fake.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		return []option.ClientOption{option.WithEndpoint(server.URL + "/storage/v1/"), option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: bearer}))}
	}
	metadata := func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/") && r.URL.Query().Get("alt") != "media"
	}
	media := func(r *http.Request) bool { return r.URL.Query().Get("alt") == "media" }
	t.Run("twin", func(t *testing.T) {
		service, err := storage.NewService(context.Background(), serve(t, metadata, replies["unparsed"])...)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Objects.Get(testBucket, files.Key(sha)).Do()
		if err == nil || !strings.Contains(err.Error(), bearer) {
			t.Fatalf("the client's own error holds no echo of the bearer, so the rows cannot see a leak: %v", err)
		}
	})
	put := func(ctx context.Context, s *files.GCS) error {
		return s.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x"))
	}
	get := func(ctx context.Context, s *files.GCS) error {
		_, err := s.Get(ctx, sha)
		return err
	}
	healthy := func(ctx context.Context, s *files.GCS) error { return s.Healthy(ctx) }
	for _, c := range []struct {
		name, reply string
		echoes      func(*http.Request) bool
		call        func(context.Context, *files.GCS) error
	}{
		{"metadata/unparsed", "unparsed", metadata, put},
		{"metadata/answer", "answer", metadata, put},
		{"insert/unparsed", "unparsed", func(r *http.Request) bool { return r.Method == http.MethodPost }, put},
		{"download/unparsed", "unparsed", media, get},
		{"download/answer", "answer", media, get},
		{"healthy/unparsed", "unparsed", metadata, healthy},
		{"healthy/elsewhere", "elsewhere", metadata, healthy},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, err := files.NewGCS(context.Background(), testBucket, serve(t, c.echoes, replies[c.reply])...)
			if err != nil {
				t.Fatalf("NewGCS: %v", err)
			}
			err = c.call(context.Background(), store)
			requireNoEcho(t, "files.gcs-echo/"+c.name, err, bearer)
			if c.reply == "answer" && !strings.Contains(err.Error(), "403") {
				t.Errorf("the error names no status: %q", err)
			}
		})
	}
}
