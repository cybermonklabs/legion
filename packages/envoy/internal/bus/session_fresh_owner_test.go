package bus

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A record that names another session's consumer never hands that consumer to its key: the
// independent tester's forgery (a well-formed, self-keyed record of ses_v naming ses_a's current,
// written by a principal that can store chosen bytes in the bucket) is refused as a malformed
// record by every session call of ses_v, and ses_a's consumer keeps its own filter and is never
// deleted. The control is the forged record read by a reader that takes a record's current on its
// name alone, which updates ses_a's consumer to ses_v's filter.
func TestARecordNamingAnotherSessionsConsumerNeverHandsItToItsKey(t *testing.T) {
	owner, consumers, store := freshSessionConsumersOnStream(t)
	ctx := context.Background()
	const victim, forger = "ses_a_owner", "ses_v_forger"
	publishStored(t, owner, contracts.LegacyAgentSubject(victim), "for the victim")
	victimSubjects := []string{contracts.LegacyAgentSubject(victim)}
	start, err := consumers.Ensure(ctx, victim, victimSubjects, 0)
	if err != nil {
		t.Fatal(err)
	}
	filters := func() []string {
		t.Helper()
		info, err := consumers.info(ctx, start.Name)
		if err != nil {
			t.Fatalf("read %s: %v", start.Name, err)
		}
		return slices.Clone(info.Config.FilterSubjects)
	}
	victimFilters := filters()

	// The forgery: a record of the forger's key naming the victim's consumer, written whole as a
	// principal with a chosen-bytes write of the bucket writes it.
	if _, err := store.Write(ctx, forger, consumernames.Record{Current: start.Name, Start: 1}, 0); err != nil {
		t.Fatal(err)
	}
	forgerSubjects := []string{contracts.LegacyAgentSubject(forger)}
	for name, call := range map[string]func() error{
		"Ensure": func() error { _, err := consumers.Ensure(ctx, forger, forgerSubjects, 0); return err },
		"UpdateFilter": func() error {
			return consumers.UpdateFilter(ctx, forger, forgerSubjects)
		},
		"RestartAtFloor": func() error { _, err := consumers.RestartAtFloor(ctx, forger, forgerSubjects); return err },
		"Info":           func() error { _, err := consumers.Info(ctx, forger); return err },
		"KeepListed": func() error {
			info, err := consumers.info(ctx, start.Name)
			if err != nil {
				return err
			}
			_, _, err = consumers.KeepListed(ctx, forger, victimSubjects, info)
			return err
		},
		"Delete": func() error { _, err := consumers.Delete(ctx, forger, nil); return err },
	} {
		if err := call(); !errors.Is(err, consumernames.ErrMalformedRecord) {
			t.Errorf("%s of the forger's key = %v, want ErrMalformedRecord", name, err)
		}
	}
	if got := filters(); !slices.Equal(got, victimFilters) {
		t.Fatalf("the victim's consumer %s filters %v after the forger's calls, want its own %v", start.Name, got, victimFilters)
	}

	// Control: a reader that takes the current on its name alone hands the victim's consumer to
	// the forger, as the protocol did before it read the consumer's metadata.
	record, _, err := store.Read(ctx, forger)
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumers.info(ctx, record.Current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumers.update(ctx, forger, record.Current, info, SessionConsumerFilters(forgerSubjects)); err != nil {
		t.Fatal(err)
	}
	if got := filters(); slices.Equal(got, victimFilters) {
		t.Fatalf("control: the name-only reader left %s on %v, want the forger's filter", start.Name, got)
	}
}

// A candidate an open create names that someone else made, with the session's owner metadata and
// start (the deep review's pre-created next name), is adopted only as the family creates it, and
// its filters are corrected to the call's own before Ensure returns. One with a setting the
// family's create never gives and no update changes (an ack policy of all, a push deliver subject,
// another replay policy, memory storage, another max waiting) is refused as ErrUnexpectedConsumer
// naming it and that setting, as the server names it, left as it was made and never published: an
// earlier adoption published the push one, whose every pull the server answered 409. One that
// filters on another session's subject filters on this session's alone once adopted. The control
// for the filter row is the candidate as it was made, filtering on the other session. A setting an
// update changes is corrected instead, and a pause is refused too
// (TestAPrecreatedCandidateThatWouldNotDeliverIsCorrectedOrRefused).
func TestAPrecreatedCandidateIsAdoptedOnlyAsTheFamilyCreatesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		made    func(*natsgo.ConsumerConfig)
		setting string
	}{
		{name: "an ack policy of all", made: func(c *natsgo.ConsumerConfig) { c.AckPolicy = natsgo.AckAllPolicy }, setting: `ack_policy "all", not "explicit"`},
		{name: "a push deliver subject", made: func(c *natsgo.ConsumerConfig) { c.DeliverSubject = "elsewhere.deliver" }, setting: `deliver_subject "elsewhere.deliver", as a push consumer`},
		{name: "replay at the original rate", made: func(c *natsgo.ConsumerConfig) { c.ReplayPolicy = natsgo.ReplayOriginalPolicy }, setting: `replay_policy "original", not "instant"`},
		{name: "memory storage", made: func(c *natsgo.ConsumerConfig) { c.MemoryStorage = true }, setting: "mem_storage true, not none"},
		{name: "another max waiting", made: func(c *natsgo.ConsumerConfig) { c.MaxWaiting = 1 }, setting: "max_waiting 1, not 512"},
		{name: "explicit acks, another session's filter", made: func(*natsgo.ConsumerConfig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, consumers, store := freshSessionConsumersOnStream(t)
			ctx := context.Background()
			const session, other = "ses_precreated", "ses_someone_else"
			publishStored(t, owner, contracts.LegacyAgentSubject(session), "one")
			const candidate = SessionConsumerNamePrefix + "~PRECREATEDBYAPRINCIPAL"
			foreign := []string{contracts.LegacyAgentSubject(other)}
			made := natsgo.ConsumerConfig{
				Durable:        candidate,
				DeliverPolicy:  natsgo.DeliverByStartSequencePolicy,
				OptStartSeq:    1,
				AckPolicy:      natsgo.AckExplicitPolicy,
				FilterSubjects: foreign,
				Metadata:       map[string]string{consumernames.LogicalMetadataKey: session, SessionIDMetadataKey: session},
			}
			tc.made(&made)
			if _, err := owner.JS().AddConsumer(Stream, &made); err != nil {
				t.Fatal(err)
			}
			op := &consumernames.Op{Kind: consumernames.OpCreate, Owner: "another process", Candidate: candidate, ReservedAt: time.Now().Add(-time.Minute)}
			if _, err := store.Write(ctx, session, consumernames.Record{Start: 1, Op: op}, 0); err != nil {
				t.Fatal(err)
			}
			before, err := owner.JS().ConsumerInfo(Stream, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(before.Config.FilterSubjects, foreign) {
				t.Fatalf("control: the candidate as made filters %v, want %v", before.Config.FilterSubjects, foreign)
			}

			start, err := consumers.Ensure(ctx, session, []string{contracts.LegacyAgentSubject(session)}, 0)
			after, infoErr := owner.JS().ConsumerInfo(Stream, candidate)
			if infoErr != nil {
				t.Fatal(infoErr)
			}
			if tc.setting != "" {
				if !errors.Is(err, consumernames.ErrUnexpectedConsumer) || !strings.Contains(err.Error(), candidate) || !strings.Contains(err.Error(), tc.setting) {
					t.Fatalf("Ensure over %s = %+v, %v; want ErrUnexpectedConsumer naming it and %s", candidate, start, err, tc.setting)
				}
				if record := selectionRecord(t, store, session); record.Current != "" {
					t.Fatalf("the refused candidate was published: %+v", record)
				}
				if !reflect.DeepEqual(after.Config, before.Config) {
					t.Fatalf("the refused candidate was changed: %+v, made as %+v", after.Config, before.Config)
				}
				return
			}
			if err != nil || start.Name != candidate {
				t.Fatalf("Ensure = %+v, %v; want %s adopted", start, err, candidate)
			}
			if want := SessionConsumerFilters([]string{contracts.LegacyAgentSubject(session)}); !slices.Equal(after.Config.FilterSubjects, want) {
				t.Fatalf("the adopted candidate filters %v after Ensure, want the session's own %v", after.Config.FilterSubjects, want)
			}
		})
	}
}

// A candidate an open create names that someone else made, with the session's owner metadata and
// start, and a setting the family's create never gives that would keep the session from receiving
// it (the deep review's pause a day ahead and max batch 1, and headers only), is corrected or
// refused before Ensure returns. A max batch or headers only, which an update changes, is
// corrected to the whole configuration the family's create gives, so the session then receives
// its message whole. A pause, which nats-server keeps across every update (only a CONSUMER.PAUSE
// request changes it), is refused as ErrUnexpectedConsumer naming pause_until, and never
// published. Before, the correction reset five settings alone, and the adopted candidate delivered
// nothing (paused), answered every pull 409 (max batch 1), or was refused only for headers only.
// The control is that five-setting reset applied to each such candidate in place, over which the
// session receives nothing whole.
func TestAPrecreatedCandidateThatWouldNotDeliverIsCorrectedOrRefused(t *testing.T) {
	dayAhead := time.Now().Add(24 * time.Hour).UTC()
	for _, tc := range []struct {
		name    string
		made    func(*jetstream.ConsumerConfig)
		refused string
	}{
		{name: "paused a day ahead", made: func(c *jetstream.ConsumerConfig) { c.PauseUntil = &dayAhead }, refused: "pause_until"},
		{name: "max batch 1", made: func(c *jetstream.ConsumerConfig) { c.MaxRequestBatch = 1 }},
		{name: "headers only", made: func(c *jetstream.ConsumerConfig) { c.HeadersOnly = true }},
	} {
		for _, control := range []bool{false, true} {
			name := tc.name
			if control {
				name = "control: today's five-setting reset of one " + tc.name
			}
			t.Run(name, func(t *testing.T) {
				owner, consumers, store := freshSessionConsumersOnStream(t)
				ctx := context.Background()
				const session = "ses_precreated_updatable"
				subjects := []string{contracts.LegacyAgentSubject(session)}
				filters := SessionConsumerFilters(subjects)
				stored := publishStored(t, owner, subjects[0], "the payload")
				const candidate = SessionConsumerNamePrefix + "~PRECREATEDUPDATABLE"
				made := jetstream.ConsumerConfig{
					Durable:       candidate,
					DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
					OptStartSeq:   1,
					Metadata:      sessionConsumerMetadata(session),
				}
				consumers.applySettings(&made, filters)
				tc.made(&made)
				js, err := jetstream.New(owner.Connection())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := js.CreateConsumer(ctx, Stream, made); err != nil {
					t.Fatal(err)
				}
				if control {
					info, err := consumers.info(ctx, candidate)
					if err != nil {
						t.Fatal(err)
					}
					next := info.Config
					consumers.applySettings(&next, filters)
					if _, err := consumers.upsert(ctx, candidate, next, "update"); err != nil {
						t.Fatal(err)
					}
				} else {
					op := &consumernames.Op{Kind: consumernames.OpCreate, Owner: "another process", Candidate: candidate, ReservedAt: time.Now().Add(-time.Minute)}
					if _, err := store.Write(ctx, session, consumernames.Record{Start: 1, Op: op}, 0); err != nil {
						t.Fatal(err)
					}
					start, err := consumers.Ensure(ctx, session, subjects, 0)
					if tc.refused != "" {
						if !errors.Is(err, consumernames.ErrUnexpectedConsumer) || !strings.Contains(err.Error(), candidate) || !strings.Contains(err.Error(), tc.refused) {
							t.Fatalf("Ensure over a candidate %s = %+v, %v; want ErrUnexpectedConsumer naming it and %s", tc.name, start, err, tc.refused)
						}
						if record := selectionRecord(t, store, session); record.Current != "" {
							t.Fatalf("the refused candidate was published: %+v", record)
						}
						return
					}
					if err != nil || start.Name != candidate {
						t.Fatalf("Ensure = %+v, %v; want %s adopted", start, err, candidate)
					}
				}
				pull := pullSessionNamed(t, consumers, session, candidate)
				nextCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				msg, err := pull.Next(nextCtx)
				whole := err == nil && msg.StreamSeq == stored && string(msg.Data) == "the payload"
				if control {
					if whole {
						t.Fatalf("control: the five-setting reset of a candidate %s delivered %d %q whole; want nothing whole", tc.name, msg.StreamSeq, msg.Data)
					}
					return
				}
				if !whole {
					t.Fatalf("the session's pull of the adopted candidate = %d %q, %v; want %d %q", msg.StreamSeq, msg.Data, err, stored, "the payload")
				}
				info, err := consumers.info(ctx, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if config := info.Config; config.MaxRequestBatch != 0 || config.HeadersOnly {
					t.Fatalf("the adopted candidate after Ensure: max batch %d, headers only %t; want neither", config.MaxRequestBatch, config.HeadersOnly)
				}
			})
		}
	}
}
