package main

import (
	"bufio"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTheConnectWritesItsRequestLineHostAndProxyAuthorization: the bytes connect writes to the
// proxy are the CONNECT request line for the bound host's port 443, its Host header, the URL's user
// and password as a Basic Proxy-Authorization where the proxy URL carries them
// (user-information) and none where it does not (no-user-information), and the blank line, byte for
// byte. A stand-in proxy records what reaches it up to that blank line and answers 200.
func TestTheConnectWritesItsRequestLineHostAndProxyAuthorization(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("agent-secrets:" + fixturePassword))
	for _, c := range []struct {
		name string
		user *url.Userinfo
		want string
	}{
		{"user-information", url.UserPassword("agent-secrets", fixturePassword),
			"CONNECT bound.example:443 HTTP/1.1\r\nHost: bound.example:443\r\nProxy-Authorization: Basic " + basic + "\r\n\r\n"},
		{"no-user-information", nil, "CONNECT bound.example:443 HTTP/1.1\r\nHost: bound.example:443\r\n\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			wire := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					wire <- "accept: " + err.Error()
					return
				}
				defer conn.Close()
				var got strings.Builder
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					got.WriteString(line)
					if err != nil || line == "\r\n" {
						break
					}
				}
				wire <- got.String()
				_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			}()
			proxy := &url.URL{Scheme: "http", User: c.user, Host: listener.Addr().String()}
			o := &loadOptions{proxyURL: proxy.String(), host: "bound.example"}
			conn, err := o.connect(10 * time.Second)
			if err != nil {
				t.Fatalf("connect = %v; want the tunnel the stand-in's 200 opens", err)
			}
			conn.Close()
			if got := <-wire; got != c.want {
				t.Errorf("the CONNECT on the wire is %q; want %q", got, c.want)
			}
		})
	}
}
