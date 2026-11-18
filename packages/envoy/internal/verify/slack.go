package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Slack verifies a Slack request's v0= signature and that its timestamp is within 5 minutes of now.
// An empty secret checks nothing, so it answers false for every signature.
func Slack(secret string, body []byte, timestamp string, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "v0=") || timestamp == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if delta := time.Now().Unix() - ts; delta > 300 || delta < -300 {
		return false
	}
	base := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	return hmac.Equal([]byte("v0="+hex.EncodeToString(mac.Sum(nil))), []byte(signature))
}
