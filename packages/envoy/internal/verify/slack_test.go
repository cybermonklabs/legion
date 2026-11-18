package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// TestSlack: Slack accepts only a signature its secret made over a timestamp inside the 5-minute
// window. An empty secret checks nothing, so it answers false for every signature, the one made
// with the empty key too.
func TestSlack(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	now := fmt.Sprintf("%d", time.Now().Unix())
	old := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	sign := func(secret, ts string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(fmt.Sprintf("v0:%s:%s", ts, string(body))))
		return "v0=" + hex.EncodeToString(mac.Sum(nil))
	}
	for _, tc := range []struct {
		name, secret, timestamp, signature string
		want                               bool
	}{
		{name: "no secret, a signature made with the empty key", timestamp: now, signature: sign("", now)},
		{name: "control: a secret and its signature", secret: "secret", timestamp: now, signature: sign("secret", now), want: true},
		{name: "control: another secret's signature", secret: "wrong", timestamp: now, signature: sign("secret", now)},
		{name: "control: a secret and a bad digest", secret: "secret", timestamp: now, signature: "v0=deadbeef"},
		{name: "control: a secret and no signature", secret: "secret", timestamp: now},
		{name: "control: a stale timestamp, signed over that timestamp", secret: "secret", timestamp: old, signature: sign("secret", old)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Slack(tc.secret, body, tc.timestamp, tc.signature)
			t.Logf("Slack(secret set %v, timestamp %s, signature %q) = %v", tc.secret != "", tc.timestamp, tc.signature, got)
			if got != tc.want {
				t.Fatalf("Slack(secret=%q, timestamp=%s, signature=%q) = %v, want %v", tc.secret, tc.timestamp, tc.signature, got, tc.want)
			}
		})
	}
}
