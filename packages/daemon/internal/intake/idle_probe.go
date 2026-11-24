package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// idleProbeBound bounds one idle probe's create (idleProbe.pending), from its first request to
	// its answer, a retry of a name still taken included. NATS refuses a request the daemon's user
	// has no grant for by not answering it, and a clustered server answers a direct consumer's
	// create only from the stream's leader, so a refused create, one on a server too loaded to
	// answer, and one sent while the stream has no leader all end here, read as not idle.
	idleProbeBound = 2 * time.Second
	// idleProbeInactive is the probe's inactive threshold: with no interest on its delivery
	// subject the stream leader deletes it this long after it made it (nats-server's
	// deleteNotActive, which a direct consumer runs locally). The count a reading takes is the
	// create's own answer, so the probe has no use once that answer is sent.
	idleProbeInactive = 100 * time.Millisecond
	// idleProbeRetry is how long a create whose name an earlier probe still holds waits before it
	// tries again: readings come back to back (a boot's reconcile, a recovery's, the hold poller's
	// tick), and the earlier reading's probe is gone once its inactive threshold has passed. The
	// tries end at idleProbeBound, so a probe of the name that stands on — another daemon's, or
	// one made with a longer threshold — reads as not idle.
	idleProbeRetry = 3 * idleProbeInactive
)

// dispatchProbeName is the one name a project's idle probe is created under,
// legion-go-<project>-dispatch-probe: the grant that holds a daemon to its projects names it with
// the project's Dispatch filter in one create subject, and names no other call on it
// (natsgrant.WithinProjects), so the daemon may create it and do nothing else with it.
func dispatchProbeName(project string) string {
	return "legion-go-" + project + "-dispatch-probe"
}

// probeConn is what the idle probe sends its create on and names its delivery subject from: the
// daemon's NATS connection, whose inbox prefix is the one its grant subscribes. A test stands in a
// connection that refuses, never answers or answers another probe's info.
type probeConn interface {
	RequestMsgWithContext(ctx context.Context, msg *nats.Msg) (*nats.Msg, error)
	NewInbox() string
}

// idleProbe confirms the Dispatch consumer idle (Consumers.DispatchPosition) without a read of
// stream state or of a stored message: it creates a consumer of its own on the Dispatch consumer's
// filter, starting at the sequence after the durable's delivered one, and reads how many messages
// that consumer counts as pending when it is made.
//
// The consumer is a direct push consumer (nats-server's ConsumerConfig.Direct, which nats.go's
// jetstream.ConsumerConfig does not carry, so the create is a request of its own): the server
// takes a direct create only at the stream's leader, where it counts the new consumer's pending
// messages from the leader's store (consumer.setLeader's streamNumPending) and captures that count
// before the consumer starts to deliver (infoWithSnap), which is what the create answers
// (initialInfo). The leader has stored every message whose publish it acknowledged, so a count of
// 0 there is the leader's own word that nothing on the filter waits after the delivered sequence,
// whether or not the durable has counted it yet; a replica, which a direct get or an ordinary
// ephemeral's create may be answered from, can be behind. It is ephemeral, holds no store, takes
// no acknowledgement and has no interest on its delivery subject, so it delivers nothing, and the
// leader deletes it after idleProbeInactive. It never touches the durable: the durable's delivered
// sequence, ack floor and delivery counts are what they were.
//
// Each create is create-only and carries a description and a delivery subject of its own, so an
// answer that does not name both is not this create's (an earlier probe's info, which a second
// create of an identical configuration would answer), and is read as not idle, never as a snapshot
// of this read. A probe of the same name still standing with another configuration is refused by
// the server (consumer already exists), and the create is tried again (idleProbeRetry). Probes of
// one daemon run one at a time (mu).
type idleProbe struct {
	mu     sync.Mutex
	conn   probeConn
	name   string
	filter string
}

func newIdleProbe(conn probeConn, project string) *idleProbe {
	return &idleProbe{conn: conn, name: dispatchProbeName(project), filter: dispatchIssueFilter(project)}
}

// probeConfig is the probe consumer's configuration as nats-server reads it (server/consumer.go,
// ConsumerConfig): by start sequence from after+1, no acknowledgement, the one filter, a push
// delivery subject nobody subscribes, headers only, one replica in memory, and direct.
type probeConfig struct {
	Name              string        `json:"name"`
	Description       string        `json:"description"`
	DeliverPolicy     string        `json:"deliver_policy"`
	OptStartSeq       uint64        `json:"opt_start_seq"`
	AckPolicy         string        `json:"ack_policy"`
	FilterSubject     string        `json:"filter_subject"`
	ReplayPolicy      string        `json:"replay_policy"`
	DeliverSubject    string        `json:"deliver_subject"`
	HeadersOnly       bool          `json:"headers_only"`
	InactiveThreshold time.Duration `json:"inactive_threshold"`
	Replicas          int           `json:"num_replicas"`
	MemoryStorage     bool          `json:"mem_storage"`
	Direct            bool          `json:"direct"`
}

// probeRequest is the create's body (server/jetstream_api.go, CreateConsumerRequest): the stream,
// the configuration and the create-only action.
type probeRequest struct {
	Stream string      `json:"stream_name"`
	Config probeConfig `json:"config"`
	Action string      `json:"action"`
}

// probeAnswer is the create's answer: an API error, or the consumer's info as the leader captured
// it when it made the consumer.
type probeAnswer struct {
	Error *jetstream.APIError `json:"error"`
	jetstream.ConsumerInfo
}

// errProbeAnswer is an answer that is not the create this probe sent: another probe's info.
var errProbeAnswer = errors.New("the probe's answer is not the consumer this create made")

// probeSubject is the create's API subject for the probe: the stream, the probe's name and its
// filter, which nats-server holds the body's filter to and a grant names.
func (p *idleProbe) probeSubject() string {
	return "$JS.API.CONSUMER.CREATE." + notificationStream + "." + p.name + "." + p.filter
}

// pending is the number of messages the stream leader counts on the probe's filter after sequence
// after, read from a probe created for this call alone, within idleProbeBound. A failed, refused,
// unanswered, colliding or foreign create is an error, and so is an after with no sequence beyond
// it; the caller reads every error as not idle.
func (p *idleProbe) pending(ctx context.Context, after uint64) (uint64, error) {
	answer, config, err := p.create(ctx, after)
	if err != nil {
		return 0, err
	}
	return answer.pendingFor(config)
}

// create sends the probe's create, from the sequence after after, within idleProbeBound, and
// answers the server's answer with the configuration it asked for; an answer that names an error
// is an error. A create the server refuses because a probe of the name still stands is tried
// again, with a description and a delivery subject of its own, after idleProbeRetry, until the
// bound.
func (p *idleProbe) create(ctx context.Context, after uint64) (probeAnswer, probeConfig, error) {
	if after == math.MaxUint64 {
		return probeAnswer{}, probeConfig{}, fmt.Errorf("the Dispatch consumer's delivered sequence %d has no sequence after it", after)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, idleProbeBound)
	defer cancel()
	for {
		answer, config, err := p.createOnce(bounded, after)
		var taken *jetstream.APIError
		if !errors.As(err, &taken) || taken.ErrorCode != jetstream.JSErrCodeConsumerExists {
			return answer, config, err
		}
		select {
		case <-bounded.Done():
			return answer, config, err
		case <-time.After(idleProbeRetry):
		}
	}
}

// createOnce is one try of create, within ctx.
func (p *idleProbe) createOnce(ctx context.Context, after uint64) (probeAnswer, probeConfig, error) {
	inbox := p.conn.NewInbox()
	config := probeConfig{
		Name:              p.name,
		Description:       "legion idle probe " + inbox[strings.LastIndexByte(inbox, '.')+1:],
		DeliverPolicy:     "by_start_sequence",
		OptStartSeq:       after + 1,
		AckPolicy:         "none",
		FilterSubject:     p.filter,
		ReplayPolicy:      "instant",
		DeliverSubject:    inbox,
		HeadersOnly:       true,
		InactiveThreshold: idleProbeInactive,
		Replicas:          1,
		MemoryStorage:     true,
		Direct:            true,
	}
	body, err := json.Marshal(probeRequest{Stream: notificationStream, Config: config, Action: "create"})
	if err != nil {
		return probeAnswer{}, config, fmt.Errorf("encode the probe's create: %w", err)
	}
	reply, err := p.conn.RequestMsgWithContext(ctx, &nats.Msg{Subject: p.probeSubject(), Data: body})
	if err != nil {
		return probeAnswer{}, config, fmt.Errorf("create the probe %s: %w", p.name, err)
	}
	var answer probeAnswer
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		return probeAnswer{}, config, fmt.Errorf("read the answer to the probe %s: %w", p.name, err)
	}
	if answer.Error != nil {
		return probeAnswer{}, config, fmt.Errorf("create the probe %s: %w", p.name, answer.Error)
	}
	return answer, config, nil
}

// pendingFor is the answer's initial pending count, once the answer is the create config sent:
// the probe's name, the attempt's description, delivery subject, filter and start, and nothing
// delivered yet, which a consumer's info has only before it starts to deliver. The server starts
// the probe at the stream's first stored message when that is past after+1, since the messages
// before it are gone (selectStartingSeqNo), so its delivered stream sequence may stand past after
// but never before it: a quiet project's durable can stand far behind a stream other projects keep
// moving, and what the leader counts from there is still everything stored after after.
func (a probeAnswer) pendingFor(config probeConfig) (uint64, error) {
	got := a.Config
	if a.Name != config.Name || got.Description != config.Description || got.DeliverSubject != config.DeliverSubject ||
		got.FilterSubject != config.FilterSubject || got.DeliverPolicy != jetstream.DeliverByStartSequencePolicy ||
		got.OptStartSeq != config.OptStartSeq || a.Delivered.Consumer != 0 || a.Delivered.Stream < config.OptStartSeq-1 {
		return 0, fmt.Errorf("%w: %s, description %q, delivery subject %q, filter %q, start %d, delivered %d at stream sequence %d; want %s, %q, %q, %q, %d, none, at %d or later",
			errProbeAnswer, a.Name, got.Description, got.DeliverSubject, got.FilterSubject, got.OptStartSeq, a.Delivered.Consumer, a.Delivered.Stream,
			config.Name, config.Description, config.DeliverSubject, config.FilterSubject, config.OptStartSeq, config.OptStartSeq-1)
	}
	return a.NumPending, nil
}
