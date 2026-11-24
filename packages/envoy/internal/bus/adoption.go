package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
)

// The defaults nats-server v2.15.1 gives a consumer's create where it leaves a setting unset
// (setConsumerConfigDefaults, consumer.go :611-726) that hold for every server: a pull
// consumer's max waiting (JSWaitQueueDefaultMax), an explicit or all ack's ack wait
// (JsAckWaitDefault), and a pinned priority policy's timeout (JsDefaultPinnedTTL).
const (
	serverMaxWaiting = 512
	serverAckWait    = 30 * time.Second
	serverPinnedTTL  = 2 * time.Minute
)

// serverDefaulted is create as nats-server stores it (setConsumerConfigDefaults, consumer.go
// :611-726), in every setting whose default holds whatever the stream and the server's limits:
// max deliver 0 or below -1 is -1; max ack pending below -1 is -1; a negative max waiting, max
// batch, max expires, max bytes, heartbeat, inactive threshold or priority timeout is 0; a pull
// consumer with no max waiting has 512; an explicit or all ack with no ack wait has 30 s; a
// backoff's first step is the ack wait; and a pinned priority policy with no timeout has 2 m. Not
// here, since the stream's consumer limits or the server's and the account's limits decide them:
// max ack pending 0, inactive threshold 0, and a pull consumer's max batch 0 (consumer.go
// :684-713). Every family's create sets the first two, and the deployment sets no max batch limit
// (consumernames README, stated limits).
func serverDefaulted(create jetstream.ConsumerConfig) jetstream.ConsumerConfig {
	if create.MaxDeliver == 0 || create.MaxDeliver < -1 {
		create.MaxDeliver = -1
	}
	if create.MaxAckPending < -1 {
		create.MaxAckPending = -1
	}
	create.MaxWaiting = max(create.MaxWaiting, 0)
	create.MaxRequestBatch = max(create.MaxRequestBatch, 0)
	create.MaxRequestExpires = max(create.MaxRequestExpires, 0)
	create.MaxRequestMaxBytes = max(create.MaxRequestMaxBytes, 0)
	create.IdleHeartbeat = max(create.IdleHeartbeat, 0)
	create.InactiveThreshold = max(create.InactiveThreshold, 0)
	create.PinnedTTL = max(create.PinnedTTL, 0)
	if create.DeliverSubject == "" && create.MaxWaiting == 0 {
		create.MaxWaiting = serverMaxWaiting
	}
	if create.AckWait == 0 && (create.AckPolicy == jetstream.AckExplicitPolicy || create.AckPolicy == jetstream.AckAllPolicy) {
		create.AckWait = serverAckWait
	}
	if len(create.BackOff) > 0 {
		create.AckWait = create.BackOff[0]
	}
	if create.PriorityPolicy == jetstream.PriorityPolicyPinned && create.PinnedTTL == 0 {
		create.PinnedTTL = serverPinnedTTL
	}
	return create
}

// serverMetadataPrefix begins every metadata key nats-server sets on a consumer itself
// (JSRequiredLevelMetadataKey, JSServerVersionMetadataKey, JSServerLevelMetadataKey): it sets them
// at each create and update, and answers them in the consumer's info.
const serverMetadataPrefix = "_nats."

// consumerSetting is what an adoption does with one setting of a consumer's configuration.
type consumerSetting int

const (
	// settingName is the consumer's own name, which the family's record names.
	settingName consumerSetting = iota
	// settingStart is where the consumer starts, which consumernames.Names checks against the start
	// its operation reserved.
	settingStart
	// settingFixed is a setting no update changes (nats-server v2.15.1 consumer.go
	// checkNewConsumerConfig, :2537-2604; and pause_until, which the server sets back to the
	// consumer's own on every update, jetstream_api.go :5261-5264, jetstream_cluster.go
	// :12678-12680, so only a CONSUMER.PAUSE request, which no listener grant holds, changes it),
	// or one an update writes to the server's record of the consumer and not to its delivery: a
	// push consumer's deliver group, whose interest the server registered at the create
	// (consumer.go :1865, :2780-2783). A candidate must hold the family's create's value, or it is
	// refused.
	settingFixed
	// settingCorrected is a setting an update changes: the correction after an adoption writes the
	// family's create's value (CorrectedConfig), so no candidate keeps another.
	settingCorrected
)

// consumerSettings is every setting of a consumer's configuration as nats-server v2.15.1 answers
// its info (server/consumer.go ConsumerConfig), by what an adoption does with it. It is the
// adoption's allowlist: a setting the server answers that it does not name is refused, so a server
// that adds one never has a candidate adopted on a setting this build cannot see.
var consumerSettings = map[string]consumerSetting{
	"durable_name": settingName,
	"name":         settingName,

	"deliver_policy": settingStart,
	"opt_start_seq":  settingStart,

	"opt_start_time":  settingFixed,
	"ack_policy":      settingFixed,
	"replay_policy":   settingFixed,
	"mem_storage":     settingFixed,
	"direct":          settingFixed,
	"sourcing":        settingFixed,
	"flow_control":    settingFixed,
	"idle_heartbeat":  settingFixed,
	"deliver_subject": settingFixed,
	"deliver_group":   settingFixed,
	"max_waiting":     settingFixed,
	"pause_until":     settingFixed,

	"description":        settingCorrected,
	"ack_wait":           settingCorrected,
	"max_deliver":        settingCorrected,
	"backoff":            settingCorrected,
	"filter_subject":     settingCorrected,
	"filter_subjects":    settingCorrected,
	"rate_limit_bps":     settingCorrected,
	"sample_freq":        settingCorrected,
	"max_ack_pending":    settingCorrected,
	"headers_only":       settingCorrected,
	"max_batch":          settingCorrected,
	"max_expires":        settingCorrected,
	"max_bytes":          settingCorrected,
	"inactive_threshold": settingCorrected,
	"num_replicas":       settingCorrected,
	"metadata":           settingCorrected,
	"priority_groups":    settingCorrected,
	"priority_policy":    settingCorrected,
	"priority_timeout":   settingCorrected,
}

// replyInbox reports whether subject is a reply inbox as a connection whose inboxes begin inbox
// names one (nats.Conn.NewInbox): inbox, then one token.
func replyInbox(subject, inbox string) bool {
	token, ok := strings.CutPrefix(subject, inbox)
	return ok && token != "" && !strings.ContainsAny(token, ".*> ")
}

// replyInboxPrefix is what every inbox the client's connections name begins with: its inbox prefix
// (WithInboxPrefix, nats.CustomInboxPrefix) and a dot, or nats.go's _INBOX. for a client with none.
// The listener's durable delivers to an inbox its own connection names (cmd/listener's
// listenerDurableConfig), so an edge listener's is under _INBOX_envoy_edge_<machine id>.
func (c *Client) replyInboxPrefix() string {
	if prefix := c.Connection().Opts.InboxPrefix; prefix != "" {
		return prefix + "."
	}
	return nats.InboxPrefix
}

// ConsumerAdoptable reads consumer name of the stream on c, as every consumer API request of this
// package is sent (consumerRequest: gated, and a grant's refusal is ErrConsumerNotGranted), and
// compares its configuration, as the server answers it, with create, the configuration its
// family's create sends: a candidate that differs in a setting no update changes, or that holds a
// setting this build does not know (consumerSettings), is refused as
// consumernames.ErrUnexpectedConsumer, naming it and each such setting (adoptionRefusals). A push
// candidate must deliver to a reply inbox of c's own connections (replyInboxPrefix), as the
// family's create on c names it. A read that fails is its own error, never a refusal. A family's
// consumernames.Config.Adopt asks here; every setting an update changes is the correction's to
// write after it (CorrectedConfig).
func (c *Client) ConsumerAdoptable(ctx context.Context, name string, create jetstream.ConsumerConfig) error {
	var response struct {
		Config map[string]json.RawMessage `json:"config"`
		Error  *jetstream.APIError        `json:"error"`
	}
	if err := c.consumerRequest(ctx, consumerAPI("INFO", name), nil, &response); err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("bus: read consumer %s: %w", name, response.Error)
	}
	why, err := adoptionRefusals(response.Config, create, c.replyInboxPrefix())
	if err != nil {
		return fmt.Errorf("bus: compare consumer %s with its family's create: %w", name, err)
	}
	if len(why) > 0 {
		return fmt.Errorf("%w: %s has %s, which its family's create never gives and no update changes", consumernames.ErrUnexpectedConsumer, name, strings.Join(why, ", "))
	}
	return nil
}

// adoptionRefusals names each setting of held, a consumer's configuration as the server answers
// it, that refuses its adoption against create, the configuration its family's create sends, as
// the server stores it (serverDefaulted): a setting the server answers that consumerSettings does
// not name, and a setting no update changes (settingFixed) whose value is not create's. A setting
// absent from one side reads as its zero value, as the server and nats.go leave a zero value out.
// A deliver subject is compared by its kind, since the create gives a fresh reply inbox each time:
// none for a pull consumer, and for a push one a reply inbox under inbox, the prefix every inbox of
// the adopting connection begins with (replyInbox): one under another client's prefix, nats.go's
// default or another edge machine's, delivers where the adopting client may not subscribe.
func adoptionRefusals(held map[string]json.RawMessage, create jetstream.ConsumerConfig, inbox string) ([]string, error) {
	create = serverDefaulted(create)
	data, err := json.Marshal(create)
	if err != nil {
		return nil, err
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(data, &want); err != nil {
		return nil, err
	}
	keys := slices.Collect(maps.Keys(held))
	for key := range want {
		if _, ok := held[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var why []string
	for _, key := range keys {
		setting, known := consumerSettings[key]
		switch {
		case !known:
			why = append(why, fmt.Sprintf("the setting %s %s, which this build does not know", key, shown(held[key])))
		case setting != settingFixed:
		case key == "deliver_subject":
			var subject string
			if !zero(held[key]) {
				if err := json.Unmarshal(held[key], &subject); err != nil {
					return nil, fmt.Errorf("its deliver_subject %s: %w", held[key], err)
				}
			}
			switch push := !zero(want[key]); {
			case push && !replyInbox(subject, inbox):
				why = append(why, fmt.Sprintf("deliver_subject %s, not a reply inbox under %s", shown(held[key]), inbox))
			case !push && subject != "":
				why = append(why, fmt.Sprintf("deliver_subject %s, as a push consumer, not none", shown(held[key])))
			}
		case !sameSetting(held[key], want[key]):
			why = append(why, fmt.Sprintf("%s %s, not %s", key, shown(held[key]), shown(want[key])))
		}
	}
	return why, nil
}

// sameSetting reports whether a and b, a setting's JSON on each side, are the same value, a setting
// absent from a side being its zero value.
func sameSetting(a, b json.RawMessage) bool {
	if zero(a) || zero(b) {
		return zero(a) && zero(b)
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// zero reports whether raw, a setting's JSON, is absent or a zero value: null, false, 0, "", an
// empty list or an empty object.
func zero(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case float64:
		return v == 0
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// shown is raw, a setting's JSON, as a refusal names it: "none" for a zero value.
func shown(raw json.RawMessage) string {
	if zero(raw) {
		return "none"
	}
	return string(raw)
}

// CorrectedConfig is the update that brings current, a consumer's configuration as the server
// answers it, to create, the configuration its family's create sends, as the server stores it
// (serverDefaulted), in every setting an update changes (settingCorrected), and whether it changes
// anything: create's whole configuration, with current's name, start and every setting no update
// changes (which the server would refuse to change, and which the adoption check found as create
// gives them), and with the metadata the server sets itself (serverMetadataPrefix) kept. So a
// candidate's max batch, headers only, filters and every other setting an update changes go back to
// its family's value, a setting the create leaves to the server's default reads as the server
// stores it, and a consumer already as its family creates it is unchanged.
func CorrectedConfig(current, create jetstream.ConsumerConfig) (jetstream.ConsumerConfig, bool) {
	next := serverDefaulted(create)
	next.Name, next.Durable = current.Name, current.Durable
	next.DeliverPolicy, next.OptStartSeq = current.DeliverPolicy, current.OptStartSeq
	next.OptStartTime, next.AckPolicy, next.ReplayPolicy = current.OptStartTime, current.AckPolicy, current.ReplayPolicy
	next.MemoryStorage, next.FlowControl, next.IdleHeartbeat = current.MemoryStorage, current.FlowControl, current.IdleHeartbeat
	next.DeliverSubject, next.DeliverGroup, next.MaxWaiting = current.DeliverSubject, current.DeliverGroup, current.MaxWaiting
	next.PauseUntil = current.PauseUntil
	next.Metadata = maps.Clone(create.Metadata)
	for key, value := range current.Metadata {
		if strings.HasPrefix(key, serverMetadataPrefix) {
			if next.Metadata == nil {
				next.Metadata = map[string]string{}
			}
			next.Metadata[key] = value
		}
	}
	return next, !reflect.DeepEqual(next, current)
}

// ConsumerInfo reads consumer name of the stream on c, as every consumer API request of this
// package is sent (consumerRequest), with every setting nats.go's jetstream package reads (push
// settings and the pause among them). The server's refusal, no such consumer among them, is its
// error, naming the consumer. A family that corrects a consumer it adopted reads it here
// (CorrectedConfig, UpdateConsumer).
func (c *Client) ConsumerInfo(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	info, err := c.consumerInfo(ctx, name)
	var refused *jetstream.APIError
	if errors.As(err, &refused) {
		return nil, fmt.Errorf("bus: read consumer %s: %w", name, refused)
	}
	return info, err
}

// UpdateConsumer sends config as the update of its consumer (config.Durable) of the stream on c, as
// every consumer API request of this package is sent (consumerRequest). The server's refusal is
// its error, naming the consumer.
func (c *Client) UpdateConsumer(ctx context.Context, config jetstream.ConsumerConfig) error {
	_, err := c.upsertConsumer(ctx, config.Durable, config, "update")
	var refused *jetstream.APIError
	if errors.As(err, &refused) {
		return fmt.Errorf("bus: update consumer %s: %w", config.Durable, refused)
	}
	return err
}
