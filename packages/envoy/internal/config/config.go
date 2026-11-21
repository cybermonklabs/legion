package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/listenfd"
)

type Service struct {
	MachineID string
	NATSURLs  []string
	// NATSReplicas is ENVOY_NATS_REPLICAS (bus.ParseNATSReplicas): the deployment's replica count
	// for the stream and every bucket the listener opens, which the listener raises them to at
	// start and every keeper period after (bus.ConnectOwningStream).
	NATSReplicas int
	// StreamMaxBytes is ENVOY_STREAM_MAX_BYTES (bus.ParseStreamMaxBytes): the stream's byte limit,
	// or 0 when unset, which sets none and keeps a deployed one.
	StreamMaxBytes int64
	// SessionMaxAge is ENVOY_SESSION_MAX_AGE (bus.ParseSessionMaxAge): how long the session
	// bucket keeps a registration record, or 0 when unset, which leaves the deployed bucket's own
	// maximum age alone. The listener applies it at open and on every pass of its session reaper.
	SessionMaxAge time.Duration
	// ListenHost and Port are the address the listener serves on: the inherited socket's own
	// address with ENVOY_LISTEN_FD, else the one main binds, from ENVOY_LISTEN_HOST (127.0.0.1 when
	// unset) and PORT (Load's default when unset). The API auth fence judges ListenHost, so what it
	// judges is what serves.
	ListenHost string
	Port       int
	// ListenSocket is the listening socket ENVOY_LISTEN_FD names, inspected but not yet adopted:
	// main serves on it in place of binding ListenAddress. Nil when the variable is unset.
	ListenSocket *listenfd.Socket
	// AllowedHosts are the ENVOY_ALLOWED_HOSTS names, each as HostName writes it, that an
	// unauthenticated listener serves /v1 to beside loopback.
	AllowedHosts []string
	// EdgeProjects is ENVOY_EDGE_PROJECTS (parseEdgeProjects): the project keys an edge listener
	// serves, sorted, each once. Non-empty is edge mode, in which the listener runs under the edge
	// listener's NATS grant for these projects (natsgrant.Edge; cmd/listener's edge_nats.go) and
	// /v1 admits only a broker token whose projects are all among these (cmd/listener's
	// EdgeAdmits). Empty, the variable unset or blank, is every other listener, which reads
	// nothing of it.
	EdgeProjects []string
	// PrivacyRequired is ENVOY_PROJECT_PRIVACY_REQUIRED=true, the listener's required mode: every
	// decision a project's privacy turns on reads it afresh from PrivacyURL, a failed or incomplete
	// read is unknown for every project and holds, and each bus client's connection name ends in
	// the suffix a tagged hub admits (cmd/listener, project_privacy.go). False is the all-public
	// mode, set by leaving both variables unset: the listener makes no privacy read and every
	// project is public, as before either variable existed. parsePrivacyMode refuses every other
	// combination, so a failure never selects the all-public path.
	PrivacyRequired bool
	// PrivacyURL is ENVOY_PROJECT_PRIVACY_URL, the secrets broker's base URL, whose
	// /v1/project-privacy the required mode reads; empty in the all-public mode.
	PrivacyURL string
}

func (s Service) ListenAddress() string {
	return net.JoinHostPort(s.ListenHost, strconv.Itoa(s.Port))
}

// Load reads the listener's configuration from its environment, with defaultPort the port when
// PORT is unset. With ENVOY_LISTEN_FD it inspects the inherited socket that names, and
// ENVOY_LISTEN_HOST and PORT, each only when set, must name the socket's address: no default stands
// in for one that is unset, and PORT=0 names no socket. It only inspects the socket; main adopts
// it.
func Load(defaultPort int) (Service, error) {
	machine := strings.TrimSpace(os.Getenv("ENVOY_MACHINE_ID"))
	if machine == "" {
		return Service{}, fmt.Errorf("ENVOY_MACHINE_ID is required")
	}
	raw := strings.TrimSpace(os.Getenv("NATS_URLS"))
	if raw == "" {
		return Service{}, fmt.Errorf("NATS_URLS is required")
	}
	urls := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' })
	for i, item := range urls {
		urls[i] = strings.TrimSpace(item)
	}
	port := defaultPort
	portValue := strings.TrimSpace(os.Getenv("PORT"))
	if portValue != "" {
		next, err := strconv.Atoi(portValue)
		if err != nil {
			return Service{}, fmt.Errorf("invalid PORT: %w", err)
		}
		port = next
	}
	hostValue := strings.TrimSpace(os.Getenv("ENVOY_LISTEN_HOST"))
	listenHost := hostValue
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	var listenSocket *listenfd.Socket
	if descriptor, set := os.LookupEnv("ENVOY_LISTEN_FD"); set {
		socket, err := listenfd.Inspect("ENVOY_LISTEN_FD", descriptor)
		if err != nil {
			return Service{}, err
		}
		// An empty value is unset here, as above: an empty ENVOY_LISTEN_HOST means 127.0.0.1, not
		// every interface.
		if err := socket.CheckAgainst(
			listenfd.Setting{Name: "ENVOY_LISTEN_HOST", Value: hostValue, Set: hostValue != ""},
			listenfd.Setting{Name: "PORT", Value: portValue, Set: portValue != ""},
		); err != nil {
			return Service{}, err
		}
		listenHost, port, listenSocket = socket.Addr().IP.String(), socket.Addr().Port, &socket
	}
	replicas, err := bus.ParseNATSReplicas(os.Getenv(bus.ReplicasEnvVar))
	if err != nil {
		return Service{}, err
	}
	streamMaxBytes, err := bus.ParseStreamMaxBytes(os.Getenv(bus.StreamMaxBytesEnvVar))
	if err != nil {
		return Service{}, err
	}
	sessionMaxAge, err := bus.ParseSessionMaxAge(os.Getenv(bus.SessionMaxAgeEnvVar))
	if err != nil {
		return Service{}, err
	}
	allowedHosts, err := parseAllowedHosts(os.Getenv("ENVOY_ALLOWED_HOSTS"))
	if err != nil {
		return Service{}, err
	}
	edgeProjects, err := parseEdgeProjects(os.Getenv(EdgeProjectsEnvVar))
	if err != nil {
		return Service{}, err
	}
	privacyRequired, privacyURL, err := parsePrivacyMode(os.Getenv)
	if err != nil {
		return Service{}, err
	}
	return Service{
		AllowedHosts:    allowedHosts,
		MachineID:       machine,
		NATSURLs:        urls,
		NATSReplicas:    replicas,
		StreamMaxBytes:  streamMaxBytes,
		SessionMaxAge:   sessionMaxAge,
		ListenHost:      listenHost,
		Port:            port,
		ListenSocket:    listenSocket,
		EdgeProjects:    edgeProjects,
		PrivacyRequired: privacyRequired,
		PrivacyURL:      privacyURL,
	}, nil
}

// The two variables of the listener's privacy modes (Service.PrivacyRequired, Service.PrivacyURL),
// and the listener's two platform identities the required mode reads its privacy source as: the
// names cmd/listener's broker-token settings read (brokerauth.go, EnvBrokerAccountTokenFile and
// EnvBrokerGoogleAudience).
const (
	PrivacyRequiredEnvVar         = "ENVOY_PROJECT_PRIVACY_REQUIRED"
	PrivacyURLEnvVar              = "ENVOY_PROJECT_PRIVACY_URL"
	privacyAccountTokenFileEnvVar = "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"
	privacyGoogleAudienceEnvVar   = "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE"
	privacyRequiredValue          = "true"
)

// parsePrivacyMode reads the listener's privacy mode: both variables unset (or blank) is the
// all-public mode; ENVOY_PROJECT_PRIVACY_REQUIRED=true with ENVOY_PROJECT_PRIVACY_URL naming the
// broker's absolute http(s) base URL and one of the listener's platform identities set is the
// required mode. Every other combination is refused, naming the variable: the flag with a blank
// URL, the flag with no platform identity to read the source as, a URL without the flag, and any
// value of the flag but true. So a required-mode listener with an incomplete configuration exits
// before it serves, and only both variables absent select the all-public path.
func parsePrivacyMode(getenv func(string) string) (required bool, source string, err error) {
	flag := strings.TrimSpace(getenv(PrivacyRequiredEnvVar))
	source = strings.TrimSpace(getenv(PrivacyURLEnvVar))
	switch {
	case flag == "" && source == "":
		return false, "", nil
	case flag == "":
		return false, "", fmt.Errorf("%s is set and %s is not: the privacy source is read only in the required mode, so set %s=%s or unset %s",
			PrivacyURLEnvVar, PrivacyRequiredEnvVar, PrivacyRequiredEnvVar, privacyRequiredValue, PrivacyURLEnvVar)
	case flag != privacyRequiredValue:
		return false, "", fmt.Errorf("invalid %s %q: the one value is %s, and the all-public mode is both %s and %s unset",
			PrivacyRequiredEnvVar, flag, privacyRequiredValue, PrivacyRequiredEnvVar, PrivacyURLEnvVar)
	case source == "":
		return false, "", fmt.Errorf("%s=%s needs %s, the secrets broker's base URL the listener reads each project's privacy from",
			PrivacyRequiredEnvVar, privacyRequiredValue, PrivacyURLEnvVar)
	}
	parsed, parseErr := url.Parse(source)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false, "", fmt.Errorf("invalid %s %q: it names the secrets broker's base URL, an absolute http or https URL with no query", PrivacyURLEnvVar, source)
	}
	if strings.TrimSpace(getenv(privacyAccountTokenFileEnvVar)) == "" && strings.TrimSpace(getenv(privacyGoogleAudienceEnvVar)) == "" {
		return false, "", fmt.Errorf("%s=%s needs %s or %s: the listener reads %s as its own platform identity",
			PrivacyRequiredEnvVar, privacyRequiredValue, privacyAccountTokenFileEnvVar, privacyGoogleAudienceEnvVar, PrivacyURLEnvVar)
	}
	return true, source, nil
}

// hostEntry is an ENVOY_ALLOWED_HOSTS entry's shape: a name or an IPv4 address, or an IPv6
// address with or without its brackets, and never a port, a scheme or a path.
var hostEntry = regexp.MustCompile(`^(\[[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*\]|[0-9A-Fa-f:.]+|[A-Za-z0-9.-]+)$`)

// dnsName is a host name HostName has normalized: dot-separated labels of letters, digits and
// inner hyphens.
var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// parseAllowedHosts reads ENVOY_ALLOWED_HOSTS: comma-separated host names or addresses, blank
// entries skipped. An entry that is not a host name or an IP address (one carrying a port, a
// scheme, a path or a stray bracket) is refused naming it, since a Host is compared by its name
// alone.
//
// CheckAllowedHosts in github.com/sjawhar/envoy/infra/core carries this rule by hand on the
// plugin's side, so a component is refused before it deploys a listener that would not boot, and
// it is deliberately stricter: it applies the shape rule to the entry as written, so it refuses a
// padded or an empty entry this function trims or skips, because it renders the list comma-joined
// into the unit's docker run argument. Every entry refused here is refused there too, and
// packages/envoy/infra/core/allowed_hosts_agreement_test.go runs the same entries through both
// rules and pins that asymmetry.
func parseAllowedHosts(raw string) ([]string, error) {
	var hosts []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, _, withPort := net.SplitHostPort(entry)
		name := HostName(entry)
		if withPort == nil || !hostEntry.MatchString(entry) || (net.ParseIP(name) == nil && !dnsName.MatchString(name)) {
			return nil, fmt.Errorf("invalid ENVOY_ALLOWED_HOSTS entry %q: want a host name or address with no port, scheme or path", entry)
		}
		hosts = append(hosts, name)
	}
	return hosts, nil
}

// HostName is a Host header's or a host entry's name as the listener compares it: the port
// stripped, the brackets taken off an IPv6 address, ASCII letters lower-cased (fold.ASCII), and one
// trailing dot dropped, since DNS reads "name." as "name". Only ASCII is folded, as DNS folds it,
// so no non-ASCII letter (U+0130 lower-cases to "i") can spell an allowed name.
func HostName(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(fold.ASCII(host), ".")
}

// EdgeProjectsEnvVar names the projects an edge listener serves (Service.EdgeProjects).
const EdgeProjectsEnvVar = "ENVOY_EDGE_PROJECTS"

// parseEdgeProjects reads ENVOY_EDGE_PROJECTS: project keys separated by commas or white space,
// read by the rule a broker token's own list is read by (oauthwire.ParseProjects): sorted, each
// once, at most oauthwire.MaxProjects, and nil when empty. A key that is not a project key, a key
// named twice and a longer list are refused, naming the variable, and so is a value other than
// the empty string that names no project (a lone comma, white space): that is a misspelt edge
// setting, and a listener started from it would serve with no edge rules.
func parseEdgeProjects(raw string) ([]string, error) {
	projects, err := oauthwire.ParseProjects(strings.ReplaceAll(raw, ",", " "))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EdgeProjectsEnvVar, err)
	}
	if raw != "" && len(projects) == 0 {
		return nil, fmt.Errorf("%s=%q names no project: set it to the project keys the edge listener serves, or to the empty string for a listener that is no edge", EdgeProjectsEnvVar, raw)
	}
	return projects, nil
}
