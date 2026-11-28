package tests3

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A request to a server that takes the connection and never answers ends at its deadline, by
// name, rather than holding the test to go test's -timeout.
func TestARequestThatIsNeverAnsweredEndsAtItsDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the server that never answers: %v", err)
	}
	var held []net.Conn
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	url := "http://" + listener.Addr().String() + "/bucket"
	_, _, err = request(context.Background(), http.MethodPut, url, "", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "PUT "+url) {
		t.Fatalf("request to a server that never answers: %v, want an error naming PUT %s and its deadline", err, url)
	}
}
