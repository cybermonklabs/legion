package writerhold

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// kvStreamPrefix begins the stream name of every KV bucket: bucket B is stream KV_B.
const kvStreamPrefix = "KV_"

// kvSubjects is the subject space of every KV bucket: bucket B's records are $KV.B.<key>.
const kvSubjects = "$KV.>"

// jsRequest bounds one JetStream API request. A server that does not answer in it is UNKNOWN to
// the step that asked.
const jsRequest = 10 * time.Second

// ProtectedStreams are the streams the hold protects and the preflight reads by name, each one
// whose STREAM.INFO the prepare grant holds: the notification stream, the session consumers'
// records, each copied bucket, the Legion agent registry, the spent DPoP proofs and the webhook
// redelivery records. The names come from internal/bus, never from the grant, so a grant that
// loses one of them leaves the preflight UNKNOWN on it rather than blind to it. Each call returns a
// fresh slice.
func ProtectedStreams() []string {
	names := []string{bus.Stream, kvStreamPrefix + bus.SessionConsumersBucket}
	for _, bucket := range bus.CopiedBuckets() {
		names = append(names, kvStreamPrefix+bucket)
	}
	return append(names,
		kvStreamPrefix+bus.LegionAgentBucket,
		kvStreamPrefix+bus.DPoPProofBucket,
		kvStreamPrefix+bus.WebhookRedeliveryBucket,
	)
}

// isProtectedStream reports whether name is a stream the hold protects: the notification stream
// and every KV bucket's stream, the ones the build names and any other.
func isProtectedStream(name string) bool {
	return name == bus.Stream || strings.HasPrefix(name, kvStreamPrefix)
}

// StreamReading is one protected stream as a step read it by name. Absent is true only for a
// stream the server answered `stream not found` (err_code 10059) for and the workflow declared
// absent; a present stream carries its STREAM.INFO `created` time, `last_seq` and message count,
// which the postcheck compares after the command's last mutation.
type StreamReading struct {
	Name     string    `json:"name"`
	Absent   bool      `json:"absent,omitempty"`
	Created  time.Time `json:"created"`
	LastSeq  uint64    `json:"last_seq"`
	Messages uint64    `json:"messages"`
}

// streamChecks are the checks of this file and of census.go and postcheck.go that a row's control
// removes or weakens, to show the danger each one stops. Production passes the zero value, which
// runs every check; only a row sets a field.
type streamChecks struct {
	// noSources leaves out the refusal of a protected stream with sources or a mirror
	// (TestHoldSources's control).
	noSources bool
	// noSchedules leaves out the refusal of a protected stream with allow_msg_schedules
	// (TestHoldMessageSchedules's control).
	noSchedules bool
	// noDestinations leaves out the republish and subject transform destination checks
	// (TestHoldWritersInsideTheServer's control).
	noDestinations bool
	// rawSpelling replaces the literal-subject test with an overlap check of the destination as
	// it is spelled (TestHoldASubstitutionIsNotLiteral's control).
	rawSpelling bool
	// acceptIncomplete takes a list answer with a `missing` or `offline` list, or a page that adds
	// no entry, as the whole list (TestHoldIncompleteLists's control).
	acceptIncomplete bool
	// restoreNewCreated makes the postcheck demand a new `created` of a restored stream, and
	// restoreSameCreated demand step 1's (TestHoldThePostcheck's controls).
	restoreNewCreated, restoreSameCreated bool
}

// ReadProtectedStreams is the preflight's stream reads (§3a step 1), on nc, the operator's
// connection under the prepare grant: the STREAM.INFO of each of ProtectedStreams, by name.
// expectedAbsent names the protected streams the hold's workflow expects absent (the selection
// bucket for the cutover's first create-bucket, a buckets-create target, and so on); for those
// alone `stream not found` is recorded as expected absence. An absent stream the workflow does not
// declare stops the hold naming it, any other read error is UNKNOWN, and a present stream must
// show no sources, no mirror, no republish, no subject transform and allow_msg_schedules false,
// since a listener holding STREAM.UPDATE on a copied bucket could have set any of them before the
// hold, and each makes the server write the stream with no client. The readings, in
// ProtectedStreams' order, are what the postcheck compares against.
func ReadProtectedStreams(ctx context.Context, nc *nats.Conn, expectedAbsent []string) ([]StreamReading, error) {
	return readProtectedStreams(ctx, nc, expectedAbsent, streamChecks{})
}

func readProtectedStreams(ctx context.Context, nc *nats.Conn, expectedAbsent []string, checks streamChecks) ([]StreamReading, error) {
	names := ProtectedStreams()
	declared := make(map[string]bool, len(expectedAbsent))
	for _, name := range expectedAbsent {
		if !slices.Contains(names, name) {
			return nil, unknownf(StepPreflight, nil, "the workflow declares %s absent, which is not a protected stream the preflight reads", name)
		}
		declared[name] = true
	}
	readings := make([]StreamReading, 0, len(names))
	for _, name := range names {
		info, found, err := streamInfo(ctx, nc, name)
		if err != nil {
			return nil, unknownf(StepPreflight, err, "STREAM.INFO of %s", name)
		}
		if !found {
			if !declared[name] {
				return nil, refusef(StepPreflight, "%s is absent (stream not found, err_code %d), and the hold's workflow does not declare it absent", name, jetstream.JSErrCodeStreamNotFound)
			}
			readings = append(readings, StreamReading{Name: name, Absent: true})
			continue
		}
		if err := checks.serverWriters(StepPreflight, info); err != nil {
			return nil, err
		}
		readings = append(readings, readingOf(info))
	}
	return readings, nil
}

// readingOf is the StreamReading of a present stream's info.
func readingOf(info *jetstream.StreamInfo) StreamReading {
	return StreamReading{Name: info.Config.Name, Created: info.Created, LastSeq: info.State.LastSeq, Messages: info.State.Msgs}
}

// serverWriters refuses, in step, a protected stream that the server itself writes with no client:
// one that sources or mirrors another stream, republishes, transforms its subjects or allows
// message schedules.
func (c streamChecks) serverWriters(step string, info *jetstream.StreamInfo) error {
	cfg := info.Config
	name := cfg.Name
	if !c.noSources {
		if len(cfg.Sources) > 0 {
			return refusef(step, "the protected stream %s sources from %s: the server writes each sourced message into it", name, sourceNames(cfg.Sources))
		}
		if cfg.Mirror != nil {
			return refusef(step, "the protected stream %s mirrors %s: the server writes each mirrored message into it", name, cfg.Mirror.Name)
		}
	}
	if cfg.RePublish != nil {
		return refusef(step, "the protected stream %s republishes %q to %q", name, cfg.RePublish.Source, cfg.RePublish.Destination)
	}
	if cfg.SubjectTransform != nil {
		return refusef(step, "the protected stream %s transforms %q to %q", name, cfg.SubjectTransform.Source, cfg.SubjectTransform.Destination)
	}
	if cfg.AllowMsgSchedules && !c.noSchedules {
		return refusef(step, "the protected stream %s allows message schedules (allow_msg_schedules): a persisted schedule writes its target with no client", name)
	}
	return nil
}

// sourceNames is the names of sources, comma-separated.
func sourceNames(sources []*jetstream.StreamSource) string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		if source != nil {
			names = append(names, source.Name)
		}
	}
	return strings.Join(names, ", ")
}

// streamInfo is the STREAM.INFO of stream name, sent as a raw JetStream API request so the answer's
// err_code is read as the server sent it. found is false for `stream not found` (err_code 10059)
// alone; any other error answer, an answer that does not decode, or one for another stream, is the
// error.
func streamInfo(ctx context.Context, nc *nats.Conn, name string) (*jetstream.StreamInfo, bool, error) {
	var answer struct {
		Error *jetstream.APIError `json:"error,omitempty"`
		jetstream.StreamInfo
	}
	if err := apiRequest(ctx, nc, fmt.Sprintf(natsserver.JSApiStreamInfoT, name), nil, &answer); err != nil {
		return nil, false, err
	}
	if answer.Error != nil {
		if answer.Error.ErrorCode == jetstream.JSErrCodeStreamNotFound {
			return nil, false, nil
		}
		return nil, false, answer.Error
	}
	if answer.Config.Name != name {
		return nil, false, fmt.Errorf("STREAM.INFO of %s answered for stream %q", name, answer.Config.Name)
	}
	return &answer.StreamInfo, true, nil
}

// apiRequest sends request, JSON-encoded (an empty body when nil), to subject on nc and decodes the
// answer into into, within jsRequest.
func apiRequest(ctx context.Context, nc *nats.Conn, subject string, request, into any) error {
	var body []byte
	if request != nil {
		var err error
		if body, err = json.Marshal(request); err != nil {
			return fmt.Errorf("encode the request to %s: %w", subject, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, jsRequest)
	defer cancel()
	msg, err := nc.RequestWithContext(ctx, subject, body)
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := json.Unmarshal(msg.Data, into); err != nil {
		return fmt.Errorf("%s answered a body that does not decode: %w", subject, err)
	}
	return nil
}
