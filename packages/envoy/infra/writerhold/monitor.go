package writerhold

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Monitor is one declared server's HTTP monitoring port.
type Monitor interface {
	// Get reads path, with its query, and decodes the JSON answer into into.
	Get(ctx context.Context, path string, into any) error
}

// monitorRead bounds one read of a monitoring endpoint. A server that does not answer in it is
// UNKNOWN to the step that asked.
const monitorRead = 10 * time.Second

// maxMonitorAnswer bounds an answer's body: the largest is a /connz page of 1,024 connections.
const maxMonitorAnswer = 64 << 20

// httpMonitor reads the monitoring port at base, http://<host>:<port>.
type httpMonitor struct {
	base   string
	client *http.Client
}

// NewHTTPMonitor is the Monitor of the monitoring port at base, http://<host>:<port>, read through
// client (http.DefaultClient when nil).
func NewHTTPMonitor(base string, client *http.Client) Monitor {
	if client == nil {
		client = http.DefaultClient
	}
	return httpMonitor{base: strings.TrimSuffix(base, "/"), client: client}
}

func (m httpMonitor) Get(ctx context.Context, path string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, monitorRead)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMonitorAnswer))
	if err != nil {
		return fmt.Errorf("read the answer to %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		text := strings.TrimSpace(string(body))
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("%s answered %d: %s", path, resp.StatusCode, text)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s answered a body that does not decode: %w", path, err)
	}
	return nil
}

// Varz is the part of a server's /varz the hold reads.
type Varz struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	Version    string `json:"version"`
	// ConfigDigest is the digest of the configuration the server runs (nats-server's
	// conf.ParseFileWithChecksDigest over its configuration file).
	ConfigDigest string `json:"config_digest"`
	// AuthTimeout is the server's authorization timeout, in seconds.
	AuthTimeout float64 `json:"auth_timeout"`
}

// Route is one route of a server's /routez.
type Route struct {
	RID        uint64 `json:"rid"`
	RemoteID   string `json:"remote_id"`
	RemoteName string `json:"remote_name"`
}

// Routez is the part of a server's /routez the hold reads.
type Routez struct {
	ServerID string  `json:"server_id"`
	Routes   []Route `json:"routes"`
}

// Leafz is the part of a server's /leafz the hold reads.
type Leafz struct {
	Leafnodes int `json:"leafnodes"`
}

// Gatewayz is the part of a server's /gatewayz the hold reads.
type Gatewayz struct {
	Name     string                     `json:"name"`
	Outbound map[string]json.RawMessage `json:"outbound_gateways"`
	Inbound  map[string]json.RawMessage `json:"inbound_gateways"`
}

// ConnInfo is the part of one /connz connection the hold reads. /connz leaves Account empty for a
// connection in $G (nats-server v2.15.1 monitor.go:471-473), so an empty Account is $G.
type ConnInfo struct {
	CID            uint64 `json:"cid"`
	Name           string `json:"name"`
	AuthorizedUser string `json:"authorized_user"`
	Account        string `json:"account"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
}

// Connz is one /connz page.
type Connz struct {
	ServerID    string     `json:"server_id"`
	Total       int        `json:"total"`
	Offset      int        `json:"offset"`
	Limit       int        `json:"limit"`
	Connections []ConnInfo `json:"connections"`
}

// RaftzGroup is the part of one raft group of /raftz the hold reads.
type RaftzGroup struct {
	Term   uint64 `json:"term"`
	Leader string `json:"leader"`
}

// Raftz is /raftz: each account's raft groups, by group name.
type Raftz map[string]map[string]RaftzGroup
