// Package writerhold is the NATS writer hold, `envoy-infra hold`: while a hold is set, no client
// but one operator process and the auth service is connected to any server of the clustered hub,
// no other client can connect, and every declared server has restarted since the hold began. So no
// work a client left inside a server can land after the hold's listings, and a held command
// (envoy-infra consumer-names, envoy-infra streams, envoy-infra buckets) changes the hub's streams
// with no other writer.
//
// The hold row and its journal live in the secrets broker's Postgres, signed by the operator's key,
// and are read and written through internal/holdrecord alone; nothing the hold trusts is in NATS.
// The auth service reads the row at every decision and, while it holds held, refuses every client
// but the operator (natscallout's outsideTheHold). This package runs the hold's steps against the
// hub itself, each a reading of the hub's own state:
//
//  1. the preflight, under the prepare grant: the render the hub runs (its ConfigMap and
//     StatefulSet), the minter report, each declared server's monitoring port (/varz, /routez,
//     /leafz, /gatewayz), and each protected stream's info;
//  2. the row moved to held, then the probe's refusal and the writer census: no stream or consumer
//     inside the server that writes what the hold protects;
//  3. the restart of every declared server, one eviction at a time under the disruption budget;
//  4. the proof of each restart: a new server id, the pinned image and revision, the claim on its
//     recorded volume, the probe refused on that server;
//  5. the meta barrier, a stream created and listed through the meta leader;
//  6. the report: every connection of every server is the caller's own or the auth service's.
//
// The hold reaches each server's monitoring port and client port through the Kubernetes API
// server's port-forward (Forwarder), since the hub's NetworkPolicy admits no pod to them.
package writerhold
