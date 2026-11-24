package bus

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The adoption's allowlist names exactly the settings nats-server answers in a consumer's
// configuration: a setting the server answers that the table lacks would refuse every candidate,
// and one the table names that the server no longer answers would hide that the table is stale.
// Upgrading the server module re-reads its ConsumerConfig here.
func TestTheAdoptionAllowlistNamesEverySettingTheServerAnswers(t *testing.T) {
	var answered []string
	config := reflect.TypeFor[natsserver.ConsumerConfig]()
	for field := range config.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			answered = append(answered, name)
		}
	}
	slices.Sort(answered)
	if listed := slices.Sorted(maps.Keys(consumerSettings)); !slices.Equal(listed, answered) {
		t.Fatalf("the allowlist names %v, the server answers %v", listed, answered)
	}
}

// sessionCreateAsAnswered is a session consumer's create as the server answers it: the create's
// JSON with the server's defaults for what it leaves unset (a pull consumer's max waiting, the
// replicas it inherits).
func sessionCreateAsAnswered(t *testing.T, create jetstream.ConsumerConfig) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	var held map[string]json.RawMessage
	if err := json.Unmarshal(data, &held); err != nil {
		t.Fatal(err)
	}
	held["max_waiting"] = json.RawMessage("512")
	held["num_replicas"] = json.RawMessage("0")
	held["name"] = held["durable_name"]
	return held
}

// A candidate whose configuration holds a setting this build does not know, as a newer server's
// could, is refused naming it: the adoption is an allowlist, not a list of refusals. The control is
// the same candidate without it, which is adopted.
func TestAnAdoptionRefusesASettingThisBuildDoesNotKnow(t *testing.T) {
	consumers := &SessionConsumers{ackWait: time.Minute}
	create := consumers.freshConfig("session~A", "ses_a", 3, nil)
	held := sessionCreateAsAnswered(t, create)
	if why, err := adoptionRefusals(held, create, natsgo.InboxPrefix); err != nil || len(why) != 0 {
		t.Fatalf("control: the candidate as its family creates it = %v, %v; want it adopted", why, err)
	}
	held["future_setting"] = json.RawMessage(`"on"`)
	why, err := adoptionRefusals(held, create, natsgo.InboxPrefix)
	if err != nil || len(why) != 1 || !strings.Contains(why[0], `future_setting "on"`) {
		t.Fatalf("a candidate with a setting this build does not know = %v, %v; want it refused naming the setting", why, err)
	}
}

// The correction writes the family's create in every setting an update changes and keeps what no
// update changes from the consumer as the server answers it, with the metadata the server sets
// itself; applied twice, it changes nothing the second time. Every setting the allowlist names
// that the jetstream package types is set on the consumer here, so a setting the correction
// should keep but did not would show.
func TestTheCorrectionKeepsWhatNoUpdateChanges(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	pause := start.Add(24 * time.Hour)
	current := jetstream.ConsumerConfig{
		Name: "session~A", Durable: "session~A",
		DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartSeq: 4, OptStartTime: &start,
		AckPolicy: jetstream.AckAllPolicy, ReplayPolicy: jetstream.ReplayOriginalPolicy,
		MemoryStorage: true, FlowControl: true, IdleHeartbeat: time.Second,
		DeliverSubject: "_INBOX.held", DeliverGroup: "group", MaxWaiting: 7,
		Description: "made elsewhere", AckWait: time.Second, MaxDeliver: 2, BackOff: []time.Duration{time.Second},
		FilterSubject: "notifications.other", RateLimit: 9, SampleFrequency: "50%", MaxAckPending: 3,
		HeadersOnly: true, MaxRequestBatch: 1, MaxRequestExpires: time.Second, MaxRequestMaxBytes: 5,
		InactiveThreshold: time.Second, Replicas: 1, PauseUntil: &pause,
		PriorityPolicy: jetstream.PriorityPolicyOverflow, PinnedTTL: time.Second, PriorityGroups: []string{"g"},
		Metadata: map[string]string{"envoy.logical": "ses_a", "elsewhere": "x", "_nats.req.level": "1", "_nats.ver": "2.15.1"},
	}
	consumers := &SessionConsumers{ackWait: time.Minute, inactiveThreshold: time.Hour}
	create := consumers.freshConfig("session~B", "ses_a", 1, []string{"notifications.agent.ses_a"})
	next, changed := CorrectedConfig(current, create)
	if !changed {
		t.Fatal("the correction of a consumer made elsewhere changes nothing")
	}
	asJSON := func(config jetstream.ConsumerConfig) map[string]json.RawMessage {
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	held, wrote, wanted := asJSON(current), asJSON(next), asJSON(create)
	for key, setting := range consumerSettings {
		switch {
		case key == "direct" || key == "sourcing":
			// The jetstream package types neither, so no update it sends carries them.
		case key == "metadata":
		case setting == settingCorrected:
			if !sameSetting(wrote[key], wanted[key]) {
				t.Errorf("the correction wrote %s %s, want the create's %s", key, shown(wrote[key]), shown(wanted[key]))
			}
		default:
			if _, set := held[key]; !set {
				t.Errorf("the test sets no %s on the consumer, so it cannot see the correction keep it", key)
			}
			if !sameSetting(wrote[key], held[key]) {
				t.Errorf("the correction wrote %s %s, want the consumer's %s, which no update changes", key, shown(wrote[key]), shown(held[key]))
			}
		}
	}
	want := map[string]string{"envoy.logical": "ses_a", SessionIDMetadataKey: "ses_a", "_nats.req.level": "1", "_nats.ver": "2.15.1"}
	if !maps.Equal(next.Metadata, want) {
		t.Errorf("the correction's metadata %v, want the create's and the server's own %v", next.Metadata, want)
	}
	if again, changed := CorrectedConfig(next, create); changed {
		t.Errorf("the correction of its own result changes it again: %+v", again)
	}
}
