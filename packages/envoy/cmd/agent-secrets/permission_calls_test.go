// packages/envoy/cmd/agent-secrets/permission_calls_test.go
//
// Each AWS and GCP call the secret forms make, the sign-in's own STS calls among them, and the
// permission each needs: a refusal of that call that names its permission prints it, so a person
// knows which one to ask for. The table is the one the pull request and operate.md give.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// callPermission is one call a form makes and one permission it needs: store is "aws" for a
// Secrets Manager call, "sts" for an STS call (the sign-in's), or "gcp" for a Secret Manager call.
type callPermission struct{ store, call, permission string }

// callPermissions are every call the forms make and every permission each needs. A GCP call that
// needs none, TestIamPermissions, has no row.
var callPermissions = []callPermission{
	{"aws", "ListSecrets", "secretsmanager:ListSecrets"},
	{"aws", "DescribeSecret", "secretsmanager:DescribeSecret"},
	{"aws", "GetSecretValue", "secretsmanager:GetSecretValue"},
	{"aws", "GetSecretValue", "kms:Decrypt"},
	{"aws", "CreateSecret", "secretsmanager:CreateSecret"},
	{"aws", "CreateSecret", "secretsmanager:TagResource"},
	{"aws", "CreateSecret", "kms:GenerateDataKey"},
	{"aws", "CreateSecret", "kms:Decrypt"},
	{"aws", "PutSecretValue", "secretsmanager:PutSecretValue"},
	{"aws", "PutSecretValue", "kms:GenerateDataKey"},
	{"aws", "PutSecretValue", "kms:Decrypt"},
	{"aws", "TagResource", "secretsmanager:TagResource"},
	{"aws", "UntagResource", "secretsmanager:UntagResource"},
	{"aws", "DeleteSecret", "secretsmanager:DeleteSecret"},
	{"aws", "RestoreSecret", "secretsmanager:RestoreSecret"},
	{"sts", "GetCallerIdentity", "sts:GetCallerIdentity"},
	{"sts", "AssumeRole", "sts:AssumeRole"},
	{"sts", "AssumeRoleWithWebIdentity", "sts:AssumeRoleWithWebIdentity"},
	{"gcp", "ListSecrets", "secretmanager.secrets.list"},
	{"gcp", "GetSecret", "secretmanager.secrets.get"},
	{"gcp", "CreateSecret", "secretmanager.secrets.create"},
	{"gcp", "UpdateSecret", "secretmanager.secrets.update"},
	{"gcp", "DeleteSecret", "secretmanager.secrets.delete"},
	{"gcp", "AddSecretVersion", "secretmanager.versions.add"},
	{"gcp", "AddSecretVersion", "cloudkms.cryptoKeyVersions.useToEncrypt"},
	{"gcp", "AccessSecretVersion", "secretmanager.versions.access"},
	{"gcp", "AccessSecretVersion", "cloudkms.cryptoKeyVersions.useToDecrypt"},
	{"gcp", "GetSecretVersion", "secretmanager.versions.get"},
	{"gcp", "ListSecretVersions", "secretmanager.versions.list"},
}

// refusedWords is what a form prints of a refusal of call that names permission, as AWS or
// Google writes it, beside a made-up secret: AWS's AccessDenied for a Secrets Manager or STS call
// (STS's own "Not authorized to perform" for a web identity), and Secret Manager's PermissionDenied
// through the interceptor every call goes through.
func refusedWords(t *testing.T, row callPermission, planted string) string {
	t.Helper()
	switch row.store {
	case "aws":
		message := "User: arn:aws:iam::111122223333:user/x is not authorized to perform: " + row.permission + " on resource: arn:aws:secretsmanager:us-east-1:111122223333:secret:x; echo: " + planted
		return awsStoreFailure(row.call, &smithy.OperationError{ServiceID: secretsmanager.ServiceID, OperationName: row.call,
			Err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: message}}).Error()
	case "sts":
		message := "User: arn:aws:iam::111122223333:user/x is not authorized to perform: " + row.permission + " on resource: arn:aws:iam::111122223333:role/agent; echo: " + planted
		if row.call == "AssumeRoleWithWebIdentity" {
			message = "Not authorized to perform " + row.permission + "; echo: " + planted
		}
		return awsSDKWords(&smithy.OperationError{ServiceID: sts.ServiceID, OperationName: row.call,
			Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: message}})
	}
	refused := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return status.Error(codes.PermissionDenied, "Permission '"+row.permission+"' denied on resource 'projects/100000000001/secrets/x'; echo: "+planted)
	}
	err := (&gcpBackend{}).storeInterceptor(context.Background(), "/google.cloud.secretmanager.v1.SecretManagerService/"+row.call, nil, nil, nil, refused)
	if err == nil {
		t.Fatal("control: the refused call returned no error, so the row is blind")
	}
	return err.Error()
}

// TestEachCallsRefusalNamesItsPermission: a refusal of each call the forms make that names a
// permission the call needs prints that permission, and nothing else of the store's message.
// Control: a permission no call needs, in the same shape, prints as not named.
func TestEachCallsRefusalNamesItsPermission(t *testing.T) {
	planted := "hunter2pw-" + "permcall-" + "3a9e"
	for _, row := range callPermissions {
		t.Run(row.store+" "+row.call+" needs "+row.permission, func(t *testing.T) {
			wantNamedNotPrinted(t, "the refusal", refusedWords(t, row, planted), []string{"missing permission: " + row.permission + ";"}, planted)
		})
	}
	for _, row := range []callPermission{
		{"aws", "ListSecrets", "secretsmanager:PutResourcePolicy"},
		{"sts", "GetCallerIdentity", "sts:TagSession"},
		{"gcp", "ListSecrets", "secretmanager.secrets.setIamPolicy"},
	} {
		t.Run("control: "+row.store+" "+row.permission+", which no call needs", func(t *testing.T) {
			words := refusedWords(t, row, planted)
			if !strings.Contains(words, "missing permission: "+permissionNotNamed) || strings.Contains(words, row.permission) {
				t.Fatalf("the refusal %q names %s, which no call needs; want %q", words, row.permission, permissionNotNamed)
			}
		})
	}
}
