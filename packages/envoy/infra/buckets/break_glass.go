package buckets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// connzPage bounds one read of a server's connection list.
const connzPage = 1024

// connz is the part of a server's /connz the break-glass create reads.
type connz struct {
	ServerID    string `json:"server_id"`
	Total       int    `json:"total"`
	Connections []struct {
		CID            uint64 `json:"cid"`
		Name           string `json:"name"`
		AuthorizedUser string `json:"authorized_user"`
	} `json:"connections"`
}

// breakGlassCreate is `create --break-glass`, for one machine with no auth service, broker or hold.
// It connects as the user the environment names (NATS_NKEY_SEED_FILE or NATS_NKEY_SEED) and reads
// each of the buckets' streams (STREAM.INFO KV_<bucket>). With every one there it changes nothing
// and exits 0, so every `up` of the single-node compose file after its first passes while the
// listener serves. With every one missing, it reads each --connz server's connection list and
// refuses while it lists any client but this command's own connection, so no listener runs at the
// create; then it creates each, with its copied-bucket configuration at replicas copies, after one
// line on standard error naming the operator, the server, the bucket and that the run is
// break-glass. Some there and some missing, or a read that answers anything but "not found",
// refuses the run, and no bucket is created.
//
// It only creates: each create is bus.CreateMissingCopiedBucket's, which answers
// bus.ErrBucketExists, opening, raising and updating nothing, for a bucket another client made after
// the read. The run then stops there, refused, and creates no bucket after it.
func breakGlassCreate(ctx context.Context, stdout, stderr io.Writer, o options, client *http.Client) int {
	natsURL := o.NATS[0]
	nc, err := bus.Connect([]string{natsURL}, bus.WithName("envoy-infra-buckets-break-glass"), bus.WithEnvironment(operatorEnvironment))
	if err != nil {
		fmt.Fprintf(stderr, "envoy-infra buckets: connect to %s: %v\n", bus.NATSURLWithoutUserinfo(natsURL), err)
		return 1
	}
	defer nc.Close()
	js := nc.JS()
	var refusals, present []string
	for _, bucket := range o.buckets {
		stream := "KV_" + bucket
		if _, err := js.StreamInfo(stream); err == nil {
			present = append(present, stream)
		} else if !errors.Is(err, nats.ErrStreamNotFound) {
			refusals = append(refusals, fmt.Sprintf("STREAM.INFO %s did not answer that it is missing: %v", stream, err))
		}
	}
	if len(refusals) == 0 && len(present) == len(o.buckets) {
		fmt.Fprintf(stdout, "every bucket named exists (%s): nothing to create\n", strings.Join(o.buckets, ", "))
		return 0
	}
	for _, stream := range present {
		refusals = append(refusals, stream+" exists")
	}
	if len(refusals) > 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets create --break-glass: refused: it creates only buckets the server reports missing, and none at all while any --bucket is there or unread")
		for _, refusal := range refusals {
			fmt.Fprintf(stderr, "  refused: %s\n", refusal)
		}
		return 1
	}
	for _, url := range o.connz {
		refusals = append(refusals, otherClients(ctx, client, url, nc.Conn)...)
	}
	if len(refusals) > 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets create --break-glass: refused: it creates only while the server has no client but this one, so no listener runs at the create")
		for _, refusal := range refusals {
			fmt.Fprintf(stderr, "  refused: %s\n", refusal)
		}
		return 1
	}
	hub := bus.NATSURLWithoutUserinfo(natsURL)
	for _, bucket := range o.buckets {
		fmt.Fprintf(stderr, "envoy-infra buckets: break-glass: operator %s creates %s on %s, as the environment's NATS user\n", o.operator, bucket, hub)
		if _, err := bus.CreateMissingCopiedBucket(ctx, nc.Conn, js, bucket, o.replicas); errors.Is(err, bus.ErrBucketExists) {
			fmt.Fprintf(stderr, "envoy-infra buckets create --break-glass: refused: KV_%s was made by another client after the read, and is left as it is: %v\n", bucket, err)
			return 1
		} else if err != nil {
			fmt.Fprintf(stderr, "envoy-infra buckets: create %s: %v\n", bucket, err)
			return 1
		}
		fmt.Fprintf(stdout, "created %s (%d copies)\n", bucket, o.replicas)
	}
	return 0
}

// otherClients reads the connection list at url and names each connection that is not own's, the
// command's connection, by its server id and client id. A list that cannot be read whole, or that
// is another server's than own's, is named too: a server that cannot show it is alone refuses.
func otherClients(ctx context.Context, client *http.Client, url string, own *nats.Conn) []string {
	base := strings.TrimSuffix(url, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/connz?auth=1&limit=%d", base, connzPage), nil)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", url, err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return []string{fmt.Sprintf("%s: the connection list could not be read: %v", url, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	var list connz
	if resp.StatusCode != http.StatusOK {
		return []string{fmt.Sprintf("%s: the connection list answered %d", url, resp.StatusCode)}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&list); err != nil {
		return []string{fmt.Sprintf("%s: the connection list does not decode: %v", url, err)}
	}
	if list.Total > len(list.Connections) {
		return []string{fmt.Sprintf("%s: the connection list holds %d connections, more than one page of %d", url, list.Total, connzPage)}
	}
	serverID := own.ConnectedServerId()
	cid, err := own.GetClientID()
	if err != nil {
		return []string{fmt.Sprintf("the command's own client id cannot be read: %v", err)}
	}
	if list.ServerID != serverID {
		return []string{fmt.Sprintf("%s is server %s's list, and the command is connected to %s: it cannot see its own connection there", url, list.ServerID, serverID)}
	}
	var others []string
	seen := false
	for _, c := range list.Connections {
		if c.CID == cid {
			seen = true
			continue
		}
		others = append(others, fmt.Sprintf("connection %d (name %q, user %q) on server %s", c.CID, c.Name, c.AuthorizedUser, list.ServerID))
	}
	if !seen {
		others = append(others, fmt.Sprintf("%s does not list the command's own connection %d: the list cannot show the server is alone", url, cid))
	}
	return others
}
