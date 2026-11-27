package awsbody

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/middleware"
)

// The request body ends with endMark, so the client's connection knows which write carries its
// last byte.
const endMark = `"end":"the last byte of the body"}`

// forcedOrderBound bounds each wait of the forced order; none of them waits on anything but
// goroutines already running.
const forcedOrderBound = 30 * time.Second

// An answered call, forced into the order that fails it: the goroutine writing the request is held
// right after the body's last byte (heldWrite), the server reads the whole body and answers 200
// with a body larger than net/http's read buffer, smithy-go gets the headers and closes the request
// body, and only then is the write let go. With smithy-go's own body, net/http's read past the
// Content-Length bytes gets the closed body's io.EOF from WriteTo, its write loop closes the
// connection, and reading the response fails with "use of closed network connection". Through
// Client the same order answers the call. The failing row is also this package's removal check:
// once the smithy-go this module builds carries aws/smithy-go#728, it fails, naming what to delete.
func TestAClosedBodysWriteToStillFailsAnAnsweredCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		through bool
	}{
		{"smithy-go's own body fails the answered call", false},
		{"through Client the call is answered", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer := bytes.Repeat([]byte("a"), 256<<10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(answer)
			}))
			defer server.Close()

			held := &heldWrite{release: make(chan struct{}), closed: make(chan struct{})}
			wrote := make(chan error, 1)
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				WroteRequest: func(info httptrace.WroteRequestInfo) { wrote <- info.Err },
			})
			cfg := aws.Config{
				Region:      "us-west-2",
				Credentials: credentials.NewStaticCredentialsProvider("test-access-key", "test-secret-key", ""),
				Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
				HTTPClient: awshttp.NewBuildableClient().WithTransportOptions(func(transport *http.Transport) {
					dial := (&net.Dialer{}).DialContext
					transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
						conn, err := dial(ctx, network, address)
						if err != nil {
							return nil, err
						}
						held.Conn = conn
						return held, nil
					}
				}),
				// Innermost in the deserialize step: it runs once smithy-go's HTTP handler has
				// returned, after its Close of the request body, and before the operation reads
				// the response body.
				APIOptions: []func(*middleware.Stack) error{func(stack *middleware.Stack) error {
					return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("releaseHeldWrite",
						func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
							out, metadata, err := next.HandleDeserialize(ctx, in)
							if err != nil {
								return out, metadata, err
							}
							return out, metadata, held.letGo(wrote)
						}), middleware.After)
				}},
			}
			client := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
				o.BaseEndpoint = aws.String(server.URL)
				if tc.through {
					o.HTTPClient = Client(o.HTTPClient)
				}
			})
			body := []byte(`{"pad":"` + strings.Repeat("x", 64<<10) + `",` + endMark)

			out, err := client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
				ModelId:     aws.String("forced-order"),
				Body:        body,
				ContentType: aws.String("application/json"),
			})
			if !held.didHold() {
				t.Fatalf("the request's write was never held after its last byte (err %v): the order was not forced", err)
			}
			if tc.through {
				if err != nil || out == nil || len(out.Body) != len(answer) {
					t.Fatalf("through Client: InvokeModel = %v, want the whole %d-byte answer", err, len(answer))
				}
				return
			}
			if err == nil {
				t.Fatalf("without Client the answered call succeeded: smithy-go %s no longer returns io.EOF from a closed request body's WriteTo "+
					"(aws/smithy-go#728 is in it). Delete package awsbody and every awsbody.Client call", smithyVersion())
			}
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("without Client: InvokeModel = %v, want the read of the answer to fail on the closed connection", err)
			}
			t.Logf("without Client, smithy-go %s: %v", smithyVersion(), err)
		})
	}
}

// heldWrite is a client connection that holds the write carrying the request body's last byte,
// after the byte has gone, until letGo, and reports its own Close.
type heldWrite struct {
	net.Conn
	release chan struct{}
	closed  chan struct{}

	mu        sync.Mutex
	tail      []byte // the last len(endMark) bytes written
	held      bool
	closeOnce sync.Once
}

func (c *heldWrite) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.tail = append(c.tail, p[:n]...)
	if len(c.tail) > len(endMark) {
		c.tail = c.tail[len(c.tail)-len(endMark):]
	}
	hold := !c.held && string(c.tail) == endMark
	c.held = c.held || hold
	c.mu.Unlock()
	if hold {
		select {
		case <-c.release:
		case <-time.After(forcedOrderBound):
		}
	}
	return n, err
}

func (c *heldWrite) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *heldWrite) didHold() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

// letGo releases the held write and waits for net/http to report the request written, and, when
// it reports an error, for the connection to be closed: the order the answered call meets.
func (c *heldWrite) letGo(wrote <-chan error) error {
	if !c.didHold() {
		return nil
	}
	close(c.release)
	select {
	case err := <-wrote:
		if err == nil {
			return nil
		}
		select {
		case <-c.closed:
			return nil
		case <-time.After(forcedOrderBound):
			return fmt.Errorf("the request's write failed (%v) and the connection was not closed within %v", err, forcedOrderBound)
		}
	case <-time.After(forcedOrderBound):
		return fmt.Errorf("net/http did not report the request written within %v of letting the write go", forcedOrderBound)
	}
}

// smithyVersion is the version of smithy-go this test binary was built with.
func smithyVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/aws/smithy-go" {
			if dep.Replace != nil {
				return dep.Version + " => " + dep.Replace.Path + " " + dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "(not in the build)"
}
