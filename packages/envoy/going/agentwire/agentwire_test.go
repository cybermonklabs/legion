package agentwire

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testtmp"
)

// countingReader counts the bytes read from it.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// A frame whose header names more bytes than its reader's limit is refused at the header: the
// reader reads the 4 bytes of the length and none of the payload behind them, so a peer cannot
// make it hold more than its limit. A frame within the limit reads back whole.
func TestAFramePastItsLimitIsRefusedAtItsHeader(t *testing.T) {
	var stream bytes.Buffer
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, 1<<20)
	stream.Write(header)
	stream.Write(bytes.Repeat([]byte("x"), 1<<20))
	counted := &countingReader{r: &stream}
	_, err := ReadFrame(counted, 1024)
	var tooLarge FrameTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Size != 1<<20 || tooLarge.Limit != 1024 {
		t.Fatalf("a frame of 1 MiB read within 1 KiB answered %v, want FrameTooLargeError of 1048576 past 1024", err)
	}
	if counted.read != 4 {
		t.Fatalf("the refused frame read %d bytes, want its 4-byte header alone", counted.read)
	}

	var written bytes.Buffer
	if err := WriteFrame(&written, []byte(`{"call":"hello"}`), 1024); err != nil {
		t.Fatalf("write a frame: %v", err)
	}
	if err := WriteFrame(&written, bytes.Repeat([]byte("y"), 2048), 1024); err == nil {
		t.Fatal("a payload of 2 KiB was written within 1 KiB, want it refused")
	}
	payload, err := ReadFrame(&written, 1024)
	if err != nil || string(payload) != `{"call":"hello"}` {
		t.Fatalf("the frame read back as %q, %v; want the payload written", payload, err)
	}
	if _, err := ReadFrame(&written, 1024); !errors.Is(err, io.EOF) {
		t.Fatalf("the refused write left %v behind it, want nothing", err)
	}
}

// fakeServer answers each connection on a socket of its own with what answer makes of the
// request frame it read.
func fakeServer(t *testing.T, answer func(Request) []byte) string {
	t.Helper()
	path := testtmp.Socket(t, "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				payload, err := ReadFrame(conn, MaxRequestLimit)
				if err != nil {
					t.Errorf("read the request: %v", err)
					return
				}
				var request Request
				if err := json.Unmarshal(payload, &request); err != nil {
					t.Errorf("decode the request: %v", err)
					return
				}
				reply := answer(request)
				header := make([]byte, 4)
				binary.BigEndian.PutUint32(header, uint32(len(reply)))
				_, _ = conn.Write(append(header, reply...))
			}()
		}
	}()
	return path
}

// The client sends the contract's version, the secret and the call's own body, decodes the
// answer's body into the call's shape, hands back the server's refusal as an *Error with its code
// and reason, and refuses an answer frame past the call's answer limit.
func TestTheClientSpeaksTheContract(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, SecretSize)
	path := fakeServer(t, func(request Request) []byte {
		if request.Version != Version || !bytes.Equal(request.Secret, secret) {
			reply, _ := json.Marshal(Answer{Error: &Error{Code: CodeUnauthorized, Message: "no secret"}})
			return reply
		}
		switch request.Call {
		case CallHello:
			var hello HelloRequest
			if err := json.Unmarshal(request.Body, &hello); err != nil || hello.Instance != "i-1" {
				reply, _ := json.Marshal(Answer{Error: &Error{Code: CodeBadRequest, Message: "no instance"}})
				return reply
			}
			reply, _ := json.Marshal(Answer{Body: json.RawMessage(`{"freed":2}`)})
			return reply
		case CallCall:
			reply, _ := json.Marshal(Answer{Error: &Error{Code: CodeGrantEnded, Message: "signed out", Reason: "SESSION_ENDED"}})
			return reply
		}
		// An end answers a frame past the 4 KiB an end's answer may be.
		reply, _ := json.Marshal(Answer{Body: json.RawMessage(`{"pad":"` + string(bytes.Repeat([]byte("z"), 8<<10)) + `"}`)})
		return reply
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := Client{Path: path, Secret: secret}

	answer, err := client.Hello(ctx, HelloRequest{Instance: "i-1"})
	if err != nil || answer.Freed != 2 {
		t.Fatalf("hello answered %+v, %v; want 2 freed", answer, err)
	}
	_, err = client.Call(ctx, CallRequest{Run: "r", Kind: KindRoute, Tool: "dispatch_read"})
	var refused *Error
	if !errors.As(err, &refused) || refused.Code != CodeGrantEnded || refused.Reason != "SESSION_ENDED" {
		t.Fatalf("a refused call answered %v, want *Error GRANT_ENDED with its reason", err)
	}
	_, err = Client{Path: path, Secret: []byte("wrong")}.Hello(ctx, HelloRequest{Instance: "i-1"})
	if !errors.As(err, &refused) || refused.Code != CodeUnauthorized {
		t.Fatalf("a hello under another secret answered %v, want *Error UNAUTHORIZED", err)
	}
	var tooLarge FrameTooLargeError
	if err := client.End(ctx, EndRequest{Run: "r", State: StateDone}); !errors.As(err, &tooLarge) {
		t.Fatalf("an end answered 8 KiB answered %v, want the frame refused past the end's limit", err)
	}
}
