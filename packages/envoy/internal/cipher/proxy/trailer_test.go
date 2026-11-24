// packages/envoy/internal/cipher/proxy/trailer_test.go
//
// The leftover scan over a request's trailers. A trailer is a header field a chunked request
// sends after its body, so it reaches the proxy only where the proxy has already read that body
// to its end - an AWS call it signs with the body's hash, a Limited name's post it reads to find
// the channel. On those paths a placeholder in a trailer would otherwise go to the service with
// the request, which is the hole this file holds shut.
package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// trailerBody is a request body that writes its trailers as the last byte is read, which is what
// net/http's client requires of a request that declares them: the values must be in the map
// before the body returns io.EOF, since the chunked writer sends them straight after the final
// chunk.
type trailerBody struct {
	reader io.Reader
	// set is run once, when the body has been read to its end.
	set  func()
	done bool
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		b.set()
	}
	return n, err
}

func (b *trailerBody) Close() error { return nil }

// chunkedWithTrailer is the request each row below sends: a PUT whose body is chunked, whose
// length is unknown, and which declares one trailer that carries value. It names no
// X-Amz-Content-Sha256, so the proxy reads the body to hash it (signAWS) and the trailer has
// arrived by the time the request is about to go upstream.
func chunkedWithTrailer(t *testing.T, name, value, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, "https://"+boundHost+"/v1/upload", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Trailer = http.Header{name: nil}
	request.Body = &trailerBody{reader: strings.NewReader(body), set: func() { request.Trailer.Set(name, value) }}
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	credentials := aws.Credentials{AccessKeyID: awsKeyPlaceholder, SecretAccessKey: awsSecretPlaceholder, SessionToken: awsSessionPlaceholder}
	if err := v4.NewSigner().SignHTTP(t.Context(), credentials, request, awsUnsignedBody, "s3", awsRegion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The command signed without naming a body hash, so the proxy reads the body and hashes it
	// itself. A request that names one is signed again from that name and its body is streamed,
	// which is the row below.
	request.Header.Del("X-Amz-Content-Sha256")
	return request
}

// TestAPlaceholderInATrailerIsRefusedWhereTheBodyIsRead: an upload whose body the proxy reads
// carries its trailers to the proxy before anything goes upstream, so a placeholder in one is
// refused 403 PLACEHOLDER_NOT_SWAPPED and the service is sent nothing - the same answer as a
// placeholder in a header. The control, in the same run: the same upload with no placeholder in
// its trailer reaches the service, with the trailer as the command wrote it, so the refusal is
// the placeholder's and not the trailer's. The named revert: proxy-leftover-skips-trailers.
func TestAPlaceholderInATrailerIsRefusedWhereTheBodyIsRead(t *testing.T) {
	r := newAWSRig(t, true)
	client := r.client(awsKeyPlaceholder)

	answer, body := sendAWS(t, client, chunkedWithTrailer(t, "X-Note", "carried: "+awsSessionPlaceholder, "an upload the proxy hashes"))
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "placeholder") {
		t.Fatalf("the upload carrying a placeholder in a trailer answered %d %q; want 403 naming the placeholder left after the swap", answer.StatusCode, body)
	}
	if lines := r.logs.refusals(); len(lines) != 1 || lines[0].fields["code"] != codePlaceholderLeft {
		t.Fatalf("the refusal logged %v; want one line coded %s", lines, codePlaceholderLeft)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d call(s) carrying a placeholder in a trailer; want none: %v", len(seen), seen)
	}

	answer, body = sendAWS(t, client, chunkedWithTrailer(t, "X-Note", "nothing of a grant", "an upload the proxy hashes"))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the control upload answered %d %q; want 200, since its trailer carries no placeholder", answer.StatusCode, body)
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the control reached the service as %v; want one call", seen)
	}
	if got := seen[0].Header.Get("X-Note"); got != "" {
		t.Errorf("the control's trailer reached the service as a header %q; a trailer is no header", got)
	}
}
