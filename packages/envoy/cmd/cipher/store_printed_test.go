// packages/envoy/cmd/cipher/store_printed_test.go
//
// What the secret forms print of the store's own answer and of the broker's settings. A store's
// refusal carries a message the store writes what it likes in, an echo of what the call sent among
// it: AWS's ValidationException quotes the field it refused, and Secret Manager's status message
// is the server's text. The broker's settings are text whoever answered AGENT_SECRETS_URL wrote,
// and beside an address that carries a credential that text can repeat it. So a refusal names the
// call and the code the store answered, and a settings field prints as the broker's address lets
// its answer print (cipheraddr.Address.Words).
package main

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// echoingList is a Secrets Manager whose ListSecrets refuses with code and message, which
// whoever answers at the store's address writes.
type echoingList struct {
	*awstest.Local
	code, message string
}

func (e echoingList) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return nil, &smithy.GenericAPIError{Code: e.code, Message: e.message}
}

// TestAStoresRefusalPrintsItsCodeNeverItsMessage: a refusal of AWS Secrets Manager's, and one of
// GCP Secret Manager's, whose message repeats a made-up secret, prints the call and the code the
// store answered with, and no part of the message but the permission it says is missing, where the
// message names one this client's calls need (awsMissingPermission, gcpMissingPermission): a
// person must know which permission to ask for, and such a permission's name is no secret. A text
// of a permission's shape that is no permission of this client's calls - a made-up secret, a
// 64-hex value of a SigV4 signature's shape - prints as "a permission this client does not name (not printed)". An AWS code prints
// only where it is one this client names (awsCodes): whoever answers at the store's address
// chooses it, so a code that is a made-up secret prints as "an AWS error". A listed code, a listed
// permission and a message with no permission in it are the controls: the check sees the store's
// answer.
func TestAStoresRefusalPrintsItsCodeNeverItsMessage(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useLentGCPAccessToken, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	planted := "hunter2pw-" + "storeecho-" + "4b1f"
	const arn = "arn:aws:iam::111122223333:user/"
	hex64 := strings.Repeat("a1b2c3d4", 4) + strings.Repeat("e5f6a7b8", 4)
	alnum, smuggled, letters := "Hunter2pw"+"ActionAlnum42", "Hunter2pw"+"Smuggled9Value", "Hunterpw"+"smuggledvalue"
	forbidden := []string{planted, "hunter2pw", "arn:aws:iam", "projects/1000", "validation error detected", "caller lacks access", "Access to KMS",
		hex64, alnum, smuggled, letters, "lower-svc"}
	unnamed := "Secrets Manager ListSecrets: AWS refused it: AccessDeniedException (missing permission: a permission this client does not name (not printed); AWS's message is not printed)"
	notAuthorized := func(action string) string {
		return "User: " + arn + "x is not authorized to perform: " + action + " because no identity-based policy allows it; echo: " + planted
	}
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
		{"AWS Secrets Manager, a made-up secret as the permission's service", "AccessDeniedException", notAuthorized("hunter2pw-" + "lower-svc:ListSecrets"), unnamed},
		{"AWS Secrets Manager, a 64-hex value as the permission's service", "AccessDeniedException", notAuthorized(hex64 + ":ListSecrets"), unnamed},
		{"AWS Secrets Manager, a made-up secret as the permission's action", "AccessDeniedException", notAuthorized("secretsmanager:" + alnum), unnamed},
		{"AWS Secrets Manager, another made-up secret as the permission's action", "AccessDeniedException", notAuthorized("secretsmanager:" + smuggled), unnamed},
		{"control: AWS Secrets Manager, a listed KMS permission", "AccessDeniedException", notAuthorized("kms:GenerateDataKey"),
			"Secrets Manager ListSecrets: AWS refused it: AccessDeniedException (missing permission: kms:GenerateDataKey; AWS's message is not printed)"},
		{"control: AWS Secrets Manager, a refusal that names no permission", "AccessDeniedException", "Access to KMS is not allowed; echo: " + planted,
			"Secrets Manager ListSecrets: AWS refused it: AccessDeniedException (AWS's message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := awstest.NewLocal(awstest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
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
		{"GCP Secret Manager, a made-up secret as the permission's resource",
			"Permission 'secretmanager." + letters + ".get' denied on resource 'projects/100000000001'; echo: " + planted,
			"Secret Manager ListSecrets answered PermissionDenied (missing permission: a permission this client does not name (not printed); its message is not printed)"},
		{"control: GCP Secret Manager, a listed Cloud KMS permission",
			"Permission 'cloudkms.cryptoKeyVersions.useToEncrypt' denied on resource 'projects/100000000001'; echo: " + planted,
			"Secret Manager ListSecrets answered PermissionDenied (missing permission: cloudkms.cryptoKeyVersions.useToEncrypt; its message is not printed)"},
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
			useGCPDialTo(t, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	// A create's cleanup deletes the empty secret it made (deleteOutright's DeleteSecret), which
	// needs secretmanager.secrets.delete: its refusal names that permission, through the
	// interceptor every Secret Manager call goes through.
	t.Run("GCP Secret Manager, a refused DeleteSecret", func(t *testing.T) {
		refused := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
			return status.Error(codes.PermissionDenied, "Permission 'secretmanager.secrets.delete' denied on resource 'projects/100000000001/secrets/x'; echo: "+planted)
		}
		err := (&gcpBackend{}).storeInterceptor(context.Background(), "/google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret", nil, nil, nil, refused)
		if err == nil {
			t.Fatal("control: the refused DeleteSecret returned no error, so the row is blind")
		}
		wantNamedNotPrinted(t, "the refusal", err.Error(),
			[]string{"Secret Manager DeleteSecret answered PermissionDenied (missing permission: secretmanager.secrets.delete; its message is not printed)"}, forbidden...)
	})
}

// TestTheBrokersSettingsPrintOnlyAsItsAddressLetsThem: a settings answer from an
// AGENT_SECRETS_URL that carries a credential prints none of its fields - an unknown store, an
// invalid KMS key ARN, a project that does not match its prefix - since whoever answers there can
// repeat the credential in any of them. Control: from a clean address each field prints, so the
// check sees the field.
func TestTheBrokersSettingsPrintOnlyAsItsAddressLetsThem(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at store_printed_test.go:179).
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
				wantNamedNotPrinted(t, "stderr", stderr, []string{row.says + cipheraddr.WordsNotPrinted}, planted, "demopass9z")
			})
		}
	}
}

// TestTheBrokersRereadAnswerPrintsOnlyAsItsAddressLetsIt: the reason a broker's reread refuses a
// secret for, and the type it says it serves the secret as, are text of its answer, so beside an
// AGENT_SECRETS_URL that carries a credential neither prints: the write's lines name them as
// cipheraddr.WordsNotPrinted. Control: beside a clean address both print as the broker wrote them.
func TestTheBrokersRereadAnswerPrintsOnlyAsItsAddressLetsIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at store_printed_test.go:222); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	planted := "hunter2pw-" + "reread-" + "7e5b"
	for _, row := range []struct {
		name   string
		answer func(name string) Reread
		says   []string
	}{
		{"a reason", func(name string) Reread { return Reread{Name: name, Reason: planted} }, []string{"(reason=%s)"}},
		{"a type", func(name string) Reread { return Reread{Name: name, Served: true, Type: planted} }, []string{"(type %s)", "serves HELD_KEY as %s, not a stored secret"}},
	} {
		for _, clean := range []bool{false, true} {
			name := row.name + ", beside a credentialed address"
			shown := cipheraddr.WordsNotPrinted
			if clean {
				name, shown = "control: "+row.name+", beside a clean address", planted
			}
			t.Run(name, func(t *testing.T) {
				local := awstest.NewLocal(awstest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
				startSecretBroker(t, row.answer)
				if !clean {
					t.Setenv("AGENT_SECRETS_URL", strings.Replace(brokerURLFromEnv(t), "http://", "http://demouser7q:demopass9z-not-a-secret@", 1))
				}
				useAWS(t, local, testAccount, adaSignIn)
				useStdin(t, "v2")
				stdout, stderr, code := runSecret("set", "HELD_KEY")
				if code != 1 {
					t.Fatalf("exit %d, stdout %q, stderr %q; want 1", code, stdout, stderr)
				}
				var want []string
				for _, says := range row.says {
					want = append(want, strings.ReplaceAll(says, "%s", shown))
				}
				forbidden := []string{"demopass9z"}
				if !clean {
					forbidden = append(forbidden, planted)
				}
				wantNamedNotPrinted(t, "the output", stdout+stderr, want, forbidden...)
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
