//go:build ignore

// live-rig-grants prints, as one JSON object, the NATS grants the daemon's live-proof rig gives
// each user of its nats-server (packages/daemon/internal/daemon/RIG.md): the listener's own user, a
// session that presents no credential, and Dispatch's user. lib/live-rig.sh copies this file into
// the packages/envoy/infra module of the tree of the listener release production pins and runs it
// there, so every set is that release's own code, read where the deployment repository reads it
// (core.ListenerHostPermissions is the set the GKE listener's user carries, envoy-cluster/
// Pulumi.prod.yaml; core.SessionPermissions the hub's no_auth_user, which the issue pods' sessions
// connect as, presenting nothing). The daemon's own user is the head's, read by the driver itself
// (natsgrant.Daemon), because the daemon the rig runs is the head's.
//
// Usage, from the infra module of that tree: go run ./live-rig-grants.go <machine id>
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/infra/core"
)

// rules is one direction of a grant as nats-server's permissions block spells it.
type rules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
}

type grant struct {
	Publish   rules `json:"publish"`
	Subscribe rules `json:"subscribe"`
}

func of(p core.Permissions) grant {
	return grant{
		Publish:   rules{Allow: p.Publish.Allow, Deny: p.Publish.Deny},
		Subscribe: rules{Allow: p.Subscribe.Allow, Deny: p.Subscribe.Deny},
	}
}

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./live-rig-grants.go <machine id>")
		os.Exit(2)
	}
	machine := os.Args[1]
	encoder := json.NewEncoder(os.Stdout)
	// nats-server's configuration reads JSON, but not Go's HTML escaping of '>' as \u003e.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{
		"machine":  machine,
		"listener": of(core.ListenerHostPermissions(machine)),
		"sessions": of(core.SessionPermissions()),
		"dispatch": of(core.DispatchPermissions(machine)),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "live-rig-grants:", err)
		os.Exit(1)
	}
}
