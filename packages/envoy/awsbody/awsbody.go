// Package awsbody keeps an answered AWS SDK call over HTTP/1.1 from failing because smithy-go
// closed its request body early. A service client takes it in its option function:
//
//	svc.NewFromConfig(cfg, func(o *svc.Options) { o.HTTPClient = awsbody.Client(o.HTTPClient) })
//
// The failure it prevents, read in smithy-go v1.28.6 and Go 1.27.3's net/http:
//
//  1. smithy-go wraps a request's body in a reader whose WriteTo, once the body is closed, returns
//     io.EOF (transport/http/internal/io/safe.go). io.WriterTo must return nil at the end of its
//     data, and io.Copy hands that io.EOF on as an error.
//  2. smithy-go closes the body as soon as the HTTP client's Do returns, that is, once the
//     response's headers are in (transport/http/client.go).
//  3. After net/http's HTTP/1.1 transport has written the body's Content-Length bytes, it reads the
//     body once more for bytes past that length (transfer.go's writeBody), through WriteTo.
//
// When the goroutine writing the request is held between its last byte and that read for as long
// as the server takes to answer (a loaded CPU is enough), the read comes after the close, gets the
// io.EOF, and net/http's write loop closes the connection while the response body is still being
// read. The call fails after an HTTP 200 with "use of closed network connection", and the server
// has done the work: a retry sends it again, and with no retry the caller treats an answered call
// as failed.
//
// Only HTTP/1.1 meets it: net/http's HTTP/2 transport reads a request body through Read alone. Over
// TLS the SDK's default client asks for HTTP/2. In us-west-2 on 2026-11-27, Bedrock Runtime and
// Cognito answered over HTTP/2, and S3, Secrets Manager, KMS and STS over HTTP/1.1. A stand-in
// server over plain HTTP, as tests use, is HTTP/1.1.
//
// aws/smithy-go#719 reports it. #728, #741 and #752 each propose the fix, which no release carries
// as of v1.28.10.
//
// Client hands net/http each body with only Read and Close, so that last read goes through Read,
// whose io.EOF io.Copy takes as the end of the data. A body cut short still fails the request:
// net/http still compares what it read with Content-Length.
//
// TestASmithyRequestBodyClosedFirstStillFailsWriteTo fails once the smithy-go this module builds
// carries a fix of aws/smithy-go#719, which is when this package and its callers are to go.
package awsbody

import (
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Client returns inner with the body of each request it sends seen by net/http as a plain
// io.ReadCloser (the package comment says why).
func Client(inner aws.HTTPClient) aws.HTTPClient {
	return client{inner: inner}
}

type client struct {
	inner aws.HTTPClient
}

// Do sends a shallow copy of request whose Body hides any method but Read and Close. The caller's
// request is left as it was: smithy-go closes its own Body, which the copy's reads then see.
func (c client) Do(request *http.Request) (*http.Response, error) {
	if request.Body != nil && request.Body != http.NoBody {
		sent := *request
		sent.Body = readCloser{request.Body}
		request = &sent
	}
	return c.inner.Do(request)
}

// readCloser is an io.ReadCloser and nothing more.
type readCloser struct {
	io.ReadCloser
}
