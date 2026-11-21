package projectrepos_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestARefusalsCodeIsQuotedAtMostSixtyFourBytes: the code a refusal of Dispatch's names is text of
// Dispatch's answer that an error quotes, as a key or a value of a 200's body is, so it is cut to
// its first 64 bytes too: a 503, a 404 and a 403 (Dispatch refusing the broker's own token) whose
// code is 1,000 bytes long each give an error that quotes no more than 64 of them.
func TestARefusalsCodeIsQuotedAtMostSixtyFourBytes(t *testing.T) {
	long := strings.Repeat("k", 1000)
	for name, status := range map[string]int{
		"Dispatch's 503":                       http.StatusServiceUnavailable,
		"a project Dispatch does not hold":     http.StatusNotFound,
		"Dispatch refusing the broker's token": http.StatusForbidden,
	} {
		t.Run(name, func(t *testing.T) {
			server := standIn(t)
			server.SetAnswer(status, `{"code":"`+long+`","error":"refused"}`)
			got, err := clientOf(server).Repositories(t.Context(), "PRIV")
			if err == nil || got != nil {
				t.Fatalf("Repositories(PRIV) answered %d = %q, %v; want an error and no list", status, got, err)
			}
			if !strings.Contains(err.Error(), strings.Repeat("k", 60)) || strings.Contains(err.Error(), strings.Repeat("k", 65)) {
				t.Fatalf("the refusal quotes %d bytes of the code; want its first 64 and no more: %.200s…", strings.Count(err.Error(), "k"), err)
			}
		})
	}
}
