// packages/envoy/cmd/agent-secrets/store_printed_test.go
//
// What the secret forms print of the store's own answer. A store's refusal carries a message the
// store writes what it likes in, an echo of what the call sent among it: AWS's ValidationException
// quotes the field it refused, and Secret Manager's status message is the server's text. So a
// refusal names the call and the code the store answered.
package main

import (
	"context"
	"net"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// echoingList is a Secrets Manager whose ListSecrets refuses with a message that echoes planted.
type echoingList struct {
	*secrets.Local
	planted string
}

func (e echoingList) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "1 validation error detected: Value '" + e.planted + "' failed to satisfy constraint"}
}

// TestAStoresRefusalPrintsItsCodeNeverItsMessage: a refusal of AWS Secrets Manager's, and one of
// GCP Secret Manager's, whose message repeats a made-up secret, prints the call and the code the
// store answered with, and no part of the message. The code is the control: the check sees the
// store's answer.
func TestAStoresRefusalPrintsItsCodeNeverItsMessage(t *testing.T) {
	planted := "hunter2pw-" + "storeecho-" + "4b1f"
	t.Run("AWS Secrets Manager", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
		startSecretBroker(t, servedBy(local))
		useAWS(t, echoingList{Local: local, planted: planted}, testAccount, adaSignIn)
		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
		}
		wantNamedNotPrinted(t, "stderr", stderr, []string{"Secrets Manager ListSecrets: AWS refused it: ValidationException (AWS's message is not printed)"}, planted, "validation error detected")
	})
	t.Run("GCP Secret Manager", func(t *testing.T) {
		fake := newFakeSecretManager()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.UnaryInterceptor(func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
			return nil, status.Error(codes.PermissionDenied, "caller lacks access; echo: "+planted)
		}))
		secretmanagerpb.RegisterSecretManagerServiceServer(server, fake)
		go func() { _ = server.Serve(lis) }()
		t.Cleanup(server.Stop)
		restore := gcpClientDial
		gcpClientDial = func(ctx context.Context, _ *google.Credentials) (*secretmanager.Client, error) {
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, err
			}
			return secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
		}
		t.Cleanup(func() { gcpClientDial = restore })
		useNoADC(t)
		useNoGcloud(t)
		useLentGCPAccessToken(t, "lent-store-token", "ada@example.com")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
		}
		wantNamedNotPrinted(t, "stderr", stderr, []string{"Secret Manager ListSecrets answered PermissionDenied (its message is not printed)"}, planted, "caller lacks access")
	})
}
