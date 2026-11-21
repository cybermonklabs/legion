package streams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// SessionConsumerRecordsBucket is the bucket that records each listener session consumer's name
// and shape, which the census reads, after its barrier on the bucket, for the session consumers
// it keeps.
const SessionConsumerRecordsBucket = "envoy_session_consumers"

// recordsStream is the records bucket's stream, which the census keeps: it is the hold's own, made
// by the writer hold's create-bucket step, and not of the declared set the build configures.
const recordsStream = "KV_" + SessionConsumerRecordsBucket

// CutoverDone is a family's cutover row's state once the writer hold's convert has moved it.
const CutoverDone = "done"

// ConsumerShape is one form of consumer the declared set names, compared by its whole
// configuration and never by name alone: its stream, its name, its filter subjects, where it
// delivers, its deliver group and its ack policy.
type ConsumerShape struct {
	// Stream is the stream the consumer reads.
	Stream string
	// Name is the consumer's durable name. A consumer named Name, or Name~<nuid>, the form
	// the writer hold's convert gives it, takes this shape. Empty for an ordered watcher (Ordered).
	Name string
	// Ordered is a KV watcher's ordered consumer, which the server names: unnamed of its own, it
	// acknowledges nothing, delivers to a reply inbox, filters within its bucket's subjects and is
	// removed by the server once it is inactive.
	Ordered bool
	// FilterSubjects are the consumer's filter subjects, in any order.
	FilterSubjects []string
	// Push is a consumer that delivers to a reply inbox the server made for it (nats.NewInbox,
	// _INBOX.<nuid>); a consumer with no deliver subject is a pull consumer.
	Push bool
	// DeliverGroup is a push consumer's deliver group, empty for none.
	DeliverGroup string
	// AckPolicy is the consumer's ack policy.
	AckPolicy nats.AckPolicy
}

// ConsumerSet is what a deployment's declared consumers are built from, beyond the code's own
// names: the machine id of each of its listeners, and each Legion daemon's consumers, which plan C
// names legion-go-<project>-<kind>.
type ConsumerSet struct {
	ListenerMachineIDs []string
	Daemons            []ConsumerShape
}

// DeclaredConsumers are the consumer shapes the declared set names but the recorded session
// consumers: each listener's durable (bus.ListenerDurable), a push consumer of every notification
// subject the build carries, acknowledged one at a time; Dispatch's two durables
// (delivery.ConsumerConfigs); set's daemons'; and on each declared bucket, its watchers' ordered
// consumers.
func DeclaredConsumers(set ConsumerSet) []ConsumerShape {
	var shapes []ConsumerShape
	for _, stream := range streamset.Declared(streamset.Settings{}) {
		if stream.Bucket != nil {
			shapes = append(shapes, ConsumerShape{Stream: stream.Name, Ordered: true})
		}
	}
	shapes = append(shapes, ConsumerShape{Stream: recordsStream, Ordered: true})
	for _, machineID := range set.ListenerMachineIDs {
		shapes = append(shapes, ConsumerShape{
			Stream: bus.Stream, Name: bus.ListenerDurable(machineID), FilterSubjects: bus.StreamSubjects(),
			Push: true, AckPolicy: nats.AckExplicitPolicy,
		})
	}
	for _, config := range delivery.ConsumerConfigs() {
		shapes = append(shapes, ConsumerShape{
			Stream: bus.Stream, Name: config.Durable, FilterSubjects: filterSubjects(config),
			Push: config.DeliverSubject != "", DeliverGroup: config.DeliverGroup, AckPolicy: config.AckPolicy,
		})
	}
	return append(shapes, set.Daemons...)
}

// CensusInput is what a census reads beside the hub.
type CensusInput struct {
	// Settings are the deployment's settings, which the declared set's configuration takes.
	Settings streamset.Settings
	// Phase reads the hold row's phase: the census runs only while it is held.
	Phase Phase
	// Families are the cutover families the declared set names: session, dispatch, and
	// daemon/<project key> for each daemon. Cutovers reads each one's row, verified
	// (holdrecord.Client.Cutovers), family to state: the census runs only once every family's is
	// done, after the writer hold's convert.
	Families []string
	Cutovers func(ctx context.Context) (map[string]string, error)
	// Barrier takes the hold's bucket barrier on bucket (holdrecord.Barrier with this hold's id),
	// so a record committed before it whose apply the leader still holds is read.
	Barrier func(ctx context.Context, bucket string) error
	// Consumers are the declared consumer shapes but the recorded session consumers
	// (DeclaredConsumers).
	Consumers []ConsumerShape
	// RecordedSessionConsumers reads the session consumers KV_envoy_session_consumers records, after
	// the barrier on it, skipping the bucket's system keys (a key holding a '.': its sentinel
	// envoy.bucket and each hold's hold.barrier.<id>).
	RecordedSessionConsumers func(ctx context.Context) ([]ConsumerShape, error)
}

// ErrNotConverted is a census asked for before every family's cutover row is done.
var ErrNotConverted = errors.New("the census runs only after convert")

// Census lists every stream of the account on conn and every consumer of each, and removes what the
// declared set does not name: a stream outside it, removed with STREAM.DELETE; a consumer whose
// name matches no shape, or whose configuration differs from its shape's, removed with
// CONSUMER.DELETE; and a declared stream with a source, a mirror, a republish or a subject
// transform, through which a stream reads another, updated back to the build's configuration with
// STREAM.UPDATE and never deleted. A declared stream that is sealed, or allows a per-message TTL or
// message schedules, settings nats-server cannot turn off, fails the census naming the setting, and
// it stops: the hold's window restores that stream from its backup. A listing is complete only
// when STREAM.LIST's own total equals the streams it returned, its missing list is empty and
// STREAM.NAMES agrees; otherwise the census is UNKNOWN and changes nothing. It runs as the operator
// inside a hold, only once every family is converted, and reads the recorded session consumers only
// after its barrier on their bucket.
func Census(ctx context.Context, conn *nats.Conn, in CensusInput) (Report, error) {
	if err := requirePhase(ctx, "streams census", in.Phase, natsgrant.OperatorPhaseHeld); err != nil {
		return Report{}, err
	}
	if err := requireConverted(ctx, in); err != nil {
		return Report{}, err
	}
	if in.Barrier == nil || in.RecordedSessionConsumers == nil {
		return Report{Unknown: []string{"no barrier or no reader of the recorded session consumers: their consumers cannot be told from planted ones"}}, nil
	}
	if err := in.Barrier(ctx, SessionConsumerRecordsBucket); err != nil {
		return Report{Unknown: []string{"the barrier on " + SessionConsumerRecordsBucket + ": " + err.Error()}}, nil
	}
	recorded, err := in.RecordedSessionConsumers(ctx)
	if err != nil {
		return Report{Unknown: []string{"read the recorded session consumers: " + err.Error()}}, nil
	}
	shapes := slices.Concat(in.Consumers, recorded)
	declared := streamset.Declared(in.Settings)

	streams, unknown := listStreams(ctx, conn)
	if unknown != "" {
		return Report{Unknown: []string{unknown}}, nil
	}
	var report Report
	var kept []string
	for _, info := range streams {
		name := info.Config.Name
		stream, ok := streamset.Find(declared, name)
		if name == recordsStream {
			// The records bucket is not the build's to configure (the writer hold's create-bucket step makes it),
			// so it is kept, and only read for what reads another stream.
			stream, ok = streamset.Stream{Name: name, Protected: true}, true
		}
		if !ok {
			if _, err := request(ctx, conn, "$JS.API.STREAM.DELETE."+name, nil); err != nil {
				report.Failures = append(report.Failures, Finding{Stream: name, What: "undeclared, and its delete failed: " + err.Error()})
				return report, nil
			}
			report.Failures = append(report.Failures, Finding{Stream: name, What: "undeclared"})
			report.Changes = append(report.Changes, Finding{Stream: name, What: "removed"})
			continue
		}
		kept = append(kept, name)
		if stream.Transient {
			continue
		}
		if stop := unswitchable(info); stop != "" {
			report.Failures = append(report.Failures, Finding{Stream: name, What: stop + ", which nats-server cannot turn off: the census stops; restore the stream from its backup under the hold"})
			return report, nil
		}
		reads := readsAnotherStream(info.Config)
		if len(reads) == 0 {
			continue
		}
		restored := info.Config
		restored.Sources, restored.Mirror, restored.RePublish, restored.SubjectTransform = nil, nil, nil, nil
		report.Failures = append(report.Failures, Finding{Stream: name, What: strings.Join(reads, ", ")})
		if _, err := request(ctx, conn, "$JS.API.STREAM.UPDATE."+name, restored); err != nil {
			report.Failures = append(report.Failures, Finding{Stream: name, What: "its update back to the build's configuration failed: " + err.Error()})
			return report, nil
		}
		report.Changes = append(report.Changes, Finding{Stream: name, What: "updated back to the build's configuration, without " + strings.Join(reads, ", ")})
	}
	for _, name := range kept {
		consumers, unknown := listConsumers(ctx, conn, name)
		if unknown != "" {
			report.Unknown = append(report.Unknown, unknown)
			return report, nil
		}
		for _, consumer := range consumers {
			if reason := shapeMismatch(shapes, name, consumer); reason != "" {
				if _, err := request(ctx, conn, "$JS.API.CONSUMER.DELETE."+name+"."+consumer.Name, nil); err != nil {
					report.Failures = append(report.Failures, Finding{Stream: name, Consumer: consumer.Name, What: reason + ", and its delete failed: " + err.Error()})
					return report, nil
				}
				report.Failures = append(report.Failures, Finding{Stream: name, Consumer: consumer.Name, What: reason})
				report.Changes = append(report.Changes, Finding{Stream: name, Consumer: consumer.Name, What: "removed"})
			}
		}
	}
	return report, nil
}

// requireConverted refuses a census, naming convert, until every family in.Families names has a
// cutover row in state done. A census that names no family is refused too: it would check nothing.
func requireConverted(ctx context.Context, in CensusInput) error {
	if len(in.Families) == 0 || in.Cutovers == nil {
		return fmt.Errorf("streams census: %w: no family named, so no cutover row was read", ErrNotConverted)
	}
	rows, err := in.Cutovers(ctx)
	if err != nil {
		return fmt.Errorf("streams census: read the cutover rows: %w", err)
	}
	var waiting []string
	for _, family := range in.Families {
		if state := rows[family]; state != CutoverDone {
			waiting = append(waiting, fmt.Sprintf("%s (%s)", family, cmpOr(state, "no row")))
		}
	}
	if len(waiting) > 0 {
		return fmt.Errorf("streams census: %w: run convert first; not done: %s", ErrNotConverted, strings.Join(waiting, ", "))
	}
	return nil
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// unswitchable names the first setting of info's stream nats-server cannot turn off once it is on,
// and is empty for none.
func unswitchable(info streamset.Info) string {
	switch {
	case info.Config.Sealed:
		return "sealed"
	case info.Config.AllowMsgTTL:
		return "allow_msg_ttl"
	case info.AllowMsgSchedules:
		return "allow_msg_schedules"
	}
	return ""
}

// readsAnotherStream names each setting of config through which a stream reads another or hands
// its messages elsewhere: sources, a mirror, a republish and a subject transform.
func readsAnotherStream(config nats.StreamConfig) []string {
	var reads []string
	if len(config.Sources) > 0 {
		reads = append(reads, "sources")
	}
	if config.Mirror != nil {
		reads = append(reads, "mirror")
	}
	if config.RePublish != nil {
		reads = append(reads, "republish")
	}
	if config.SubjectTransform != nil {
		reads = append(reads, "subject_transform")
	}
	return reads
}

// shapeMismatch is why consumer, of stream, matches no shape, and empty when one takes it: no shape
// of its stream names it, or the one that does has another configuration.
func shapeMismatch(shapes []ConsumerShape, stream string, consumer *nats.ConsumerInfo) string {
	mismatch := ""
	for _, shape := range shapes {
		if shape.Stream != stream {
			continue
		}
		if shape.Ordered {
			if orderedWatcher(stream, consumer.Config) {
				return ""
			}
			continue
		}
		if !namedFor(shape.Name, consumer.Name) {
			continue
		}
		reason := configMismatch(shape, consumer.Config)
		if reason == "" {
			return ""
		}
		mismatch = "named for " + shape.Name + ", but " + reason
	}
	if mismatch != "" {
		return mismatch
	}
	return "its name matches no declared consumer"
}

// namedFor reports whether name is the shape's own, or its convert form, <shape>~<nuid>.
func namedFor(shape, name string) bool {
	if name == shape {
		return true
	}
	suffix, found := strings.CutPrefix(name, shape+"~")
	return found && nuidLike(suffix)
}

// nuidLike reports whether s is a nuid: 22 base-62 characters.
func nuidLike(s string) bool {
	if len(s) != 22 {
		return false
	}
	for _, r := range s {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z') {
			return false
		}
	}
	return true
}

// configMismatch names the first setting of config that differs from shape's, and is empty when
// none does.
func configMismatch(shape ConsumerShape, config nats.ConsumerConfig) string {
	want, got := slices.Sorted(slices.Values(shape.FilterSubjects)), slices.Sorted(slices.Values(filterSubjects(config)))
	switch {
	case !slices.Equal(want, got):
		return fmt.Sprintf("its filter subjects are %v, want %v", got, want)
	case shape.Push != (config.DeliverSubject != ""):
		return fmt.Sprintf("its deliver subject is %q", config.DeliverSubject)
	case shape.Push && !replyInbox(config.DeliverSubject):
		return fmt.Sprintf("it delivers to %q, which is no reply inbox the server made", config.DeliverSubject)
	case config.DeliverGroup != shape.DeliverGroup:
		return fmt.Sprintf("its deliver group is %q, want %q", config.DeliverGroup, shape.DeliverGroup)
	case config.AckPolicy != shape.AckPolicy:
		return fmt.Sprintf("its ack policy is %s, want %s", config.AckPolicy, shape.AckPolicy)
	}
	return ""
}

// replyInbox reports whether subject is a reply inbox nats.NewInbox makes: _INBOX.<nuid>.
func replyInbox(subject string) bool {
	rest, found := strings.CutPrefix(subject, nats.InboxPrefix)
	return found && nuidLike(rest)
}

// orderedWatcher reports whether config is a KV watcher's ordered consumer on stream, a bucket's:
// no durable name, no acknowledgement, delivery to a reply inbox, every filter within the bucket's
// subjects, and an inactive threshold, past which the server removes it.
func orderedWatcher(stream string, config nats.ConsumerConfig) bool {
	bucket, isBucket := strings.CutPrefix(stream, "KV_")
	if !isBucket || config.Durable != "" || config.AckPolicy != nats.AckNonePolicy || config.InactiveThreshold <= 0 {
		return false
	}
	if !strings.HasPrefix(config.DeliverSubject, nats.InboxPrefix) {
		return false
	}
	filters := filterSubjects(config)
	return len(filters) > 0 && !slices.ContainsFunc(filters, func(filter string) bool {
		return !strings.HasPrefix(filter, "$KV."+bucket+".")
	})
}

// filterSubjects are config's filter subjects, its one FilterSubject or its FilterSubjects.
func filterSubjects(config nats.ConsumerConfig) []string {
	if config.FilterSubject != "" {
		return []string{config.FilterSubject}
	}
	return slices.Clone(config.FilterSubjects)
}

// page is the paging of a JetStream list answer.
type page struct {
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// listStreams is every stream of the account, from STREAM.LIST, page by page, and a non-empty
// unknown naming why the listing is not complete: a page that drew no answer or an error, a total
// that differs from the streams returned, a missing list, or STREAM.NAMES naming other streams.
func listStreams(ctx context.Context, conn *nats.Conn) ([]streamset.Info, string) {
	var streams []streamset.Info
	total := -1
	for {
		raw, err := request(ctx, conn, "$JS.API.STREAM.LIST", map[string]int{"offset": len(streams)})
		if err != nil {
			return nil, "list the streams: " + err.Error()
		}
		var answer struct {
			page
			Streams []json.RawMessage `json:"streams"`
			Missing []string          `json:"missing"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, "read the stream list: " + err.Error()
		}
		if len(answer.Missing) > 0 {
			return nil, "the stream list names streams it could not read: " + strings.Join(answer.Missing, ", ")
		}
		total = answer.Total
		for _, item := range answer.Streams {
			info, err := decodeListed(item)
			if err != nil {
				return nil, "read the stream list: " + err.Error()
			}
			streams = append(streams, info)
		}
		if len(answer.Streams) == 0 || len(streams) >= total {
			break
		}
	}
	if len(streams) != total {
		return nil, fmt.Sprintf("the stream list returned %d streams of the %d it counts", len(streams), total)
	}
	names, unknown := listStreamNames(ctx, conn)
	if unknown != "" {
		return nil, unknown
	}
	listed := make([]string, len(streams))
	for index, info := range streams {
		listed[index] = info.Config.Name
	}
	slices.Sort(listed)
	slices.Sort(names)
	if !slices.Equal(listed, names) {
		return nil, fmt.Sprintf("STREAM.LIST names %v and STREAM.NAMES %v", listed, names)
	}
	return streams, ""
}

// decodeListed reads one stream of a STREAM.LIST page as streamset.DecodeInfo reads a STREAM.INFO
// answer.
func decodeListed(raw json.RawMessage) (streamset.Info, error) {
	return streamset.DecodeInfo(raw)
}

// listStreamNames is every stream name STREAM.NAMES gives, page by page, and why the listing is not
// complete.
func listStreamNames(ctx context.Context, conn *nats.Conn) ([]string, string) {
	var names []string
	for {
		raw, err := request(ctx, conn, "$JS.API.STREAM.NAMES", map[string]int{"offset": len(names)})
		if err != nil {
			return nil, "list the stream names: " + err.Error()
		}
		var answer struct {
			page
			Streams []string `json:"streams"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, "read the stream names: " + err.Error()
		}
		names = append(names, answer.Streams...)
		if len(answer.Streams) == 0 || len(names) >= answer.Total {
			if len(names) != answer.Total {
				return nil, fmt.Sprintf("STREAM.NAMES returned %d names of the %d it counts", len(names), answer.Total)
			}
			return names, ""
		}
	}
}

// listConsumers is every consumer of stream, from CONSUMER.LIST, page by page, and why the listing
// is not complete.
func listConsumers(ctx context.Context, conn *nats.Conn, stream string) ([]*nats.ConsumerInfo, string) {
	var consumers []*nats.ConsumerInfo
	for {
		raw, err := request(ctx, conn, "$JS.API.CONSUMER.LIST."+stream, map[string]int{"offset": len(consumers)})
		if err != nil {
			return nil, "list " + stream + "'s consumers: " + err.Error()
		}
		var answer struct {
			page
			Consumers []*nats.ConsumerInfo `json:"consumers"`
			Missing   []string             `json:"missing"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, "read " + stream + "'s consumer list: " + err.Error()
		}
		if len(answer.Missing) > 0 {
			return nil, stream + "'s consumer list names consumers it could not read: " + strings.Join(answer.Missing, ", ")
		}
		consumers = append(consumers, answer.Consumers...)
		if len(answer.Consumers) == 0 || len(consumers) >= answer.Total {
			if len(consumers) != answer.Total {
				return nil, fmt.Sprintf("%s's consumer list returned %d of the %d it counts", stream, len(consumers), answer.Total)
			}
			return consumers, ""
		}
	}
}
