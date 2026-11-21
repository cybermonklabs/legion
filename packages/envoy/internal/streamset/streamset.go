// Package streamset is the set of JetStream streams Envoy declares, each with the build's fixed
// configuration: the names come from the code's own name functions (bus.Stream,
// bus.ListenerBuckets, bus.WebhookRedeliveryBucket, natsgrant.HoldBarrierStream) and are never
// written as a list, and each configuration is the one the program that owns the stream makes it
// with outside the auth service. Beside the auth service no program but the operator makes or
// changes a stream (natsgrant.ForCallout, bus.StreamsOperatorMadeEnvVar): the operator makes and
// raises every stream from this set at deploy time, and reads each against it
// (packages/envoy/infra/streams, envoy-infra streams).
package streamset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/natsgrant"
)

// Settings are the deployment's settings a declared stream's configuration takes, which every
// program of one deployment reads alike.
type Settings struct {
	// Replicas is ENVOY_NATS_REPLICAS, the copies of every stream; 0 is 1.
	Replicas int
	// StreamMaxBytes is ENVOY_STREAM_MAX_BYTES, the notification stream's byte limit; 0 sets none.
	StreamMaxBytes int64
	// SessionMaxAge is ENVOY_SESSION_MAX_AGE, the session bucket's age; 0 is the bucket's own
	// default, session.BucketMaxAge, the age the listener creates it with.
	SessionMaxAge time.Duration
}

func (s Settings) replicas() int { return max(s.Replicas, 1) }

// MeasureStream is ENVOY_MEASURE, a stream on subjects of its own, envoy-measure.>, which no
// grant publishes on. The operator makes and raises it inside a hold (envoy-infra streams update,
// Update), and no other client makes, changes or removes it: its configuration would be the
// caller's choosing. It is the one stream that is not protected (Protected: the writer hold does
// not hold it), and its copies are held to the deployment's count like every other stream's
// (Drift).
const MeasureStream = "ENVOY_MEASURE"

// measureMaxBytes is MeasureStream's byte limit: it stores nothing, and an account may require one.
const measureMaxBytes = 1 << 20

// Stream is one declared stream.
type Stream struct {
	// Name is the stream's name: ENVOY_NOTIFICATIONS, KV_<bucket>, ENVOY_MEASURE or the hold's
	// barrier.
	Name string
	// Protected is every stream but MeasureStream: only the operator, under a hold, makes or
	// changes one.
	Protected bool
	// Transient is the writer hold's barrier (natsgrant.HoldBarrierStream), which the hold makes and
	// removes: the census keeps it while it stands, and nothing else makes it or misses it.
	Transient bool
	// Bucket is a KV bucket's configuration, as its program opens it (bus.EnsureKeyValue); nil for
	// a stream that is no bucket.
	Bucket *nats.KeyValueConfig
	// Config is the stream's configuration: a bucket's is the stream nats.go makes for Bucket
	// (BucketStreamConfig).
	Config nats.StreamConfig
}

// Declared is the declared set under s, in a fixed order: ENVOY_NOTIFICATIONS, each bucket of
// bus.ListenerBuckets in its order, the webhook redelivery bucket, MeasureStream and the hold's
// barrier. No private-projects stream is declared: the privacy answer is Dispatch's, read through
// the broker, so a KV_envoy_private_projects is undeclared like any other stream.
func Declared(s Settings) []Stream {
	notifications := bus.NotificationStreamConfig(s.replicas(), s.StreamMaxBytes)
	streams := []Stream{{Name: bus.Stream, Protected: true, Config: notifications}}
	for _, name := range slices.Concat(bus.ListenerBuckets(), []string{bus.WebhookRedeliveryBucket}) {
		bucket := BucketConfig(name, s)
		streams = append(streams, Stream{Name: "KV_" + name, Protected: true, Bucket: bucket, Config: BucketStreamConfig(bucket)})
	}
	return append(streams,
		Stream{Name: MeasureStream, Config: nats.StreamConfig{
			Name: MeasureStream, Subjects: []string{"envoy-measure.>"}, Retention: nats.LimitsPolicy,
			MaxBytes: measureMaxBytes, Storage: nats.FileStorage, Replicas: s.replicas(),
		}},
		Stream{Name: natsgrant.HoldBarrierStream, Protected: true, Transient: true},
	)
}

// Find is the declared stream named name in streams, and false for an undeclared one.
func Find(streams []Stream, name string) (Stream, bool) {
	for _, stream := range streams {
		if stream.Name == name {
			return stream, true
		}
	}
	return Stream{}, false
}

// BucketConfig is the configuration the program that owns bucket opens it with under s, the one
// the operator makes it with: the interest and role-claim buckets (internal/store), the session
// registry at the deployment's age and the known-session store (internal/session), the Legion
// agent registry with one entry of history (internal/legionagents), the CI state
// (internal/cistore), the spent proofs (cmd/listener/brokerauth.go) and Dispatch's webhook
// redelivery state (internal/dispatch/redeliver, at bus.WebhookRedeliveryRetention). Every one is
// on file at the deployment's copies. A bucket it does not know is nil.
func BucketConfig(bucket string, s Settings) *nats.KeyValueConfig {
	cfg := &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: s.replicas()}
	switch bucket {
	case bus.InterestBucket, bus.RoleClaimBucket:
	case bus.SessionBucket:
		cfg.TTL = session.BucketMaxAge
		if s.SessionMaxAge > 0 {
			cfg.TTL = s.SessionMaxAge
		}
	case bus.KnownSessionBucket:
		cfg.TTL = session.KnownSessionRetention
	case bus.LegionAgentBucket:
		cfg.History = 1
	case bus.CIStateBucket:
		cfg.TTL = cistore.Retention
	case bus.DPoPProofBucket:
		cfg.TTL = ciphertoken.SpentProofRetention
	case bus.WebhookRedeliveryBucket:
		cfg.TTL = bus.WebhookRedeliveryRetention
	default:
		return nil
	}
	return cfg
}

// BucketStreamConfig is the stream nats.go v1.55.0 makes for kv, a bucket that mirrors and
// sources nothing, on nats-server 2.7.2 or later (kv.go, CreateKeyValue): one subject, $KV.<bucket>.>,
// its history as the per-subject limit, its TTL as the age, roll-ups and direct gets allowed,
// deletes refused, and discard new.
func BucketStreamConfig(kv *nats.KeyValueConfig) nats.StreamConfig {
	history := int64(1)
	if kv.History > 0 {
		history = int64(kv.History)
	}
	maxBytes := kv.MaxBytes
	if maxBytes == 0 {
		maxBytes = -1
	}
	maxMsgSize := kv.MaxValueSize
	if maxMsgSize == 0 {
		maxMsgSize = -1
	}
	duplicates := 2 * time.Minute
	if kv.TTL > 0 && kv.TTL < duplicates {
		duplicates = kv.TTL
	}
	return nats.StreamConfig{
		Name:              "KV_" + kv.Bucket,
		Subjects:          []string{"$KV." + kv.Bucket + ".>"},
		MaxMsgsPerSubject: history,
		MaxBytes:          maxBytes,
		MaxAge:            kv.TTL,
		MaxMsgSize:        maxMsgSize,
		Storage:           kv.Storage,
		Replicas:          max(kv.Replicas, 1),
		AllowRollup:       true,
		DenyDelete:        true,
		Duplicates:        duplicates,
		MaxMsgs:           -1,
		MaxConsumers:      -1,
		AllowDirect:       true,
		Discard:           nats.DiscardNew,
	}
}

// Info is a stream's info as STREAM.INFO answers it, read whole: nats.go's StreamInfo, and the
// settings of a nats-server newer than nats.go v1.55.0 decodes, which a census must still see.
type Info struct {
	nats.StreamInfo
	// AllowMsgSchedules is config.allow_msg_schedules: messages the server publishes later, on a
	// subject the message names.
	AllowMsgSchedules bool
}

// ErrStreamNotFound is a STREAM.INFO answer naming no stream (JetStream's err_code 10059).
var ErrStreamNotFound = errors.New("stream not found")

// jsStreamNotFound is JetStream's error code for a stream that does not exist.
const jsStreamNotFound = 10059

// DecodeInfo reads raw, the body of a $JS.API.STREAM.INFO.<name> answer: the stream's info, or the
// error the answer carries, ErrStreamNotFound for a stream that does not exist.
func DecodeInfo(raw []byte) (Info, error) {
	var answer struct {
		Error *struct {
			Code        int    `json:"code"`
			ErrCode     int    `json:"err_code"`
			Description string `json:"description"`
		} `json:"error"`
		Config struct {
			AllowMsgSchedules bool `json:"allow_msg_schedules"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return Info{}, fmt.Errorf("read a stream's info: %w", err)
	}
	if answer.Error != nil {
		if answer.Error.ErrCode == jsStreamNotFound {
			return Info{}, ErrStreamNotFound
		}
		return Info{}, fmt.Errorf("stream info refused: %s (%d, err_code %d)", answer.Error.Description, answer.Error.Code, answer.Error.ErrCode)
	}
	var info Info
	if err := json.Unmarshal(raw, &info.StreamInfo); err != nil {
		return Info{}, fmt.Errorf("read a stream's info: %w", err)
	}
	info.AllowMsgSchedules = answer.Config.AllowMsgSchedules
	return info, nil
}

// Difference is one setting in which a deployed stream differs from the build's configuration.
type Difference struct {
	// Setting is the setting's name in nats-server's stream configuration: num_replicas,
	// subjects, sources, republish and the rest.
	Setting string
	// Want and Got are the build's value and the deployed one, as a person reads them.
	Want, Got string
	// Raise is a difference the operator's update makes good by today's reconcile rules
	// (internal/bus/stream.go): copies below the deployment's count, a byte limit below its own,
	// a subject the notification stream lacks, the session bucket's age, and the settings a start
	// puts back on the notification stream (its age, duplicate window, discard, consumer cap and
	// id). Every other difference stops the update, naming the stream.
	Raise bool
}

func (d Difference) String() string {
	return fmt.Sprintf("%s is %s, want %s", d.Setting, d.Got, d.Want)
}

// Drift is every setting in which info, the deployed stream's info, differs from s, its
// declared stream: the copies against the deployment's count (s.Config.Replicas), for every
// stream; any source, mirror, republish or subject transform, through which a stream reads another;
// a sealed stream, and a per-message TTL or message schedule, which nats-server cannot turn off; the
// storage and retention; and the settings the build fixes for its kind. A deployed setting above the
// build's where the build only raises (copies, the notification stream's subjects, byte limit and
// consumer cap) is no difference. A transient stream has no configuration to drift from.
func Drift(s Stream, info Info) []Difference {
	if s.Transient {
		return nil
	}
	got, want := info.Config, s.Config
	var drift []Difference
	add := func(setting string, raise bool, wantValue, gotValue any) {
		drift = append(drift, Difference{Setting: setting, Want: fmt.Sprint(wantValue), Got: fmt.Sprint(gotValue), Raise: raise})
	}
	if got.Replicas < want.Replicas {
		add("num_replicas", true, want.Replicas, got.Replicas)
	}
	if len(got.Sources) > 0 {
		add("sources", false, "none", sourceNames(got.Sources))
	}
	if got.Mirror != nil {
		add("mirror", false, "none", got.Mirror.Name)
	}
	if got.RePublish != nil {
		add("republish", false, "none", got.RePublish.Source+" -> "+got.RePublish.Destination)
	}
	if got.SubjectTransform != nil {
		add("subject_transform", false, "none", got.SubjectTransform.Source+" -> "+got.SubjectTransform.Destination)
	}
	if got.Sealed {
		add("sealed", false, false, true)
	}
	if got.AllowMsgTTL {
		add("allow_msg_ttl", false, false, true)
	}
	if info.AllowMsgSchedules {
		add("allow_msg_schedules", false, false, true)
	}
	if got.Storage != want.Storage {
		add("storage", false, want.Storage, got.Storage)
	}
	if got.Retention != want.Retention {
		add("retention", false, want.Retention, got.Retention)
	}
	switch {
	case s.Name == bus.Stream:
		drift = append(drift, notificationDrift(want, got)...)
	case s.Bucket != nil:
		drift = append(drift, bucketDrift(s, want, got)...)
	default:
		if !slices.Equal(got.Subjects, want.Subjects) {
			add("subjects", false, want.Subjects, got.Subjects)
		}
		if got.MaxBytes != want.MaxBytes {
			add("max_bytes", false, want.MaxBytes, got.MaxBytes)
		}
	}
	return drift
}

// notificationDrift is what a start reconciles on the notification stream (ensureStreamWithConfig),
// each a raise: a subject the build carries that the stream lacks, its age, duplicate window and
// discard, a byte limit below the deployment's, a consumer cap below the build's, and a missing
// stream id.
func notificationDrift(want, got nats.StreamConfig) []Difference {
	var drift []Difference
	raise := func(setting string, wantValue, gotValue any) {
		drift = append(drift, Difference{Setting: setting, Want: fmt.Sprint(wantValue), Got: fmt.Sprint(gotValue), Raise: true})
	}
	if missing := slices.DeleteFunc(slices.Clone(want.Subjects), func(subject string) bool { return slices.Contains(got.Subjects, subject) }); len(missing) > 0 {
		raise("subjects", "also "+strings.Join(missing, ", "), got.Subjects)
	}
	if got.MaxAge != want.MaxAge {
		raise("max_age", want.MaxAge, got.MaxAge)
	}
	if got.Duplicates != want.Duplicates {
		raise("duplicate_window", want.Duplicates, got.Duplicates)
	}
	if got.Discard != want.Discard {
		raise("discard", want.Discard, got.Discard)
	}
	if want.MaxBytes > 0 && (got.MaxBytes <= 0 || got.MaxBytes < want.MaxBytes) {
		raise("max_bytes", want.MaxBytes, got.MaxBytes)
	}
	if want.MaxConsumers > 0 && got.MaxConsumers > 0 && got.MaxConsumers < want.MaxConsumers {
		raise("max_consumers", want.MaxConsumers, got.MaxConsumers)
	}
	if got.Metadata[bus.StreamIDMetadataKey] == "" {
		raise("metadata."+bus.StreamIDMetadataKey, "an id", "none")
	}
	return drift
}

// bucketDrift is how a bucket's stream differs from the one nats.go makes for its declared
// configuration (BucketStreamConfig): its one subject, history, byte, size and message limits,
// roll-ups, deletes, direct gets and discard, and its age, which the operator raises on the
// session bucket alone, the one bucket whose age is a deployment setting (bus.SessionMaxAgeEnvVar).
func bucketDrift(s Stream, want, got nats.StreamConfig) []Difference {
	var drift []Difference
	add := func(setting string, raise bool, wantValue, gotValue any) {
		drift = append(drift, Difference{Setting: setting, Want: fmt.Sprint(wantValue), Got: fmt.Sprint(gotValue), Raise: raise})
	}
	if !slices.Equal(got.Subjects, want.Subjects) {
		add("subjects", false, want.Subjects, got.Subjects)
	}
	if got.MaxAge != want.MaxAge {
		add("max_age", s.Bucket.Bucket == bus.SessionBucket, want.MaxAge, got.MaxAge)
	}
	for _, field := range []struct {
		setting   string
		want, got any
	}{
		{"max_msgs_per_subject", want.MaxMsgsPerSubject, got.MaxMsgsPerSubject},
		{"max_bytes", want.MaxBytes, got.MaxBytes},
		{"max_msg_size", want.MaxMsgSize, got.MaxMsgSize},
		{"max_msgs", want.MaxMsgs, got.MaxMsgs},
		{"allow_rollup_hdrs", want.AllowRollup, got.AllowRollup},
		{"deny_delete", want.DenyDelete, got.DenyDelete},
		{"deny_purge", want.DenyPurge, got.DenyPurge},
		{"allow_direct", want.AllowDirect, got.AllowDirect},
		{"mirror_direct", want.MirrorDirect, got.MirrorDirect},
		{"discard", want.Discard, got.Discard},
	} {
		if field.want != field.got {
			add(field.setting, false, field.want, field.got)
		}
	}
	return drift
}

func sourceNames(sources []*nats.StreamSource) []string {
	names := make([]string, len(sources))
	for index, source := range sources {
		names[index] = source.Name
	}
	return names
}

// Ensure makes stream on conn, through js, with the build's configuration when the server has
// none, or raises the one it has by a start's own rules: the notification stream by a start's
// create and reconcile (bus.EnsureNotificationStream: subjects only added, limits only raised, its
// id kept), a bucket by its program's own open (bus.EnsureKeyValue, which makes it or raises its
// copies) and, for the session bucket, its age (bus.SetKeyValueMaxAge), and MeasureStream by a
// create when it is missing and a raise of its copies when it has fewer. conn must be a connection no client beside the auth service owns
// (bus.StreamsOperatorMadeEnvVar), whose grant holds the creates and updates: the operator's inside
// a hold (envoy-infra streams update), or a test's or a dev stack's own admin user. A transient
// stream is the hold's own to make, and is refused.
func Ensure(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, stream Stream) error {
	switch {
	case stream.Transient:
		return fmt.Errorf("%s is the writer hold's to make", stream.Name)
	case stream.Name == bus.Stream:
		_, err := bus.EnsureNotificationStream(ctx, conn, js, stream.Config)
		return err
	case stream.Bucket == nil:
		info, err := js.StreamInfo(stream.Name, nats.Context(ctx))
		if errors.Is(err, nats.ErrStreamNotFound) {
			config := stream.Config
			_, err = js.AddStream(&config, nats.Context(ctx))
			return err
		}
		if err != nil || info.Config.Replicas >= stream.Config.Replicas {
			return err
		}
		raised := info.Config
		raised.Replicas = stream.Config.Replicas
		_, err = js.UpdateStream(&raised, nats.Context(ctx))
		return err
	}
	if _, err := bus.EnsureKeyValue(ctx, conn, js, stream.Bucket); err != nil {
		return err
	}
	if stream.Bucket.Bucket == bus.SessionBucket {
		if _, err := bus.SetKeyValueMaxAge(ctx, conn, js, bus.SessionBucket, stream.Bucket.TTL); err != nil {
			return err
		}
	}
	return nil
}

// EnsureStreams makes or raises every protected stream of the declared set under settings on conn
// (Ensure), in Declared's order, and stops at the first that fails, naming it. It is how a test's
// fixture and a dev stack make their streams as their own admin user before any program connects
// beside the auth service, which makes none (bus.StreamsOperatorMadeEnvVar); in a deployment the
// operator's update makes them (envoy-infra streams update).
func EnsureStreams(ctx context.Context, conn *nats.Conn, settings Settings) error {
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return err
	}
	for _, stream := range Declared(settings) {
		if !stream.Protected || stream.Transient {
			continue
		}
		if err := Ensure(ctx, conn, js, stream); err != nil {
			return fmt.Errorf("make %s: %w", stream.Name, err)
		}
	}
	return nil
}
