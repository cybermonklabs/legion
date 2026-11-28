// Each AWS call the writer makes, the sign-in's own STS calls among them, and the permission each
// needs: a refusal of that call that names its permission prints it, so a person knows which one
// to ask for. The table is the one operate.md gives.

package aws_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
)

// permissionRow is one permission some call the writer makes needs, with one such call, Secrets
// Manager's or STS's, which one chain words (awsWords). The refusal check reads every permission
// any call needs, so one row per permission tests it.
type permissionRow struct{ call, permission string }

// permissionRows are every permission the writer's calls need, once each, with a call that needs it.
var permissionRows = []permissionRow{
	{"ListSecrets", "secretsmanager:ListSecrets"},
	{"DescribeSecret", "secretsmanager:DescribeSecret"},
	{"GetSecretValue", "secretsmanager:GetSecretValue"},
	{"GetSecretValue", "kms:Decrypt"},
	{"CreateSecret", "secretsmanager:CreateSecret"},
	{"CreateSecret", "kms:GenerateDataKey"},
	{"PutSecretValue", "secretsmanager:PutSecretValue"},
	{"TagResource", "secretsmanager:TagResource"},
	{"UntagResource", "secretsmanager:UntagResource"},
	{"DeleteSecret", "secretsmanager:DeleteSecret"},
	{"RestoreSecret", "secretsmanager:RestoreSecret"},
	{"GetCallerIdentity", "sts:GetCallerIdentity"},
	{"AssumeRole", "sts:AssumeRole"},
	{"AssumeRoleWithWebIdentity", "sts:AssumeRoleWithWebIdentity"},
}

// refusedWords is what a form prints of a refusal of the row's call that names its permission,
// as AWS writes it, beside a made-up secret: AWS's AccessDenied, in AWS's words (STS's own "Not
// authorized to perform" for a web identity).
func refusedWords(row permissionRow, planted string) string {
	message := "User: arn:aws:iam::111122223333:user/x is not authorized to perform: " + row.permission + " on resource: x; echo: " + planted
	if row.call == "AssumeRoleWithWebIdentity" {
		message = "Not authorized to perform " + row.permission + "; echo: " + planted
	}
	return awsstore.Words(&smithy.OperationError{ServiceID: "AWS", OperationName: row.call,
		Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: message}}, "the call failed")
}

// TestEachCallsRefusalNamesItsPermission: a refusal that names a permission some call the writer
// makes needs prints that permission, and nothing else of the store's message. Control: a
// permission no call needs, in the same shape, prints as not named.
func TestEachCallsRefusalNamesItsPermission(t *testing.T) {
	planted := "hunter2pw-" + "permcall-" + "3a9e"
	for _, row := range permissionRows {
		t.Run("aws "+row.permission, func(t *testing.T) {
			wantNamedNotPrinted(t, "the refusal", refusedWords(row, planted), []string{"missing permission: " + row.permission + ";"}, planted)
		})
	}
	for _, row := range []permissionRow{
		{"ListSecrets", "secretsmanager:PutResourcePolicy"},
		{"GetCallerIdentity", "sts:TagSession"},
	} {
		t.Run("control: aws "+row.permission+", which no call needs", func(t *testing.T) {
			words := refusedWords(row, planted)
			if !strings.Contains(words, "missing permission: "+secretstore.PermissionNotNamed) || strings.Contains(words, row.permission) {
				t.Fatalf("the refusal %q names %s, which no call needs; want %q", words, row.permission, secretstore.PermissionNotNamed)
			}
		})
	}
}

// TestEveryCallTheFormsMakeHasItsPermissions ties each call the writer makes to the permissions it
// needs: every method of WriterAPI and STSAPI is an entry of awsCallPermissions, and the
// permissions those entries need are exactly permissionRows' permissions, one row each, which
// TestEachCallsRefusalNamesItsPermission checks a refusal of. So a call a later change adds fails
// here until its permissions are listed and a row checks them. Control: the walk found the calls
// it read (a count of zero is a walk that read nothing, and fails).
func TestEveryCallTheFormsMakeHasItsPermissions(t *testing.T) {
	calls := 0
	for _, api := range []reflect.Type{reflect.TypeFor[awsstore.WriterAPI](), reflect.TypeFor[awsstore.STSAPI]()} {
		for i := range api.NumMethod() {
			calls++
			if name := api.Method(i).Name; len(awsstore.CallPermissions[name]) == 0 {
				t.Errorf("%s.%s has no entry in awsCallPermissions", api.Name(), name)
			}
		}
	}
	if calls == 0 {
		t.Fatal("control: the walk found no AWS call, so it read nothing")
	}
	listed := map[string]bool{}
	for _, row := range permissionRows {
		if listed[row.permission] {
			t.Errorf("aws %s has two rows; one row per permission tests it", row.permission)
		}
		listed[row.permission] = true
	}
	for permission := range awsstore.Permissions {
		if !listed[permission] {
			t.Errorf("{aws %s} is a permission some call needs, but no row of permissionRows checks it", permission)
		}
	}
	for permission := range listed {
		if !awsstore.Permissions[permission] {
			t.Errorf("{aws %s} is a row of permissionRows, but no call the code lists needs it", permission)
		}
	}
}
