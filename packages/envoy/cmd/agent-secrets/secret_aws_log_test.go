// packages/envoy/cmd/agent-secrets/secret_aws_log_test.go
//
// The AWS SDK's own log: it writes WARN lines to the process's stderr itself, past every message
// this client words, so the clients awsClients builds must log nothing.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// captureStderr is what fn writes to the process's stderr, os.Stderr itself, while it runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	saved := os.Stderr
	os.Stderr = w
	func() {
		defer func() {
			os.Stderr = saved
			_ = w.Close()
		}()
		fn()
	}()
	return <-read
}

// TestTheAWSSDKsOwnLogWritesNothing: the AWS SDK logs a response's Date header it cannot parse,
// quoted whole, as a WARN line on the process's stderr, and whoever answers at AWS_ENDPOINT_URL
// writes that header: here a made-up secret. A call through the clients awsClients builds writes
// none of it. Controls: the stand-in answered the call, which succeeded, and the capture sees a
// line written to the process's stderr.
func TestTheAWSSDKsOwnLogWritesNothing(t *testing.T) {
	planted := "hunter2pw-" + "datehdr-" + "8e1f"
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.Header().Set("Date", planted)
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::111122223333:user/ada</Arn><UserId>AIDAEXAMPLE</UserId><Account>111122223333</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r5</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	t.Cleanup(srv.Close)
	awsSignInOnly(t)
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	const seen = "control: a line the capture sees"
	var callErr error
	written := captureStderr(t, func() {
		fmt.Fprintln(os.Stderr, seen)
		_, identity, _, err := awsClients(context.Background(), "us-east-1")
		if err != nil {
			callErr = err
			return
		}
		_, callErr = identity.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	})
	if callErr != nil || !called.Load() {
		t.Fatalf("control: the call through the stand-in failed (%v) or never reached it, so the row is blind", callErr)
	}
	wantNamedNotPrinted(t, "the process's stderr", written, []string{seen}, planted, "Date header")
}
