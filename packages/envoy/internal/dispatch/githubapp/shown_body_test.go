package githubapp_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// A body is shown whole up to 512 runes and cut at 512 past that, at a rune and never inside one,
// naming the whole body's bytes, whether the body is a string or bytes; a byte that is no UTF-8
// counts as one rune.
func TestShownBodyCutsAtTheBoundOfRunesAndNeverInsideOne(t *testing.T) {
	for _, body := range []struct {
		name, body, want string
	}{
		{"a short body", `{"message":"Not Found"}`, `{"message":"Not Found"}`},
		{"512 two-byte letters", strings.Repeat("é", 512), strings.Repeat("é", 512)},
		{"513 two-byte letters", strings.Repeat("é", 513), strings.Repeat("é", 512) + "… (1026 bytes)"},
		{"600 four-byte letters", strings.Repeat("😀", 600), strings.Repeat("😀", 512) + "… (2400 bytes)"},
		{"600 bytes that are no UTF-8", strings.Repeat("\xff", 600), strings.Repeat("\xff", 512) + "… (600 bytes)"},
	} {
		t.Run(strings.ReplaceAll(body.name, " ", "_"), func(t *testing.T) {
			if got := githubapp.ShownBody(body.body); got != body.want {
				t.Errorf("ShownBody of %d bytes = %.80q (%d bytes), want %.80q (%d bytes)", len(body.body), got, len(got), body.want, len(body.want))
			}
			if got := githubapp.ShownBody([]byte(body.body)); got != body.want {
				t.Errorf("ShownBody of the same %d bytes as a []byte = %.80q, want %.80q", len(body.body), got, body.want)
			}
		})
	}
}
