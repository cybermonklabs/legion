package aws_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
)

// TestTheWebIdentityTokenFileIsNamedByItsVariable: the web identity token file holds a token, so
// the using line and a store's refusal name it by AWS_WEB_IDENTITY_TOKEN_FILE, never by its path,
// which here carries a made-up credential, and so does a later failed read of it (FileName),
// in a store call whose credential refresh reads it again as well as in the sign-in. Control: the
// command a person pastes to run again still names the file, which signed in.
func TestTheWebIdentityTokenFileIsNamedByItsVariable(t *testing.T) {
	planted := "hunter2pw-" + "webidpath-" + "31d0"
	file := filepath.Join(t.TempDir(), planted, "token")
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_PROFILE", "AWS_DEFAULT_PROFILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", file)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::111122223333:role/agent")
	phrase, another, prefix := awsstore.SourceWords(nil)
	wantNamedNotPrinted(t, "the source's words", phrase+"; "+another, []string{"the web identity token file AWS_WEB_IDENTITY_TOKEN_FILE names"}, planted)
	wantNamedNotPrinted(t, "a later read's words", awsstore.FileName(file), []string{"the file AWS_WEB_IDENTITY_TOKEN_FILE names"}, planted)
	if !strings.Contains(prefix, file) {
		t.Errorf("the command to paste %q does not name the file that signed in", prefix)
	}
	t.Run("a store call whose credential refresh cannot read the file again", func(t *testing.T) {
		_, read := awsstore.WebIdentityToken(file).GetIdentityToken()
		if read == nil {
			t.Fatal("control: the token file was read, so the row is blind")
		}
		// The SDK's chain for a store call whose credential refresh failed to read the token.
		failed := &smithy.OperationError{ServiceID: secretsmanager.ServiceID, OperationName: "ListSecrets",
			Err: fmt.Errorf("failed to refresh cached credentials, %w", fmt.Errorf("failed to retrieve jwt from provide source, %w", read))}
		wantNamedNotPrinted(t, "the store call's words", awsstore.StoreFailure("ListSecrets", failed).Error(),
			[]string{"Secrets Manager ListSecrets: read the file AWS_WEB_IDENTITY_TOKEN_FILE names: open: no such file or directory"}, planted)
	})
}
