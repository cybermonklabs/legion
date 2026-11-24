package oidc

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// rawIssuer answers every request with statusLine and then body, byte for byte, so the reason
// phrase is whatever a row says.
func rawIssuer(t *testing.T, statusLine, body string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = http.ReadRequest(bufio.NewReader(conn))
				fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", statusLine, len(body), body)
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

// TestADiscoveryErrorPrintsNoCredentialItsReferencesHide: an issuer's discovery answer whose body,
// which go-oidc quotes in its error, or whose reason phrase writes a credential's URL with its
// separators as HTML character references (&sol;, &colon;, &commat;, &quest;, &equals;). NewKeys'
// and CheckClient's errors print none of the credential: the rule decodes the references of a
// URL's separators before it reads a field (redact.Remote), so every caller gets the decoding,
// not only the two that decode a page themselves. Captures oidc.discovery-references/<case>/<call>.
func TestADiscoveryErrorPrintsNoCredentialItsReferencesHide(t *testing.T) {
	const userinfo = "https:&sol;&sol;" + redacttest.User + "&colon;" + redacttest.Password + "&commat;issuer.example&sol;path"
	const query = "https://issuer.example/cb&quest;access_token&equals;" + redacttest.Token
	for _, c := range []struct{ name, status, body string }{
		{"body-userinfo", "500 Internal Server Error", "<p>Continue at " + userinfo + " to sign in</p>"},
		{"body-query", "500 Internal Server Error", "<p>Continue at " + query + " </p>"},
		{"reason-query", "500 " + query, "x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer := rawIssuer(t, c.status, c.body)
			_, keysErr := NewKeys(context.Background(), issuer)
			_, checkErr := CheckClient(context.Background(), issuer, "client", "client-secret-x")
			for call, err := range map[string]error{"NewKeys": keysErr, "CheckClient": checkErr} {
				if err == nil {
					t.Fatalf("%s answered no error", call)
				}
				redacttest.RequireNone(t, "oidc.discovery-references/"+c.name+"/"+call, fmt.Sprintf("%s | %+v | %#v", err.Error(), err, err))
			}
		})
	}
}
