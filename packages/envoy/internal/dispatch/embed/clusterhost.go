package embed

import (
	"net"
	"strings"
)

// ClusterLocalHost reports whether host is reached inside the machine or the cluster: localhost, a
// loopback address, or a Kubernetes Service's name (ending .svc or .cluster.local), read without
// case and without a trailing dot. A call over plain http carries a credential to such a host
// alone (OWASP ASVS 5.0 12.3.1): cmd/dispatch refuses a model group's http endpoint on any other
// host, and the writing check's commands an http URL a key, a token or a bearer goes to
// (harness.CredentialURL), each by this one rule.
func ClusterLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
}
