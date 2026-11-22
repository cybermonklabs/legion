package consumernames

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// listingOverlap is how many names each CONSUMER.NAMES page asks for again of the page before
// it: a consumer removed between two pages shifts every later name down by one, and the overlap
// catches a name the shift would otherwise have skipped past.
const listingOverlap = 32

// requestTimeout bounds one JetStream API request whose ctx carries no deadline.
const requestTimeout = 10 * time.Second

// withRequestTimeout is ctx bounded by requestTimeout, with its cancel, when ctx carries no
// deadline; a ctx that carries one comes back as it is, with a cancel that does nothing.
func withRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, requestTimeout)
}

// apiRequest sends one JetStream API request on nc, body marshalled as JSON (nil sends an empty
// body), and answers the answer's body; an answer holding an error is a *jetstream.APIError.
func apiRequest(ctx context.Context, nc *nats.Conn, subject string, body any) (json.RawMessage, error) {
	ctx, cancel := withRequestTimeout(ctx)
	defer cancel()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	reply, err := nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return nil, apiRefusal(nc, subject, fmt.Errorf("%s: %w", subject, err))
	}
	var answer struct {
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		return nil, fmt.Errorf("%s: an answer that does not decode: %w", subject, err)
	}
	if answer.Error != nil {
		return nil, fmt.Errorf("%s: %w", subject, answer.Error)
	}
	return reply.Data, nil
}

// apiRefusal is err, a request to subject on nc that failed, with the server's refusal wrapped in
// when nc recorded one of that publish or of the subscription its answer comes back on. The
// server answers a publish its connection may not make with `-ERR 'Permissions Violation for
// Publish to "<subject>"'` and drops it, and a subscription it may not make with `Permissions
// Violation for Subscription to "<subject>"` (nats-server client.go, each subject written with
// %q), so the request ends unanswered, and nats.go reports the refusal only as the connection's
// last error (nats.Conn.LastError, wrapping nats.ErrPermissionViolation). The answers' subscription
// is under the connection's inbox prefix (nats.CustomInboxPrefix). A refused request is never read
// as a timeout: its error names the refused subject in the server's own words.
func apiRefusal(nc *nats.Conn, subject string, err error) error {
	if err == nil || nc == nil {
		return err
	}
	last := nc.LastError()
	if !errors.Is(last, nats.ErrPermissionViolation) {
		return err
	}
	inbox := cmp.Or(nc.Opts.InboxPrefix, nats.InboxPrefix)
	if !strings.Contains(last.Error(), fmt.Sprintf("Publish to %q", subject)) &&
		!strings.Contains(last.Error(), `Subscription to "`+strings.TrimSuffix(inbox, ".")+".") {
		return err
	}
	return fmt.Errorf("%w; the server refused it: %w", err, last)
}

// connRefusal is err, a command's whole run's, with the server's refusal of a publish wrapped in
// when the run's connection nc recorded one and err does not name it already: whichever request
// the grant refused (a JetStream API call, a KV write, a restore's chunk), the error then names the
// refused subject in the server's words, as apiRefusal names one request's, never a bare timeout.
// backup, create-bucket, replay, convert and repair-sentinel answer their error through it, the
// commands whose requests also go through clients other than apiRequest (kvrecords' walk, nats.go's
// KV and JetStream puts, jsm.go's restore, the selection store's requests).
func connRefusal(nc *nats.Conn, err error) error {
	if err == nil || nc == nil {
		return err
	}
	last := nc.LastError()
	if !errors.Is(last, nats.ErrPermissionViolation) || strings.Contains(err.Error(), last.Error()) {
		return err
	}
	return fmt.Errorf("%w; the server refused a publish of this run: %w", err, last)
}

// apiErrorIs reports whether err holds the JetStream API's answer code (jetstream.JSErrCode*).
func apiErrorIs(err error, code jetstream.ErrorCode) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) && api.ErrorCode == code
}

// consumerNames lists every consumer of stream, through CONSUMER.NAMES pages that each overlap
// the page before by listingOverlap names. The listing ends only on one answer whose offset and
// count reach its own total; a listing that cannot end so is UNKNOWN. The names are sorted.
func consumerNames(ctx context.Context, nc *nats.Conn, stream string) ([]string, error) {
	seen := map[string]bool{}
	offset := 0
	for range 10_000 {
		raw, err := apiRequest(ctx, nc, "$JS.API.CONSUMER.NAMES."+stream, map[string]int{"offset": offset})
		if err != nil {
			return nil, unknownf(err, "the consumer names of %s", stream)
		}
		var page struct {
			Total     int      `json:"total"`
			Offset    int      `json:"offset"`
			Limit     int      `json:"limit"`
			Consumers []string `json:"consumers"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, unknownf(err, "a page of %s's consumer names", stream)
		}
		for _, name := range page.Consumers {
			seen[name] = true
		}
		if page.Offset+len(page.Consumers) >= page.Total {
			if page.Offset+len(page.Consumers) != page.Total {
				return nil, unknownf(nil, "a page of %s's consumer names at offset %d holds %d names past its total %d", stream, page.Offset, len(page.Consumers), page.Total)
			}
			names := make([]string, 0, len(seen))
			for name := range seen {
				names = append(names, name)
			}
			slices.Sort(names)
			return names, nil
		}
		if len(page.Consumers) == 0 {
			return nil, unknownf(nil, "an empty page of %s's consumer names at offset %d of %d", stream, page.Offset, page.Total)
		}
		next := page.Offset + len(page.Consumers) - listingOverlap
		if next <= offset {
			next = page.Offset + len(page.Consumers)
		}
		offset = next
	}
	return nil, unknownf(nil, "the consumer names of %s did not end", stream)
}

// consumerInfo reads consumer name of stream, and answers nil with no error for one the server
// does not hold.
func consumerInfo(ctx context.Context, nc *nats.Conn, stream, name string) (*jetstream.ConsumerInfo, error) {
	raw, err := apiRequest(ctx, nc, "$JS.API.CONSUMER.INFO."+stream+"."+name, nil)
	if apiErrorIs(err, jetstream.JSErrCodeConsumerNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info jetstream.ConsumerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("the info of %s on %s: %w", name, stream, err)
	}
	return &info, nil
}

// consumerRequest is the body of a CONSUMER.CREATE request, the form of session_consumers.go's
// consumerAPI: a named create or update with its action, never the filter-subject subject, which
// the operator's grant does not name.
type consumerRequest struct {
	Stream string                   `json:"stream_name"`
	Config jetstream.ConsumerConfig `json:"config"`
	Action string                   `json:"action,omitempty"`
}

// The CONSUMER.CREATE actions.
const (
	actionCreateConsumer = "create"
	actionUpdateConsumer = "update"
)

// writeConsumer sends config's create or update (action) on stream, and answers the consumer's
// info. The subject is $JS.API.CONSUMER.CREATE.<stream>.<name>, the one form the operator's grant
// names.
func writeConsumer(ctx context.Context, nc *nats.Conn, stream string, config jetstream.ConsumerConfig, action string) (*jetstream.ConsumerInfo, error) {
	name := config.Durable
	if name == "" {
		name = config.Name
	}
	raw, err := apiRequest(ctx, nc, "$JS.API.CONSUMER.CREATE."+stream+"."+name, consumerRequest{Stream: stream, Config: config, Action: action})
	if err != nil {
		return nil, err
	}
	var info jetstream.ConsumerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("the answer to %s's %s on %s: %w", name, action, stream, err)
	}
	return &info, nil
}

// deleteConsumer deletes consumer name of stream; one the server does not hold is no error.
func deleteConsumer(ctx context.Context, nc *nats.Conn, stream, name string) error {
	_, err := apiRequest(ctx, nc, "$JS.API.CONSUMER.DELETE."+stream+"."+name, nil)
	if apiErrorIs(err, jetstream.JSErrCodeConsumerNotFound) {
		return nil
	}
	return err
}

// streamInfo reads stream's info.
func streamInfo(ctx context.Context, nc *nats.Conn, stream string) (*jetstream.StreamInfo, error) {
	raw, err := apiRequest(ctx, nc, "$JS.API.STREAM.INFO."+stream, nil)
	if err != nil {
		return nil, err
	}
	var info jetstream.StreamInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("the info of %s: %w", stream, err)
	}
	return &info, nil
}

// notifications is the notification stream every family's consumers read.
const notifications = bus.Stream
