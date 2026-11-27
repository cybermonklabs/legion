package repocache

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestARefusedEmulatorHostPrintsNoCredential: a STORAGE_EMULATOR_HOST carrying a made-up user
// name and password, as a URL (url) or as user information before a host:port (no-scheme), is no
// loopback host:port and is refused naming the variable, and the refusal holds no run of either:
// it quoted the value as written. The twin shows each value holds both and is still refused naming
// STORAGE_EMULATOR_HOST, so the row can see one printed. Captures repocache.emulator-host/<case>.
func TestARefusedEmulatorHostPrintsNoCredential(t *testing.T) {
	cases := []struct{ name, value string }{
		{"url", "http://" + wordsUser + ":" + wordsPassword + "@127.0.0.1:4443"},
		{"no-scheme", wordsUser + ":" + wordsPassword + "@127.0.0.1:4443"},
	}
	t.Run("twin", func(t *testing.T) {
		for _, tc := range cases {
			if !strings.Contains(tc.value, wordsUser) || !strings.Contains(tc.value, wordsPassword) {
				t.Errorf("%s: the value %q holds no credential, so the row could see none printed", tc.name, tc.value)
			}
			t.Setenv(emulatorHostEnv, tc.value)
			if b, err := openBucket(testwait.Context(t), testBucket, testRepo); b != nil || err == nil || !strings.Contains(err.Error(), emulatorHostEnv) {
				t.Errorf("%s: openBucket = %v, %v; want a refusal naming %s", tc.name, b, err, emulatorHostEnv)
			}
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(emulatorHostEnv, tc.value)
			b, err := openBucket(testwait.Context(t), testBucket, testRepo)
			if b != nil || err == nil {
				t.Fatalf("openBucket took STORAGE_EMULATOR_HOST %q; want it refused", tc.value)
			}
			oauth2calltest.RequireNoPiece(t, "repocache.emulator-host/"+tc.name, err.Error(), wordsUser, wordsPassword)
			if !strings.Contains(err.Error(), emulatorHostEnv) {
				t.Errorf("the refusal %q does not name %s", err, emulatorHostEnv)
			}
		})
	}
}
