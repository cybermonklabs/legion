package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/redact"
)

// EdgeSettings are the broker's edge's three settings, set together or not at all. Every message
// that names them names them in this order.
var EdgeSettings = []string{"BROKER_EDGE_LISTEN_ADDR", "BROKER_EDGE_URL", "BROKER_EDGE_MACHINE_LOGIN_URL"}

// workspaceDomainsSetting is the setting that names the Workspace's other domains.
const workspaceDomainsSetting = "BROKER_SIGNIN_WORKSPACE_DOMAINS"

// loadEdge holds cfg's edge settings, which Load has read into it, to what the edge needs:
// all three or none; a listen address of host:port whose port is a number and not the port of
// the broker's other three listeners; an edge URL of BROKER_PUBLIC_URL's form whose origin is not
// BROKER_PUBLIC_URL's; a page that is an absolute http(s) URL; and the two settings the edge's
// routes stand on, the device grant's page and the sign-in domain. With none of the three set it
// holds nothing, and the broker has no edge.
func loadEdge(cfg *Config) error {
	values := []string{cfg.EdgeListenAddr, cfg.EdgeURL, cfg.EdgeMachineLoginURL}
	var missing []string
	for i, setting := range EdgeSettings {
		if values[i] == "" {
			missing = append(missing, setting)
		}
	}
	switch len(missing) {
	case len(EdgeSettings):
		return nil
	case 0:
	default:
		return fmt.Errorf("the broker's edge needs %s; missing %s", strings.Join(EdgeSettings, ", "), strings.Join(missing, ", "))
	}
	_, edgePortText, err := net.SplitHostPort(cfg.EdgeListenAddr)
	if err != nil {
		return fmt.Errorf("BROKER_EDGE_LISTEN_ADDR must be host:port, such as 0.0.0.0:13383, got %q", cfg.EdgeListenAddr)
	}
	edgePort, err := strconv.ParseUint(edgePortText, 10, 16)
	if err != nil {
		return fmt.Errorf("BROKER_EDGE_LISTEN_ADDR must name its port as a number from 0 to 65535, such as 0.0.0.0:13383, got %q", cfg.EdgeListenAddr)
	}
	// Port 0 is the kernel's choice of a port no listener holds, so two listeners given it never
	// share one: a local run gives it to every listener.
	if edgePort != 0 {
		for _, other := range []struct{ setting, addr string }{
			{"BROKER_LISTEN_ADDR", cfg.ListenAddr},
			{"BROKER_METRICS_LISTEN_ADDR", cfg.MetricsListenAddr},
			{"BROKER_AIRLOCK_LISTEN_ADDR", cfg.AirlockListenAddr},
		} {
			// Each port is read as the bind reads it (listenPort), so a spelling of the same
			// number, 013382 beside 13382, is the same port. An address that does not parse is
			// not this check's to refuse: its own bind refuses the boot, naming it.
			if port, ok := listenPort(other.addr); ok && port == int(edgePort) {
				return fmt.Errorf("BROKER_EDGE_LISTEN_ADDR (%s) and %s (%s) name the same port, %d: the edge is a listener of its own, which the public load balancer reaches and no other caller does, so give it a port no other listener of the broker binds",
					cfg.EdgeListenAddr, other.setting, other.addr, edgePort)
			}
		}
	}
	// The edge serves its routes under this URL as the broker's own serve theirs under
	// BROKER_PUBLIC_URL (api.RegisterEdge gives its Deps this URL as PublicURL), so it is held to
	// that URL's form, and its refusal names the setting alone: the URL as written is where a
	// credential would be.
	edgeURL, err := url.Parse(cfg.EdgeURL)
	if err != nil || (edgeURL.Scheme != "http" && edgeURL.Scheme != "https") || !originAlone(edgeURL) {
		return fmt.Errorf("BROKER_EDGE_URL must be an absolute http or https URL with no user information, path, query or fragment")
	}
	// One origin for both would let a DPoP proof made for one listener be spent at the other,
	// which naming each its own address is there to prevent. The origins are compared as the
	// proof's htu is (record.SameOrigin): https://secrets.example:443 is https://secrets.example.
	if record.SameOrigin(cfg.EdgeURL, cfg.PublicURL) {
		return fmt.Errorf("BROKER_EDGE_URL (%s) is BROKER_PUBLIC_URL's origin: the edge's callers name an address of its own in their request objects and proofs, so that one made for either listener is never spent at the other", redact.URL(cfg.EdgeURL))
	}
	if err := httpURLSetting("BROKER_EDGE_MACHINE_LOGIN_URL", cfg.EdgeMachineLoginURL); err != nil {
		return err
	}
	if cfg.MachineLoginURL == "" {
		return fmt.Errorf("BROKER_EDGE_LISTEN_ADDR needs BROKER_MACHINE_LOGIN_URL: the edge serves the device grant, which the broker composes only with a machine-login page")
	}
	if cfg.SignInOff {
		return fmt.Errorf("BROKER_EDGE_LISTEN_ADDR needs BROKER_SIGNIN_DOMAIN: the edge refuses a machine login naming a person of the Workspace, and %s=%s names no Workspace", signInSwitchSetting, signInOffValue)
	}
	return nil
}

// listenPort is the port addr, a listener's host:port, binds, read as net.Listen reads it
// (net.LookupPort): a decimal number, written with leading zeros or a sign or not, or a service
// name. It answers false for an address that does not split into a host and a port, and for a
// port the lookup does not take, which the listener's own bind refuses.
func listenPort(addr string) (int, bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	n, err := net.LookupPort("tcp", port)
	return n, err == nil
}

// parseWorkspaceDomains reads BROKER_SIGNIN_WORKSPACE_DOMAINS, raw: a list separated by commas,
// each entry trimmed, and each as oidc.WorkspaceDomainALabel writes it, the A-label form the edge
// compares an address's domain in. A value blank once trimmed lists none. An entry that is empty,
// or one oidc.WorkspaceDomainALabel does not take, refuses, named by its position and never
// quoted (oidc.DomainNameRefusal).
func parseWorkspaceDomains(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ",")
	domains := make([]string, len(entries))
	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("%s entry %d of %d is empty", workspaceDomainsSetting, i+1, len(entries))
		}
		ascii, ok := oidc.WorkspaceDomainALabel(entry)
		if !ok {
			return nil, oidc.DomainNameRefusal(fmt.Sprintf("%s entry %d of %d", workspaceDomainsSetting, i+1, len(entries)), entry)
		}
		domains[i] = ascii
	}
	return domains, nil
}

// WorkspaceDomains are the domains the Google Workspace owns, as the edge compares an address's
// domain with them: BROKER_SIGNIN_DOMAIN, lowered and as IDNA lookup writes it, then each
// BROKER_SIGNIN_WORKSPACE_DOMAINS entry, which Load already holds in that form. A broker with
// BROKER_SIGNIN=off names no Workspace and answers none; Load refuses an edge there.
func (c *Config) WorkspaceDomains() []string {
	if c.SignIn.Domain == "" {
		return nil
	}
	domain := strings.ToLower(c.SignIn.Domain)
	if ascii, err := idna.Lookup.ToASCII(domain); err == nil {
		domain = ascii
	}
	return append([]string{domain}, c.SignInWorkspaceDomains...)
}
