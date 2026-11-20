// packages/envoy/cmd/agent-secrets/permission_calls_test.go
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

// TestEveryCallTheFormsMakeHasItsPermissions ties each call the forms make to the permissions it
// needs: every method of secretsAPI and stsAPI, and every method the package's code calls on the
// Secret Manager client (a `.client.<Method>(` in a non-test file), is an entry of
// awsCallPermissions or gcpCallPermissions, and those entries are exactly callPermissions'
// rows, which TestEachCallsRefusalNamesItsPermission checks a refusal of. So a call a later
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
	listed := map[callPermission]bool{}
	for _, row := range callPermissions {
		listed[row] = true
	}
	inCode := map[callPermission]bool{}
	for call, needs := range awsCallPermissions {
		store := "aws"
		if strings.HasPrefix(needs[0], "sts:") {
			store = "sts"
		}
		for _, permission := range needs {
			inCode[callPermission{store, call, permission}] = true
		}
	}
	for call, needs := range gcpCallPermissions {
		for _, permission := range needs {
			inCode[callPermission{"gcp", call, permission}] = true
		}
	}
	for row := range inCode {
		if !listed[row] {
			t.Errorf("%v is in the code's lists, but no row of callPermissions checks it", row)
		}
	}
	for row := range listed {
		if !inCode[row] {
			t.Errorf("%v is a row of callPermissions, but the code's lists do not hold it", row)
		}
	}
}
