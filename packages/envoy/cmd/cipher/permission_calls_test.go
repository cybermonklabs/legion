// packages/envoy/cmd/cipher/permission_calls_test.go
//
// Each AWS and GCP call the secret forms make, the sign-in's own STS calls among them, and the
// permission each needs: a refusal of that call that names its permission prints it, so a person
// knows which one to ask for. The table is the one the pull request and operate.md give.
package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// permissionRow is one permission some call the forms make needs, with one such call: store is
// "aws" for an AWS call, Secrets Manager's or STS's, which one chain words (awsWords), and "gcp" for
// a Secret Manager call. The refusal check reads every permission any call needs, so one row per
// permission tests it.
type permissionRow struct{ store, call, permission string }

// permissionRows are every permission the forms' calls need, once each, with a call that needs it.
var permissionRows = []permissionRow{
	{"aws", "ListSecrets", "secretsmanager:ListSecrets"},
	{"aws", "DescribeSecret", "secretsmanager:DescribeSecret"},
	{"aws", "GetSecretValue", "secretsmanager:GetSecretValue"},
	{"aws", "GetSecretValue", "kms:Decrypt"},
	{"aws", "CreateSecret", "secretsmanager:CreateSecret"},
	{"aws", "CreateSecret", "kms:GenerateDataKey"},
	{"aws", "PutSecretValue", "secretsmanager:PutSecretValue"},
	{"aws", "TagResource", "secretsmanager:TagResource"},
	{"aws", "UntagResource", "secretsmanager:UntagResource"},
	{"aws", "DeleteSecret", "secretsmanager:DeleteSecret"},
	{"aws", "RestoreSecret", "secretsmanager:RestoreSecret"},
	{"aws", "GetCallerIdentity", "sts:GetCallerIdentity"},
	{"aws", "AssumeRole", "sts:AssumeRole"},
	{"aws", "AssumeRoleWithWebIdentity", "sts:AssumeRoleWithWebIdentity"},
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

// refusedWords is what a form prints of a refusal of the row's call that names its permission,
// as AWS or Google writes it, beside a made-up secret: AWS's AccessDenied, in AWS's words (STS's
// own "Not authorized to perform" for a web identity), and Secret Manager's PermissionDenied
// through the interceptor every call goes through.
func refusedWords(t *testing.T, row permissionRow, planted string) string {
	t.Helper()
	if row.store == "aws" {
		message := "User: arn:aws:iam::111122223333:user/x is not authorized to perform: " + row.permission + " on resource: x; echo: " + planted
		if row.call == "AssumeRoleWithWebIdentity" {
			message = "Not authorized to perform " + row.permission + "; echo: " + planted
		}
		return awsWords(&smithy.OperationError{ServiceID: "AWS", OperationName: row.call,
			Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: message}}, "the call failed")
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

// TestEachCallsRefusalNamesItsPermission: a refusal that names a permission some call the forms
// make needs prints that permission, and nothing else of the store's message. Control: a
// permission no call needs, in the same shape, prints as not named.
func TestEachCallsRefusalNamesItsPermission(t *testing.T) {
	planted := "hunter2pw-" + "permcall-" + "3a9e"
	for _, row := range permissionRows {
		t.Run(row.store+" "+row.permission, func(t *testing.T) {
			wantNamedNotPrinted(t, "the refusal", refusedWords(t, row, planted), []string{"missing permission: " + row.permission + ";"}, planted)
		})
	}
	for _, row := range []permissionRow{
		{"aws", "ListSecrets", "secretsmanager:PutResourcePolicy"},
		{"aws", "GetCallerIdentity", "sts:TagSession"},
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

// TestEveryCallTheFormsMakeHasItsPermissions ties each call the forms make to the permissions it
// needs: every method of secretsAPI and stsAPI, and every method the package's code calls on the
// Secret Manager client (a `.client.<Method>(` in a non-test file), is an entry of
// awsCallPermissions or gcpCallPermissions, and the permissions those entries need are exactly
// permissionRows' permissions, one row each, which TestEachCallsRefusalNamesItsPermission checks a
// refusal of. So a call a later
// change adds fails here until its permissions are listed and a row checks them. Control: the
// walks found the calls they read (a count of zero is a walk that read nothing, and fails).
func TestEveryCallTheFormsMakeHasItsPermissions(t *testing.T) {
	awsCalls := 0
	for _, api := range []reflect.Type{reflect.TypeFor[secretsAPI](), reflect.TypeFor[stsAPI]()} {
		for i := range api.NumMethod() {
			awsCalls++
			if name := api.Method(i).Name; len(awsCallPermissions[name]) == 0 {
				t.Errorf("%s.%s has no entry in awsCallPermissions", api.Name(), name)
			}
		}
	}
	called := regexp.MustCompile(`\.client\.([A-Z][A-Za-z]*)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gcpCalls := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range called.FindAllSubmatch(src, -1) {
			if name := string(m[1]); name != "Close" {
				gcpCalls++
				if _, ok := gcpCallPermissions[name]; !ok {
					t.Errorf("%s calls the Secret Manager client's %s, which has no entry in gcpCallPermissions", file, name)
				}
			}
		}
	}
	if awsCalls == 0 || gcpCalls == 0 {
		t.Fatalf("control: the walks found %d AWS and %d Secret Manager calls, so a walk read nothing", awsCalls, gcpCalls)
	}
	type stored struct{ store, permission string }
	listed := map[stored]bool{}
	for _, row := range permissionRows {
		if listed[stored{row.store, row.permission}] {
			t.Errorf("%s %s has two rows; one row per permission tests it", row.store, row.permission)
		}
		listed[stored{row.store, row.permission}] = true
	}
	inCode := map[stored]bool{}
	for permission := range awsPermissions {
		inCode[stored{"aws", permission}] = true
	}
	for permission := range gcpPermissions {
		inCode[stored{"gcp", permission}] = true
	}
	for row := range inCode {
		if !listed[row] {
			t.Errorf("%v is a permission some call needs, but no row of permissionRows checks it", row)
		}
	}
	for row := range listed {
		if !inCode[row] {
			t.Errorf("%v is a row of permissionRows, but no call the code lists needs it", row)
		}
	}
}
