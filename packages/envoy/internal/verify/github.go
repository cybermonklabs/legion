package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Github verifies a GitHub webhook's sha256= HMAC signature. An empty secret checks nothing, so it
// answers false for every signature.
func Github(secret string, body []byte, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	expected := sha256mac(secret, body)
	return hmac.Equal([]byte("sha256="+expected), []byte(signature))
}

func sha256mac(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
