// packages/envoy/cmd/agent-secrets/store_printed_test.go
//
// What the secret forms print of the store's own answer and of the broker's settings. A store's
// refusal carries a message the store writes what it likes in, an echo of what the call sent among
// it: AWS's ValidationException quotes the field it refused, and Secret Manager's status message
// is the server's text. The broker's settings are text whoever answered AGENT_SECRETS_URL wrote,
// and beside an address that carries a credential that text can repeat it. So a refusal names the
// call and the code the store answered, and a settings field prints as the broker's address lets
// its answer print (brokeraddr.Address.Words).
package main

import (
	"context"
	"net"
	"strconv"
	"strings"
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

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// echoingList is a Secrets Manager whose ListSecrets refuses with code and message, which
// whoever answers at the store's address writes.
type echoingList struct {
	*secrets.Local
	code, message string
}

func (e echoingList) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return nil, &smithy.GenericAPIError{Code: e.code, Message: e.message}
}

// TestAStoresRefusalPrintsItsCodeNeverItsMessage: a refusal of AWS Secrets Manager's, and one of
// GCP Secret Manager's, whose message repeats a made-up secret, prints the call and the code the
// store answered with, and no part of the message but the permission it says is missing, where the
// message names one in the shape AWS or Google writes it (awsMissingPermission,
// gcpMissingPermission): a person must know which permission to ask for, and a permission's name
// is no secret. An AWS code prints only where it is one this client names (awsCodes): whoever
// answers at the store's address chooses it, so a code that is a made-up secret prints as "an AWS
// error". A listed code and a message with no permission in it are the controls: the check sees
// the store's answer.
func TestAStoresRefusalPrintsItsCodeNeverItsMessage(t *testing.T) {
	planted := "hunter2pw-" + "storeecho-" + "4b1f"
	const arn = "arn:aws:iam::111122223333:user/"
	forbidden := []string{planted, "hunter2pw", "arn:aws:iam", "projects/1000", "validation error detected", "caller lacks access", "Access to KMS"}
	for _, row := range []struct {
		name, code, message, says string
	}{
		{"AWS Secrets Manager, a listed code", "ValidationException", "1 validation error detected: Value '" + planted + "' failed to satisfy constraint",
			"Secrets Manager ListSecrets: AWS refused it: ValidationException (AWS's message is not printed)"},
		{"AWS Secrets Manager, a code that is a made-up secret", "hunter2pwCodeEcho77", "echo: " + planted,
			"Secrets Manager ListSecrets: AWS refused it: an AWS error (its code is none this client names, and AWS's message is not printed)"},
		{"AWS Secrets Manager, a missing permission beside an ARN and a made-up secret", "AccessDeniedException",
			"User: " + arn + planted + " is not authorized to perform: secretsmanager:ListSecrets because no identity-based policy allows the secretsmanager:ListSecrets action; echo: " + planted,
			"Secrets Manager ListSecrets: AWS refused it: AccessDeniedException (missing permission: secretsmanager:ListSecrets; AWS's message is not printed)"},
		{"control: AWS Secrets Manager, a refusal that names no permission", "AccessDeniedException", "Access to KMS is not allowed; echo: " + planted,
			"Secrets Manager ListSecrets: AWS refused it: AccessDeniedException (AWS's message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
			startSecretBroker(t, servedBy(local))
			useAWS(t, echoingList{Local: local, code: row.code, message: row.message}, testAccount, adaSignIn)
			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.says}, forbidden...)
		})
	}
	for _, row := range []struct{ name, message, says string }{
		{"GCP Secret Manager, a missing permission beside a resource and a made-up secret",
			"Permission 'secretmanager.secrets.list' denied on resource 'projects/100000000001' (or it may not exist); echo: " + planted,
			"Secret Manager ListSecrets answered PermissionDenied (missing permission: secretmanager.secrets.list; its message is not printed)"},
		{"control: GCP Secret Manager, a refusal that names no permission", "caller lacks access; echo: " + planted,
			"Secret Manager ListSecrets answered PermissionDenied (its message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(grpc.UnaryInterceptor(func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
				return nil, status.Error(codes.PermissionDenied, row.message)
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
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.says}, forbidden...)
		})
	}
}

// TestTheBrokersSettingsPrintOnlyAsItsAddressLetsThem: a settings answer from an
// AGENT_SECRETS_URL that carries a credential prints none of its fields - an unknown store, an
// invalid KMS key ARN, a project that does not match its prefix - since whoever answers there can
// repeat the credential in any of them. Control: from a clean address each field prints, so the
// check sees the field.
func TestTheBrokersSettingsPrintOnlyAsItsAddressLetsThem(t *testing.T) {
	planted := "hunter2pw-" + "settings-" + "0d3a"
	for _, row := range []struct {
		name     string
		settings Settings
		says     string
	}{
		{"an unknown store", Settings{Store: planted, SecretsPrefix: policytest.Prefix}, "the broker's settings name an unknown secret store "},
		{"an invalid KMS key ARN", Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: planted, AWSAccountID: testAccount, AWSRegion: "us-east-1"}, "the broker's settings name an invalid KMS key ARN "},
		{"a project its prefix does not name", func() Settings {
			s := gcpTestSettings()
			s.GCPProject = planted
			return s
		}(), "the broker's settings name project "},
	} {
		for _, clean := range []bool{false, true} {
			name := row.name + ", beside a credentialed address"
			if clean {
				name = "control: " + row.name + ", beside a clean address"
			}
			t.Run(name, func(t *testing.T) {
				startSecretBrokerWith(t, row.settings, func(string) Reread { return Reread{} })
				if !clean {
					t.Setenv("AGENT_SECRETS_URL", strings.Replace(brokerURLFromEnv(t), "http://", "http://demouser7q:demopass9z-not-a-secret@", 1))
				}
				_, stderr, code := runSecret("list")
				if code == 0 {
					t.Fatalf("exit 0, stderr %q; want a refusal of the settings", stderr)
				}
				if clean {
					if !strings.Contains(stderr, row.says) || !strings.Contains(stderr, planted) {
						t.Fatalf("stderr %q does not name the field %q after %q", stderr, planted, row.says)
					}
					return
				}
				wantNamedNotPrinted(t, "stderr", stderr, []string{row.says + brokeraddr.WordsNotPrinted}, planted, "demopass9z")
			})
		}
	}
}

// brokerURLFromEnv is the AGENT_SECRETS_URL startSecretBrokerWith set, which must be a plain
// http URL the test can add a user and password to.
func brokerURLFromEnv(t *testing.T) string {
	t.Helper()
	raw := brokerBaseURL().Raw()
	if !strings.HasPrefix(raw, "http://") {
		t.Fatalf("AGENT_SECRETS_URL %s is no http URL", strconv.Quote(raw))
	}
	return raw
}
