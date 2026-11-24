package secrets

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAnAWSStoreCallPrintsNoEchoOfItsCredential: whatever answers at Secrets Manager's or KMS's
// address answers a call the broker's AWS store makes, which carries the session token in
// X-Amz-Security-Token, with an echo of it: a redirect whose Location does not parse (unparsed),
// which net/http's error quotes whatever the client's redirect policy, since Go parses a Location
// before CheckRedirect is asked, and a 400 whose request id header and message quote it (answer).
// The store's error, which the policy's reload, reread and value read log, holds no run of the
// token and no word of the Location, and the answer's error still names its status and code. The
// clients are the SDK's own, as the broker builds them. The twin shows the SDK's own error holds
// the echo. Captures secrets.aws-echo/<case>.
func TestAnAWSStoreCallPrintsNoEchoOfItsCredential(t *testing.T) {
	const token = "pr6vx-not-a-secret-RDX-1a4b"
	const keyARN = "arn:aws:kms:us-east-1:123456789012:key/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	unparsed := func(w http.ResponseWriter, credential string) {
		w.Header().Set("Location", redirectstest.Unparsed(credential))
		w.WriteHeader(http.StatusFound)
	}
	answer := func(w http.ResponseWriter, credential string) {
		w.Header().Set("X-Amzn-Requestid", credential)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"__type":"AccessDeniedException","message":"denied to %s"}`, credential)
	}
	// listed is a ListSecrets answer of one secret on an alias of the key, whose OnKey asks KMS.
	listed := func(w http.ResponseWriter, _ string) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		fmt.Fprint(w, `{"SecretList":[{"Name":"dev/agent-secrets/k","ARN":"arn:aws:secretsmanager:us-east-1:123456789012:secret:dev/agent-secrets/k","KmsKeyId":"alias/agent-secrets","SecretVersionsToStages":{"v1":["AWSCURRENT"]}}]}`)
	}
	// serve is the store over stand-ins of Secrets Manager and KMS that answer each call by its
	// X-Amz-Target with reply, with the credential the call carried.
	serve := func(t *testing.T, replies map[string]func(http.ResponseWriter, string)) AWS {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reply, ok := replies[r.Header.Get("X-Amz-Target")]
			if !ok {
				t.Errorf("the stand-in has no answer for %q", r.Header.Get("X-Amz-Target"))
				w.WriteHeader(http.StatusNotImplemented)
				return
			}
			reply(w, r.Header.Get("X-Amz-Security-Token"))
		}))
		t.Cleanup(server.Close)
		cfg := aws.Config{
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", token),
			BaseEndpoint: aws.String(server.URL),
		}
		return AWS{Client: secretsmanager.NewFromConfig(cfg), KMS: kms.NewFromConfig(cfg), KeyARN: keyARN}
	}
	t.Run("twin", func(t *testing.T) {
		store := serve(t, map[string]func(http.ResponseWriter, string){"secretsmanager.ListSecrets": unparsed})
		_, err := store.Client.ListSecrets(context.Background(), &secretsmanager.ListSecretsInput{})
		if err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("the SDK's own error holds no echo of the session token, so the rows cannot see a leak: %v", err)
		}
	})
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		replies map[string]func(http.ResponseWriter, string)
		call    func(*testing.T, AWS) error
	}{
		{"list/unparsed", map[string]func(http.ResponseWriter, string){"secretsmanager.ListSecrets": unparsed}, func(_ *testing.T, s AWS) error {
			_, err := s.List(ctx, "dev/agent-secrets/")
			return err
		}},
		{"describe/unparsed", map[string]func(http.ResponseWriter, string){"secretsmanager.DescribeSecret": unparsed}, func(_ *testing.T, s AWS) error {
			_, err := s.Describe(ctx, "dev/agent-secrets/k")
			return err
		}},
		{"read/unparsed", map[string]func(http.ResponseWriter, string){"secretsmanager.GetSecretValue": unparsed}, func(_ *testing.T, s AWS) error {
			_, err := s.Read(ctx, "dev/agent-secrets/k")
			return err
		}},
		{"read/answer", map[string]func(http.ResponseWriter, string){"secretsmanager.GetSecretValue": answer}, func(_ *testing.T, s AWS) error {
			_, err := s.Read(ctx, "dev/agent-secrets/k")
			return err
		}},
		{"aliases/unparsed", map[string]func(http.ResponseWriter, string){"secretsmanager.ListSecrets": listed, "TrentService.ListAliases": unparsed}, func(t *testing.T, s AWS) error {
			listings, err := s.List(ctx, "dev/agent-secrets/")
			if err != nil || len(listings) != 1 {
				t.Fatalf("List = %+v, %v; want the one secret", listings, err)
			}
			_, err = listings[0].OnKey(ctx)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(t, serve(t, c.replies))
			if err == nil {
				t.Fatal("the call answered no error")
			}
			redacttest.RequireNoPiece(t, "secrets.aws-echo/"+c.name, err.Error(), token)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the store's error names the Location: %q", err)
			}
			if c.name == "read/answer" && (!strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "AccessDeniedException")) {
				t.Errorf("the store's error names no status and code: %q", err)
			}
		})
	}
}
