// packages/envoy/internal/cipher/helper/request_reparsed_test.go
//go:build linux

package helper

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerRequestWhoseJoinedURLDoesNotParsePrintsNoneOfIt: an address with redacttest's
// credentials in its user information and its query that url.Parse takes, whose URL joined with a
// route, as url.URL.String writes it, url.Parse refuses again (an IPv6 zone holding a byte that is
// not ASCII, which String escapes as %FD), fails Broker.request before anything is sent, through
// the Address's Error, the URL as redact.NotPrinted (reparsed). Twin: http.NewRequest's error for
// that joined URL quotes the credentials, so the row can see them printed. Captures
// helper.broker-request/reparsed.
func TestABrokerRequestWhoseJoinedURLDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	address := "http://" + redacttest.User + ":" + redacttest.Password + "@[::%25\xfd]/broker?" + param + "=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		joined, err := cipheraddr.New(address).Join("/v1/enrollments")
		if err != nil {
			t.Fatalf("the join refused the address (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, joined.String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		b := &Broker{URL: cipheraddr.New(address)}
		_, _, err := b.request(context.Background(), http.MethodPost, "/v1/enrollments", nil)
		if err == nil {
			t.Fatal("the request was built")
		}
		redacttest.RequireNoPiece(t, "helper.broker-request/reparsed", err.Error())
	})
}
