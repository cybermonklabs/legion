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
// listener serves. With any missing, it reads each --connz server's connection list and refuses
// while it lists any client but this command's own connection, so no listener runs at the create;
// then it creates each missing one, with its copied-bucket configuration at replicas copies, after
// one line on standard error naming the operator, the server, the bucket and that the run is
// break-glass, and leaves each one there as it is. A read that answers anything but "not found"
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
	exists := map[string]bool{}
	for _, bucket := range o.buckets {
		stream := "KV_" + bucket
		if _, err := js.StreamInfo(stream); err == nil {
			present = append(present, stream)
			exists[bucket] = true
		} else if !errors.Is(err, nats.ErrStreamNotFound) {
			refusals = append(refusals, fmt.Sprintf("STREAM.INFO %s did not answer that it is missing: %v", stream, err))
		}
	}
	if len(refusals) == 0 && len(present) == len(o.buckets) {
		fmt.Fprintf(stdout, "every bucket named exists (%s): nothing to create\n", strings.Join(o.buckets, ", "))
		return 0
	}
	if len(refusals) > 0 {
		fmt.Fprintln(stderr, "envoy-infra buckets create --break-glass: refused: it creates only buckets the server reports missing, and none at all while any --bucket's read is not whole")
		for _, refusal := range refusals {
			fmt.Fprintf(stderr, "  refused: %s\n", refusal)
		}
		return 1
	}
	sawOwn := false
	for _, url := range o.connz {
		others, ownServer := otherClients(ctx, client, url, nc.Conn)
		refusals = append(refusals, others...)
		sawOwn = sawOwn || ownServer
	}
	if !sawOwn {
		refusals = append(refusals, fmt.Sprintf("no --connz list is server %s's, the one the command is connected to, so the command cannot see its own connection there: name every server's monitoring URL", nc.Conn.ConnectedServerId()))
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
		if exists[bucket] {
			fmt.Fprintf(stdout, "%s exists; left as it is\n", bucket)
			continue
		}
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
// command's connection: on the server own is connected to, every connection but own's; on any other
// server (each member of a cluster, read through its own --connz), every connection. It reports
// whether the list is own server's, which must list own's connection. A list that cannot be read
// whole is named too: a server that cannot show it is alone refuses.
func otherClients(ctx context.Context, client *http.Client, url string, own *nats.Conn) (others []string, ownServer bool) {
	base := strings.TrimSuffix(url, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/connz?auth=1&limit=%d", base, connzPage), nil)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", url, err)}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return []string{fmt.Sprintf("%s: the connection list could not be read: %v", url, err)}, false
	}
	defer func() { _ = resp.Body.Close() }()
	var list connz
	if resp.StatusCode != http.StatusOK {
		return []string{fmt.Sprintf("%s: the connection list answered %d", url, resp.StatusCode)}, false
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&list); err != nil {
		return []string{fmt.Sprintf("%s: the connection list does not decode: %v", url, err)}, false
	}
	if list.Total > len(list.Connections) {
		return []string{fmt.Sprintf("%s: the connection list holds %d connections, more than one page of %d", url, list.Total, connzPage)}, false
	}
	cid, err := own.GetClientID()
	if err != nil {
		return []string{fmt.Sprintf("the command's own client id cannot be read: %v", err)}, false
	}
	ownServer = list.ServerID == own.ConnectedServerId()
	seen := false
	for _, c := range list.Connections {
		if ownServer && c.CID == cid {
			seen = true
			continue
		}
		others = append(others, fmt.Sprintf("connection %d (name %q, user %q) on server %s", c.CID, c.Name, c.AuthorizedUser, list.ServerID))
	}
	if ownServer && !seen {
		others = append(others, fmt.Sprintf("%s does not list the command's own connection %d: the list cannot show the server is alone", url, cid))
	}
	return others, ownServer
}
