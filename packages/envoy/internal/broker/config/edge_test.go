package config

import (
	"slices"
	"strings"
	"testing"
)

// edgeEnv is validEnv with the broker's edge configured: its listener on 0.0.0.0:13383, the
// address the edge's callers reach it at, and the machine-login page it sends them to.
func edgeEnv() map[string]string {
	e := validEnv()
	e["BROKER_EDGE_LISTEN_ADDR"] = "0.0.0.0:13383"
	e["BROKER_EDGE_URL"] = "https://secrets.edge.example"
	e["BROKER_EDGE_MACHINE_LOGIN_URL"] = "https://dispatch.edge.example/credentials/machine"
	return e
}

// TestTheEdgeAddressSharesNoPortWithTheBrokersOtherListeners: the edge's address is refused when
// its port is the port of the broker's API, its metrics port or its airlock's proxy port, each at
// its default address and at one a deployment names, and the refusal names both settings, since
// an operator reads from it which two collide. 13383, the port the deployment gives the edge, is
// none of them and loads.
//
// The named revert: drop the port comparison from Load, and the 13382 row loads.
func TestTheEdgeAddressSharesNoPortWithTheBrokersOtherListeners(t *testing.T) {
	for _, row := range []struct {
		name, edge string
		set        map[string]string
		other      string
	}{
		{name: "the default metrics address", edge: "0.0.0.0:13382", other: "BROKER_METRICS_LISTEN_ADDR"},
		{name: "the default API address", edge: "0.0.0.0:13380", other: "BROKER_LISTEN_ADDR"},
		{name: "the default airlock address", edge: "0.0.0.0:13381", other: "BROKER_AIRLOCK_LISTEN_ADDR"},
		{name: "a metrics address the deployment names", edge: ":13390", set: map[string]string{"BROKER_METRICS_LISTEN_ADDR": "0.0.0.0:13390"}, other: "BROKER_METRICS_LISTEN_ADDR"},
		{name: "an API address the deployment names", edge: "127.0.0.1:8080", set: map[string]string{"BROKER_LISTEN_ADDR": "0.0.0.0:8080"}, other: "BROKER_LISTEN_ADDR"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := edgeEnv()
			e["BROKER_EDGE_LISTEN_ADDR"] = row.edge
			for name, value := range row.set {
				e[name] = value
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), "BROKER_EDGE_LISTEN_ADDR") || !strings.Contains(err.Error(), row.other) {
				t.Fatalf("Load(BROKER_EDGE_LISTEN_ADDR=%s) = %v, want a refusal naming BROKER_EDGE_LISTEN_ADDR and %s", row.edge, err, row.other)
			}
		})
	}
	cfg, err := Load(env(edgeEnv()))
	if err != nil || cfg.EdgeListenAddr != "0.0.0.0:13383" {
		t.Fatalf("Load(BROKER_EDGE_LISTEN_ADDR=0.0.0.0:13383) = (%q, %v), want the edge on 0.0.0.0:13383", cfg.EdgeListenAddr, err)
	}
	// Port 0 is the kernel's choice of a free port, which a local run gives every listener: two
	// listeners on port 0 never share one, so it is no collision.
	e := edgeEnv()
	e["BROKER_LISTEN_ADDR"], e["BROKER_METRICS_LISTEN_ADDR"], e["BROKER_EDGE_LISTEN_ADDR"] = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	if _, err := Load(env(e)); err != nil {
		t.Fatalf("Load with every listener on port 0 = %v, want it to load", err)
	}
}

// TestTheEdgeSettingsAreAllThreeOrNone: the edge's three settings are set together or not at
// all. With none the broker has no edge, as every broker before it; with some, the start is
// refused naming the ones missing, since an edge with no address of its own, or no page to send
// a person to, could not be reached or followed.
func TestTheEdgeSettingsAreAllThreeOrNone(t *testing.T) {
	settings := []string{"BROKER_EDGE_LISTEN_ADDR", "BROKER_EDGE_URL", "BROKER_EDGE_MACHINE_LOGIN_URL"}
	for _, missing := range settings {
		e := edgeEnv()
		delete(e, missing)
		_, err := Load(env(e))
		if err == nil || !strings.Contains(err.Error(), "missing "+missing) {
			t.Errorf("Load without %s = %v, want a refusal naming it missing", missing, err)
		}
	}
	for _, set := range settings {
		e := validEnv()
		e[set] = edgeEnv()[set]
		_, err := Load(env(e))
		if err == nil {
			t.Errorf("Load with %s alone loaded, want a refusal naming the other two", set)
			continue
		}
		for _, other := range settings {
			if other != set && !strings.Contains(err.Error(), other) {
				t.Errorf("Load with %s alone = %v, want it to name %s missing", set, err, other)
			}
		}
	}
	cfg, err := Load(env(validEnv()))
	if err != nil || cfg.EdgeListenAddr != "" || cfg.EdgeURL != "" || cfg.EdgeMachineLoginURL != "" {
		t.Fatalf("Load with no edge setting = (%+v, %v), want a broker with no edge", cfg, err)
	}
	cfg, err = Load(env(edgeEnv()))
	if err != nil || cfg.EdgeURL != "https://secrets.edge.example" || cfg.EdgeMachineLoginURL != "https://dispatch.edge.example/credentials/machine" {
		t.Fatalf("Load with the edge = (%q, %q, %v), want both URLs as given", cfg.EdgeURL, cfg.EdgeMachineLoginURL, err)
	}
}

// TestTheEdgeSettingsAreHeldToTheirForm: the edge's address is the one its callers' proofs and
// request objects name, so it is held to BROKER_PUBLIC_URL's form and must differ from it; its
// page is an absolute http(s) URL; its listen address is host:port; and it needs the two things
// its routes stand on, the device grant (BROKER_MACHINE_LOGIN_URL) and a Workspace to refuse
// (BROKER_SIGNIN_DOMAIN, which BROKER_SIGNIN=off takes away).
func TestTheEdgeSettingsAreHeldToTheirForm(t *testing.T) {
	for _, row := range []struct {
		name    string
		set     map[string]string
		unset   []string
		refusal string
	}{
		{name: "an edge URL with a path", set: map[string]string{"BROKER_EDGE_URL": "https://secrets.edge.example/v1"}, refusal: "BROKER_EDGE_URL"},
		{name: "an edge URL with no scheme", set: map[string]string{"BROKER_EDGE_URL": "secrets.edge.example"}, refusal: "BROKER_EDGE_URL"},
		{name: "an edge URL of another scheme", set: map[string]string{"BROKER_EDGE_URL": "ftp://secrets.edge.example"}, refusal: "BROKER_EDGE_URL"},
		{name: "the public URL as the edge URL", set: map[string]string{"BROKER_EDGE_URL": "https://secrets.internal.example"}, refusal: "BROKER_PUBLIC_URL"},
		{name: "a relative edge page", set: map[string]string{"BROKER_EDGE_MACHINE_LOGIN_URL": "/credentials/machine"}, refusal: "BROKER_EDGE_MACHINE_LOGIN_URL"},
		{name: "an edge address with no port", set: map[string]string{"BROKER_EDGE_LISTEN_ADDR": "0.0.0.0"}, refusal: "BROKER_EDGE_LISTEN_ADDR"},
		{name: "no device grant", unset: []string{"BROKER_MACHINE_LOGIN_URL"}, refusal: "BROKER_MACHINE_LOGIN_URL"},
		{name: "sign-in off", set: map[string]string{"BROKER_SIGNIN": "off"}, unset: signInSettings, refusal: "BROKER_SIGNIN_DOMAIN"},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := edgeEnv()
			for _, name := range row.unset {
				delete(e, name)
			}
			for name, value := range row.set {
				e[name] = value
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Fatalf("Load with %s = %v, want a refusal naming %s", row.name, err, row.refusal)
			}
		})
	}
}

// TestTheEdgeReadsTheWorkspaceDomains: BROKER_SIGNIN_WORKSPACE_DOMAINS names the domains the
// Workspace owns beside BROKER_SIGNIN_DOMAIN, separated by commas as Dispatch's setting of the
// same name lists them, each a lowercase domain name, an internationalized one in Unicode or as
// its A-labels and kept as its A-labels; an empty entry or one in capitals is refused by its
// position, and the setting needs the sign-in domain it lists the others beside, so
// BROKER_SIGNIN=off refuses it.
func TestTheEdgeReadsTheWorkspaceDomains(t *testing.T) {
	e := edgeEnv()
	e["BROKER_SIGNIN_WORKSPACE_DOMAINS"] = " second.example, bücher.example ,third.example "
	cfg, err := Load(env(e))
	if want := []string{"second.example", "xn--bcher-kva.example", "third.example"}; err != nil || !slices.Equal(cfg.SignInWorkspaceDomains, want) {
		t.Fatalf("Load(BROKER_SIGNIN_WORKSPACE_DOMAINS) = (%q, %v), want %q", cfg.SignInWorkspaceDomains, err, want)
	}
	for _, row := range []struct{ value, refusal string }{
		{"second.example,,third.example", "entry 2 of 3 is empty"},
		{"Second.example", "entry 1 of 1"},
		{"second..example", "entry 1 of 1"},
	} {
		e := edgeEnv()
		e["BROKER_SIGNIN_WORKSPACE_DOMAINS"] = row.value
		if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_WORKSPACE_DOMAINS") || !strings.Contains(err.Error(), row.refusal) {
			t.Errorf("Load(BROKER_SIGNIN_WORKSPACE_DOMAINS=%q) = %v, want a refusal naming the setting and %q", row.value, err, row.refusal)
		}
	}
	off := signInOffEnv()
	off["BROKER_SIGNIN_WORKSPACE_DOMAINS"] = "second.example"
	if _, err := Load(env(off)); err == nil || !strings.Contains(err.Error(), "BROKER_SIGNIN_WORKSPACE_DOMAINS") {
		t.Fatalf("Load(BROKER_SIGNIN=off, BROKER_SIGNIN_WORKSPACE_DOMAINS) = %v, want a refusal naming it", err)
	}
}
