package cipheraddr

import "net/url"

// AirlockProxyUser is the user name of the proxy credential in the airlock proxy URL: the URL
// `agent-secrets NAME -- command` puts in the command's HTTPS_PROXY and https_proxy when the
// broker holds NAME's value back (cmd/cipher's airlock.go), with the grant's proxy password beside
// this name. The proxy reads the password alone and leaves the user part unread
// (proxy.proxyPassword), so it is a fixed word that says which program built the URL rather than
// the secret's name: a name in a proxy URL reaches a proxy log and a client's own error text.
const AirlockProxyUser = "agent-secrets"

// NamesAirlockProxy reports whether value, the value of a proxy variable such as HTTPS_PROXY, is
// an airlock proxy URL: one whose user is AirlockProxyUser. packages/daemon's
// leakguard.TokenChildKeeps reads it, and says why.
func NamesAirlockProxy(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.User != nil && parsed.User.Username() == AirlockProxyUser
}
