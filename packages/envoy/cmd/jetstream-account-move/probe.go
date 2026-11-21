package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// probeOptions are the account probe's subjects and its remover, the operator of the account that
// holds JetStream, by its seed.
type probeOptions struct {
	stream, target, control, removerSeedFile string
}

func (p *probeOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&p.stream, "stream", bus.Stream, "probe: the stream whose info the ordinary request asks for")
	fs.StringVar(&p.target, "target", "notifications.dispatch.issue.ZZPROBE.1", "probe: an unused subject the stream captures, the injected reply's subject")
	fs.StringVar(&p.control, "control-subject", "$KV.envoy_session_consumers.probe", "probe: a KV key the probe's identity may not write, its control publish")
	fs.StringVar(&p.removerSeedFile, "remover-seed-file", "", "probe: a file holding the nkey seed of the JetStream account's operator, which removes what the probe stored")
}

// The probe's bounded reads: how long it waits for the server's refusal of its control publish,
// and how often, and how far apart, it reads the target's count after the injected request. On a
// hub that stores the answer the first read finds it (the merge base's layout in the probe's test);
// the reads are the probe's wait for a store it cannot otherwise see, since its identity may not
// subscribe the target.
const (
	controlWait  = 5 * time.Second
	storedReads  = 10
	storedPacing = 200 * time.Millisecond
)

// run probes the hub as the identity nc connected as, which is the listener's in production: its
// ordinary STREAM.INFO request must come back with the stream's configuration, else UNKNOWN, since
// an identity refused everything would store nothing and read clean; its plain publish to a KV
// key it may not write must be refused, else UNKNOWN; then the same request, with its reply aimed
// at the unused captured subject target, must leave nothing stored there for GATE OPEN. Anything
// stored is GATE SHUT, and is removed by the remover when one is named, each removal printed;
// without one, each stored sequence is named as left.
func (p probeOptions) run(ctx context.Context, nc *nats.Conn, conn connection, o out) (int, error) {
	var mu sync.Mutex
	var refusals []string
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		mu.Lock()
		defer mu.Unlock()
		refusals = append(refusals, err.Error())
	})
	refused := func(subject string) bool {
		mu.Lock()
		defer mu.Unlock()
		want := fmt.Sprintf("Permissions Violation for Publish to %q", subject)
		for _, r := range refusals {
			if strings.Contains(r, want) {
				return true
			}
		}
		return false
	}

	ordinary := apiPrefix + "STREAM.INFO." + p.stream
	var answered struct {
		Type   string `json:"type"`
		Config struct {
			Name     string   `json:"name"`
			Subjects []string `json:"subjects"`
		} `json:"config"`
	}
	if err := request(ctx, nc, ordinary, nil, &answered); err != nil {
		o.line("UNKNOWN the ordinary request %s was not answered with the stream: %v", ordinary, err)
		return o.verdict("GATE OPEN", 0, 1), nil
	}
	if answered.Config.Name != p.stream {
		o.line("UNKNOWN the ordinary request %s answered %s, not the stream's configuration", ordinary, answered.Type)
		return o.verdict("GATE OPEN", 0, 1), nil
	}
	if !captures(answered.Config.Subjects, p.target) {
		o.line("UNKNOWN %s is no subject %s captures (%s), so nothing could be stored there", p.target, p.stream, strings.Join(answered.Config.Subjects, ", "))
		return o.verdict("GATE OPEN", 0, 1), nil
	}
	o.line("ANSWERED %s with the stream's configuration", ordinary)

	before, err := storedOn(ctx, nc, ordinary, p.target)
	if err != nil {
		o.line("UNKNOWN the count on %s could not be read: %v", p.target, err)
		return o.verdict("GATE OPEN", 0, 1), nil
	}

	if err := nc.Publish(p.control, []byte("jetstream-account-probe")); err != nil {
		return exitUnknown, err
	}
	if err := nc.Flush(); err != nil {
		return exitUnknown, err
	}
	deadline := time.Now().Add(controlWait)
	for !refused(p.control) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !refused(p.control) {
		o.line("UNKNOWN the control publish to %s was not refused within %s: this identity may write it, or the hub reports no refusal", p.control, controlWait)
		return o.verdict("GATE OPEN", 0, 1), nil
	}
	o.line("REFUSED the control publish to %s", p.control)

	if err := nc.PublishRequest(ordinary, p.target, nil); err != nil {
		return exitUnknown, err
	}
	if err := request(ctx, nc, ordinary, nil, nil); err != nil {
		o.line("UNKNOWN the ordinary request after the injected one was not answered: %v", err)
		return o.verdict("GATE OPEN", 0, 1), nil
	}
	after := before
	for range storedReads {
		if after, err = storedOn(ctx, nc, ordinary, p.target); err != nil {
			o.line("UNKNOWN the count on %s could not be read: %v", p.target, err)
			return o.verdict("GATE OPEN", 0, 1), nil
		}
		if after > 0 {
			break
		}
		time.Sleep(storedPacing)
	}
	if after == 0 {
		o.line("NOTHING STORED on %s after the request whose reply was aimed there", p.target)
		return o.verdict("GATE OPEN", 0, 0), nil
	}
	o.line("STORED %d on %s (%d before the injected request)", after, p.target, before)
	p.remove(ctx, nc, conn, o)
	return o.verdict("GATE OPEN", 1, 0), nil
}

// captures reports whether a stream with subjects stores a publish to subject.
func captures(subjects []string, subject string) bool {
	for _, s := range subjects {
		p, t := strings.Split(s, "."), strings.Split(subject, ".")
		matched := len(p) == len(t)
		for i, token := range p {
			if token == ">" {
				matched = len(t) > i
				break
			}
			if i >= len(t) || (token != "*" && token != t[i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// storedOn is how many messages the stream holds on subject, read through the ordinary request
// with a subject filter.
func storedOn(ctx context.Context, nc *nats.Conn, ordinary, subject string) (uint64, error) {
	var i streamInfo
	if err := request(ctx, nc, ordinary, map[string]string{"subjects_filter": subject}, &i); err != nil {
		return 0, err
	}
	return i.State.Subjects[subject], nil
}

// stored lists the stream sequences of the messages the stream holds on the probe's target, read
// one by one with next_by_subj.
func (p probeOptions) stored(ctx context.Context, nc *nats.Conn) ([]uint64, error) {
	var seqs []uint64
	next := uint64(1)
	for {
		var answer struct {
			Message struct {
				Seq uint64 `json:"seq"`
			} `json:"message"`
		}
		err := request(ctx, nc, apiPrefix+"STREAM.MSG.GET."+p.stream, map[string]any{"seq": next, "next_by_subj": p.target}, &answer)
		var api *apiError
		if errors.As(err, &api) && api.Code == 404 {
			return seqs, nil
		}
		if err != nil {
			return seqs, err
		}
		seqs = append(seqs, answer.Message.Seq)
		next = answer.Message.Seq + 1
	}
}

// remove deletes each message stored on the target as the remover, the JetStream account's
// operator, printing REMOVED per sequence; without a remover, or when the remover cannot, it names
// each sequence it leaves.
func (p probeOptions) remove(ctx context.Context, nc *nats.Conn, conn connection, o out) {
	seqs, err := p.stored(ctx, nc)
	if err != nil {
		o.line("UNKNOWN the stored sequences on %s could not be read: %v", p.target, err)
	}
	if p.removerSeedFile == "" {
		for _, seq := range seqs {
			o.line("LEFT %s seq=%d in %s: no --remover-seed-file names the JetStream account's operator", p.target, seq, p.stream)
		}
		return
	}
	remover := connection{servers: conn.servers, seedFile: p.removerSeedFile}
	rc, err := remover.dial("jetstream-account-probe remover")
	if err != nil {
		for _, seq := range seqs {
			o.line("LEFT %s seq=%d in %s: the remover could not connect: %v", p.target, seq, p.stream, err)
		}
		return
	}
	defer rc.Close()
	for _, seq := range seqs {
		body, _ := json.Marshal(map[string]any{"seq": seq})
		var deleted struct {
			Success bool `json:"success"`
		}
		if err := request(ctx, rc, apiPrefix+"STREAM.MSG.DELETE."+p.stream, json.RawMessage(body), &deleted); err != nil || !deleted.Success {
			o.line("LEFT %s seq=%d in %s: the remover's delete failed: %v", p.target, seq, p.stream, err)
			continue
		}
		o.line("REMOVED %s seq=%d from %s", p.target, seq, p.stream)
	}
}
