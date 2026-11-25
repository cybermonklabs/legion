// packages/envoy/cmd/cipher/aws_chain_test.go
//
// A Secrets Manager call's failure and a sign-in's are printed by one chain (awsWords), so a call
// that failed for a reason of its sign-in's says it as the sign-in would.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// TestAStoreCallFailsInTheSignInsWords: a Secrets Manager call whose failure is one of its
// sign-in's - a shared config file the credential refresh could not load, a container token file
// it could not read, an SDK error that quotes a setting's value - prints it in the words the
// sign-in's own failure prints it in, naming the setting and never the path or the value, since
// both read one chain. Control: an API error the call met prints AWS's code, as before.
func TestAStoreCallFailsInTheSignInsWords(t *testing.T) {
	planted := "hunter2pw-" + "storechain-" + "7b0d"
	call := func(err error) error {
		return &smithy.OperationError{ServiceID: secretsmanager.ServiceID, OperationName: "ListSecrets", Err: fmt.Errorf("failed to refresh cached credentials, %w", err)}
	}
	for _, row := range []struct {
		name string
		err  func(t *testing.T) error
		says string
	}{
		{"a shared config file the refresh could not load", func(t *testing.T) error {
			dir := filepath.Join(t.TempDir(), planted)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AWS_CONFIG_FILE", dir)
			return config.SharedConfigLoadError{Filename: dir, Err: errors.New("read all: read " + dir + ": is a directory")}
		}, "Secrets Manager ListSecrets: read the file AWS_CONFIG_FILE names: read: is a directory"},
		{"a container token file the refresh could not read", func(t *testing.T) error {
			t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", filepath.Join(t.TempDir(), planted))
			return errors.New("failed to load token: the SDK quotes " + planted)
		}, "Secrets Manager ListSecrets: read the file AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE names: open: no such file or directory"},
		{"an SDK error that quotes a setting's value", func(t *testing.T) error {
			t.Setenv("AWS_MAX_ATTEMPTS", planted)
			return errors.New("invalid value AWS_MAX_ATTEMPTS=" + planted)
		}, "Secrets Manager ListSecrets: the call failed (its message is not printed: it can quote an AWS setting's value); check AWS_MAX_ATTEMPTS and ~/.aws/config"},
		{"control: an API error", func(*testing.T) error {
			return &smithy.GenericAPIError{Code: "ThrottlingException", Message: "echo: " + planted}
		}, "Secrets Manager ListSecrets: AWS refused it: ThrottlingException (AWS's message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, name := range []string{"AWS_CONFIG_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_MAX_ATTEMPTS"} {
				t.Setenv(name, "")
			}
			wantNamedNotPrinted(t, "the store call's words", awsStoreFailure("ListSecrets", call(row.err(t))).Error(), []string{row.says}, planted)
		})
	}
}

// TestASignInThatTimedOutNamesTheAddressSettings: a sign-in whose STS call ran out of time, at an
// AWS_ENDPOINT_URL_STS that takes the connection and never answers, names that setting beside
// "context deadline exceeded", since the address may be why, and never prints its value, which
// carries a made-up secret. Control: the call ran out of its own deadline, so the row read a real
// timeout.
func TestASignInThatTimedOutNamesTheAddressSettings(t *testing.T) {
	planted := "hunter2pw-" + "stsdeadline-" + "1c4f"
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	awsSignInOnly(t)
	t.Setenv("AWS_ENDPOINT_URL_STS", "http://"+silent.Addr().String()+"/"+planted)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, identity, _, err := awsClients(ctx, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("control: the call failed with %v, not its deadline, so the row is blind", err)
	}
	wantNamedNotPrinted(t, "the sign-in's failure", awsSignInFailure("read your AWS sign-in", err).Error(),
		[]string{"read your AWS sign-in: context deadline exceeded (the address is not printed; AWS_ENDPOINT_URL_STS is set)"}, planted)
}
