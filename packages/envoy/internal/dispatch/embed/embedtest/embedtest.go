// Package embedtest holds fakes shared by this module's own tests and its callers' tests, kept
// out of the embed package proper so no test-only type ships in the production binary.
package embedtest

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// FakeThrottleError stands in for a model service's throttle/capacity error, as a client hands it
// back: it carries the Throttled() marker embed.IsThrottled reads, so a caller's test means
// "the provider said back off" without a live call. Code names the condition in the error's text.
// Shared by embed's own tests, embedqueue's, and api's - before this, two independent copies were
// hardcoded to "ThrottlingException" alone and a third, parameterized one in embed_test.go
// duplicated the same handful of lines a third time.
type FakeThrottleError struct{ Code string }

func (e FakeThrottleError) Error() string   { return "simulated: " + e.Code }
func (e FakeThrottleError) Throttled() bool { return true }

// BedrockConfig is the AWS configuration a test's BedrockClient is built from
// (embed.NewBedrockWithConfig): a fake region, static fake credentials and the SDK's own retries
// off. The SDK still signs every request (SigV4), but nothing on a stand-in Bedrock's side checks
// the signature, and a test that answers an error sees it on the first call rather than after the
// SDK's backoff. It returns the configuration rather than a client because embed's own tests use
// it, and this package cannot import embed.
func BedrockConfig() aws.Config {
	return aws.Config{
		Region:      "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
}

// ServingBedrockConfig is BedrockConfig with handler as the Bedrock it calls, reached in this
// process rather than over the machine's network: every request the AWS SDK builds is written as
// HTTP/1.1, read back as a server request, answered by handler, and what handler wrote is the
// client's response (ServingHTTPClient). The whole client is still the real one - the SDK's
// middleware, SigV4, the request and response encodings and the client's own decoding all run - and
// only the loopback socket is gone, so no verdict of a test using it rests on a dial, a port, a
// file descriptor or a connection the machine broke. A test whose subject IS the socket (embed's
// own hanging-server rows) keeps httptest.NewServer and a real client.
//
// A test points the client at a stand-in endpoint all the same (embed.WithBaseEndpoint), so the
// request names the stand-in rather than Bedrock's own host; nothing ever resolves or dials it.
func ServingBedrockConfig(handler http.Handler) aws.Config {
	cfg := BedrockConfig()
	cfg.HTTPClient = ServingHTTPClient{Handler: handler}
	return cfg
}

// ServingHTTPClient is the aws.HTTPClient ServingBedrockConfig installs: it answers each request
// from Handler, in this process.
type ServingHTTPClient struct{ Handler http.Handler }

// Do answers request from the handler, as ServingTransport does.
func (c ServingHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return ServingTransport{Handler: c.Handler}.RoundTrip(request)
}

// ServingTransport is the http.RoundTripper face of the same stand-in, for a client a place takes
// as an *http.Client rather than as an aws.HTTPClient (embed.AzureConfig's). An http.Client around
// it keeps its own Timeout, which it applies through the request's context, as it does over a
// socket.
type ServingTransport struct{ Handler http.Handler }

// RoundTrip answers request from the handler. It writes the request as HTTP/1.1 and reads it back,
// so the handler receives a server request as net/http's own server would build it (its URI, its
// host and a body of exactly Content-Length bytes), and it honours the request's context as a
// transport does: a handler that answers nothing before the context ends gives the context's
// error, and the handler keeps the recorder it is writing to, which nothing here reads again.
func (t ServingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		defer func() { _ = request.Body.Close() }() // the RoundTripper contract: the body is this call's to close
	}
	var wire bytes.Buffer
	if err := request.Write(&wire); err != nil {
		return nil, fmt.Errorf("embedtest: write the request: %w", err)
	}
	served, err := http.ReadRequest(bufio.NewReader(&wire))
	if err != nil {
		return nil, fmt.Errorf("embedtest: read the request back: %w", err)
	}
	ctx := request.Context()
	recorder := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		t.Handler.ServeHTTP(recorder, served.WithContext(ctx))
	}()
	select {
	case <-answered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	response := recorder.Result()
	response.Request = request
	return response, nil
}
