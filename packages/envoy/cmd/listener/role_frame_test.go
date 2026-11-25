package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/counttest"
	"github.com/sjawhar/envoy/internal/store"
)

// readRoleFrame refuses a document that is not a JSON object, and each object the plugins'
// JSON.parse refuses at the object's own level, as its doc comment says (eachTopLevelField), and tells
// the role of each object JSON.parse reads, however its top level is written. A refused document is
// no role message, so the relay sends it to the session whose subject holds it, as it sends every
// frame that is no role message.
func TestTheRoleReadRefusesWhatJSONParseRefusesAtTheTopLevel(t *testing.T) {
	const role = `"topic":"notifications.role.reader"`
	refused := []struct{ name, data string }{
		{"not an object", `["notifications.role.reader"]`},
		{"empty", ``},
		{"only whitespace", " \t\r\n"},
		{"a vertical tab before the object", "\v{" + role + "}"},
		{"a form feed before the object", "\f{" + role + "}"},
		{"null", `null`},
		{"a string with no end", `{"topic":"notifications.role.reader`},
		{"a key with no end", `{"top`},
		{"a key that is no string", `{topic:"notifications.role.reader"}`},
		{"a bad escape in a key", `{"top\qic":"notifications.role.reader"}`},
		{"a missing colon", `{"topic" "notifications.role.reader"}`},
		{"a missing comma", `{` + role + ` "role":"reader"}`},
		{"a missing value", `{"x":,` + role + `}`},
		{"a trailing comma", `{` + role + `,}`},
		{"an object with no end", `{` + role},
		{"an object cut after its brace", `{`},
		{"an object cut after a key", `{"topic"`},
		{"an object cut after a colon", `{"topic":`},
		{"an array with no end", `{` + role + `,"nested":[[1]}`},
		{"text after the object", `{` + role + `}x`},
		{"text after an empty object", `{}x`},
		{"a second object", `{` + role + `}{` + role + `}`},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			var parsed map[string]json.RawMessage
			if json.Unmarshal([]byte(c.data), &parsed) == nil && parsed != nil {
				t.Fatalf("encoding/json reads %q as an object; each case must be no JSON object, or an object JSON.parse refuses too", c.data)
			}
			if _, err := readRoleFrame([]byte(c.data)); !errors.Is(err, errRoleFrameUnreadable) {
				t.Errorf("readRoleFrame(%q) answers %v; want errRoleFrameUnreadable", c.data, err)
			}
		})
	}

	read := []struct{ name, data string }{
		{"whitespace around the object", " \t\r\n{" + role + "} \n"},
		{"one byte order mark", "\xef\xbb\xbf{" + role + "}"},
		{"brackets, commas and an escaped quote in a string before the topic", `{"payload":"]},{\"[","topic":"notifications.role.reader"}`},
		{"an escaped backslash ending a string before the topic", `{"payload":"a\\",` + role + `}`},
		{"a nested object that holds another topic", `{"meta":{"topic":"notifications.agent.ses_x"},` + role + `}`},
		{"a value nested 100,000 deep", `{"nested":` + strings.Repeat("[", 100_000) + strings.Repeat("]", 100_000) + `,` + role + `}`},
		{"the last of a repeated key", `{"topic":"notifications.agent.ses_x",` + role + `}`},
		{"an escaped key", `{"top\u0069c":"notifications.role.reader"}`},
	}
	for _, c := range read {
		t.Run("read: "+c.name, func(t *testing.T) {
			frame, err := readRoleFrame([]byte(c.data))
			if err != nil || frame.role != "reader" {
				t.Errorf("readRoleFrame answers role %q and %v; want role reader", frame.role, err)
			}
		})
	}
}

// envelope reads an integer field written as a JSON number, with or without a zero fraction, and
// leaves out one written as a JSON string that holds a number: encoding/json refuses such a string
// for an integer, and the envelope's schema types the field as an integer, so a copy that took it
// would carry a time the original does not.
func TestTheEnvelopeReadTakesAWholeNumberAndNoStringHoldingOne(t *testing.T) {
	read := func(t *testing.T, data string) (int64, string) {
		t.Helper()
		frame, err := readRoleFrame([]byte(data))
		if err != nil {
			t.Fatalf("readRoleFrame(%q): %v", data, err)
		}
		item := frame.envelope()
		expires := "none"
		if item.ExpiresAt != nil {
			expires = strconv.FormatInt(*item.ExpiresAt, 10)
		}
		return item.IssuedAt, expires
	}
	t.Run("a whole number, and one written with a zero fraction, is read", func(t *testing.T) {
		issued, expires := read(t, `{"topic":"notifications.role.reader","issued_at":1795191371979.0,"expires_at":7}`)
		if issued != 1795191371979 || expires != "7" {
			t.Errorf("envelope reads issued_at %d and expires_at %s; want 1795191371979 and 7", issued, expires)
		}
	})
	t.Run("a string that holds a number is left out", func(t *testing.T) {
		issued, expires := read(t, `{"topic":"notifications.role.reader","issued_at":"1795191371979","expires_at":"7"}`)
		if issued != 0 || expires != "none" {
			t.Errorf("envelope reads issued_at %d and expires_at %s; want 0 and none, as encoding/json reads neither", issued, expires)
		}
	})
}

// readRoleFrame keys a role mark as the role's claim is keyed (store.RoleClaimKeyOf), as it keys a
// role lane's topic (roleOfLaneTopic): a free-form role under a project is that project's own
// claim, <project>.<role>, and every other name is its bare name. A takeover pass copies a message
// only to the role its read names (takenRoleMessage), so a mark read as the bare name would copy a
// project's message to the lane of the claim of the same name under no project, and the session
// relay would read that claim's holder for it.
func TestARoleMarkIsReadAsTheClaimKeyOfTheRoleItNames(t *testing.T) {
	for _, c := range []struct{ mark, key string }{
		{"PRIV.lead-reviewer", "PRIV.lead-reviewer"},
		{"INFRA.reviewer", "INFRA.reviewer"},
		{"_.lead-reviewer", "lead-reviewer"},
		{"lead-reviewer", "lead-reviewer"},
		{"ZZ.legion-zz-controller", "legion-zz-controller"},
		{"legion-zz-controller", "legion-zz-controller"},
	} {
		t.Run(c.mark, func(t *testing.T) {
			if got := store.RoleClaimKeyOf(c.mark); got != c.key {
				t.Fatalf("store.RoleClaimKeyOf(%q) = %q; want %q", c.mark, got, c.key)
			}
			lane, ok := roleOfLaneTopic("notifications.role." + c.mark)
			if !ok || lane != c.key {
				t.Fatalf("roleOfLaneTopic of the role's topic answers %q, %t; want %q", lane, ok, c.key)
			}
			data := []byte(`{"event_id":"evt-mark","topic":"notifications.agent.ses_marked","role":"` + c.mark + `","message":"for the role"}`)
			frame, err := readRoleFrame(data)
			if err != nil || frame.role != c.key {
				t.Errorf("readRoleFrame of a message marked %q answers role %q and %v; want %q, the key roleOfLaneTopic gives its topic", c.mark, frame.role, err, c.key)
			}
			if item, ok := takenRoleMessage(data, c.key); !ok || item.EventID != "evt-mark" {
				t.Errorf("a takeover pass of role %s takes the message marked %q: %t (event %q); want it taken", c.key, c.mark, ok, item.EventID)
			}
		})
	}
}

// roleFrameErrorBound is the most an error of readRoleFrame may hold: a few dozen bytes of a key
// beside the words. The kept lane's walk logs the error whole (judgeKeptRoleCopy), and a key or a
// value can be as long as the message.
const roleFrameErrorBound = 256

// readRoleFrame's error names the field and the JSON type it holds, and at most a few dozen
// characters of a key as the error quotes it, however long the value or the key, and whatever bytes
// the key holds: an 8 MiB topic nested 4 Mi deep, a key of 1 MiB with no colon, a bad escape or no
// value after it, and a key of bytes the quote writes as four characters each (0xff, a control
// byte), each give an error of at most roleFrameErrorBound bytes.
func TestAnUnreadableRoleFrameIsNamedInAFewDozenBytes(t *testing.T) {
	nested := strings.Repeat("[", 4<<20) + strings.Repeat("]", 4<<20)
	key := strings.Repeat("k", 1<<20)
	invalid32, invalid1M := strings.Repeat("\xff", 32), strings.Repeat("\xff", 1<<20)
	control32 := strings.Repeat("\x01\x1f", 16)
	for _, c := range []struct{ name, data, names string }{
		{"an 8 MiB nested topic", `{"topic":` + nested + `}`, "its topic is a JSON array, not a string"},
		{"an 8 MiB nested role mark under a topic that names no role", `{"topic":"notifications.agent.ses_x","role":` + nested + `}`, "its role is a JSON array, not a string"},
		{"a topic that is a 1 MiB number", `{"topic":` + strings.Repeat("7", 1<<20) + `}`, "its topic is a JSON number, not a string"},
		{"a topic that is a 1 MiB string with a bad escape", `{"topic":"` + key + `\q"}`, "its topic is a JSON string that does not decode"},
		{"a 1 MiB key with no colon", `{"` + key + `" 1}`, "has no colon after it"},
		{"a 1 MiB key with a bad escape", `{"` + key + `\q":1}`, "holds a bad escape"},
		{"a 1 MiB key with no value", `{"` + key + `":}`, "it is missing"},
		{"a key of 32 0xff bytes with no colon", `{"` + invalid32 + `" 1}`, "has no colon after it"},
		{"a key of 32 0xff bytes with a bad escape", `{"` + invalid32 + `\q":1}`, "holds a bad escape"},
		{"a key of 32 0xff bytes whose value has no end", `{"` + invalid32 + `":[`, "an object or array has no end"},
		{"a key of 1 MiB 0xff bytes with no colon", `{"` + invalid1M + `" 1}`, "has no colon after it"},
		{"a key of 1 MiB 0xff bytes whose value has no end", `{"` + invalid1M + `":[`, "an object or array has no end"},
		{"a key of 32 control bytes with no colon", `{"` + control32 + `" 1}`, "has no colon after it"},
		{"a key of 32 control bytes whose value has no end", `{"` + control32 + `":[`, "an object or array has no end"},
		{"a key of 32 0xff bytes whose value is a string with no end", `{"` + invalid32 + `":"abc`, "has no end"},
		{"a key of 1 MiB 0xff bytes whose value is a string with no end", `{"` + invalid1M + `":"abc`, "has no end"},
		{"a key of 1 MiB control bytes whose value is a string with no end", `{"` + strings.Repeat("\x01", 1<<20) + `":"abc`, "has no end"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := readRoleFrame([]byte(c.data))
			if !errors.Is(err, errRoleFrameUnreadable) {
				t.Fatalf("readRoleFrame answers %.200v; want errRoleFrameUnreadable", err)
			}
			if text := err.Error(); len(text) > roleFrameErrorBound || !strings.Contains(text, c.names) {
				t.Errorf("readRoleFrame's error is %d bytes, starting %.200q; want at most %d bytes, naming %q", len(text), text, roleFrameErrorBound, c.names)
			}
		})
	}
}

// manyKeysFrame is a message on topic whose top level holds an event id, the topic and keys
// more distinct short keys, every other one written with an escape that the read decodes to tell
// it from the topic.
func manyKeysFrame(topic string, keys int) []byte {
	var data bytes.Buffer
	data.WriteString(`{"event_id":"evt-many-keys","topic":"` + topic + `"`)
	for i := range keys {
		if i%2 == 0 {
			fmt.Fprintf(&data, `,"k%d":0`, i)
		} else {
			fmt.Fprintf(&data, `,"k\u0030%d":0`, i)
		}
	}
	data.WriteString("}")
	return data.Bytes()
}

// relaysFirstReadAllocsChildEnv marks the run of the test binary that measures the relay's first
// read alone (TestTheRelaysFirstReadOfAFrameMakesNoAllocationForEachKey, counttest.RunAlone).
const relaysFirstReadAllocsChildEnv = "ENVOY_TEST_RELAYS_FIRST_READ_ALLOCS_CHILD"

// relaysFirstReadAllocsMeasured is the phrase that
// TestTheRelaysFirstReadOfAFrameMakesNoAllocationForEachKey logs with each of its measurements, and
// which the run that starts its run alone counts in that run's output (counttest.RunAlone).
const relaysFirstReadAllocsMeasured = "allocations for 8 keys"

// relaysFirstReadAllocsSlack is how many allocations more than for a frame of 8 keys the relay's
// first read of a frame of 100,000 keys may count: the process's own, which the measurement counts
// too (at most 4 for either frame in eight runs of this test alone with -race, 1 without), and
// none of the read's.
const relaysFirstReadAllocsSlack = 100

// The session relay reads every frame it judges before it reads a claim (takeOverFrame), and that
// read keeps the frame's topic, role mark and event id alone, so it makes no more allocations for a
// frame of 100,000 keys (1.3 MiB) than for one of 8: for a frame that is no role message, judged
// whole, and for the first read of a role message, after which the relay reads the role's claim.
// A read that kept every key in a map made more than 200,000 for such a frame, and one that
// decoded each key with escapes into a string of its own more than 300,000.
//
// testing.AllocsPerRun counts every allocation the process makes while it measures, and the other
// tests of this package leave servers and watchers running that allocate meanwhile: a run of the
// whole package counted 27 and 103 for these reads. So the test runs the test binary again, for
// this test alone (counttest.RunAlone), and the measurement is that run's, held to
// relaysFirstReadAllocsSlack above the count for 8 keys; it fails unless that run reports both
// measurements.
func TestTheRelaysFirstReadOfAFrameMakesNoAllocationForEachKey(t *testing.T) {
	if !counttest.RunAlone(t, relaysFirstReadAllocsChildEnv, relaysFirstReadAllocsMeasured, 2) {
		return
	}
	relays := &sessionRelays{}
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		topic string
		judge func(t *testing.T, data []byte)
	}{
		{"a frame that is no role message, which the relay sends", contracts.AgentTopicPrefix + "ses_many_keys", func(t *testing.T, data []byte) {
			if relays.takeOverFrame(ctx, "ses_many_keys", bus.SessionMessage{Data: data}) {
				t.Fatal("the relay took a frame that is no role message")
			}
		}},
		{"a role message, whose claim the relay reads next", contracts.RoleTopicPrefix + "reader", func(t *testing.T, data []byte) {
			if frame, err := readRoleFrame(data); err != nil || frame.role != "reader" {
				t.Fatalf("readRoleFrame answers role %q and %v; want role reader", frame.role, err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			few, many := manyKeysFrame(c.topic, 8), manyKeysFrame(c.topic, 100_000)
			fewAllocs := testing.AllocsPerRun(5, func() { c.judge(t, few) })
			manyAllocs := testing.AllocsPerRun(5, func() { c.judge(t, many) })
			t.Logf("%v "+relaysFirstReadAllocsMeasured+" (%d bytes), %v for 100,000 keys (%d bytes)", fewAllocs, len(few), manyAllocs, len(many))
			if manyAllocs > fewAllocs+relaysFirstReadAllocsSlack {
				t.Errorf("the read makes %v allocations for a frame of 100,000 keys and %v for one of 8; want at most %d more for more keys", manyAllocs, fewAllocs, relaysFirstReadAllocsSlack)
			}
		})
	}
}

// A role message the relay, a takeover pass or the kept lane's walk copies has its every key read
// (roleFrame.envelope), however many other keys come before and after them: the copy holds each
// field of the envelope its sender wrote. Every field of the envelope is set, so a field a later
// version adds is held to this too.
func TestACopiedRoleMessageCarriesEveryFieldAmongManyKeys(t *testing.T) {
	expires, kept := int64(1795191372979), int64(1795191371999)
	want := contracts.Envelope{
		EventID:           "evt-every-field",
		Source:            "agent",
		SourceEventID:     "src-every-field",
		SourceSession:     "ses_every_field_sender",
		Topic:             contracts.RoleTopicPrefix + "reader",
		DedupeKey:         "dedupe-every-field",
		IssuedAt:          1795191371979,
		ExpiresAt:         &expires,
		KeptAt:            &kept,
		MentionDedupeKeys: map[string]string{"ses_mentioned": "mention-key"},
		Role:              "reader",
		PayloadSummary:    "every field",
		PayloadRef:        "ref-every-field",
		Payload:           "the payload",
		TraceID:           "trace-every-field",
		Sender:            &contracts.EnvelopeSender{SessionID: "ses_every_field_sender", Machine: "machine", Cwd: "/cwd", Title: "title", Roles: []string{"writer"}},
		InReplyTo:         "evt-asked",
		Supersedes:        "evt-superseded",
		Urgency:           "high",
		ExpectsReply:      contracts.ExpectsReplyRequired,
	}
	fields := reflect.ValueOf(want)
	for i := range fields.NumField() {
		if fields.Field(i).IsZero() {
			t.Fatalf("the envelope's field %s is not set; set it, so the copy is held to carry it", fields.Type().Field(i).Name)
		}
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.TrimSuffix(manyKeysFrame(contracts.AgentTopicPrefix+"ses_before", 5_000), []byte("}"))
	after := bytes.TrimPrefix(manyKeysFrame(contracts.AgentTopicPrefix+"ses_after", 5_000), []byte("{"))
	// The envelope's own topic and event id come after the ones the keys before it carry, and
	// before the ones after it, so the frame's are the last: the keys after it are renamed.
	after = bytes.Replace(after, []byte(`"event_id"`), []byte(`"after_event_id"`), 1)
	after = bytes.Replace(after, []byte(`"topic"`), []byte(`"after_topic"`), 1)
	inner := bytes.TrimSuffix(bytes.TrimPrefix(encoded, []byte("{")), []byte("}"))
	data := bytes.Join([][]byte{before, inner, after}, []byte(","))

	frame, err := readRoleFrame(data)
	if err != nil || frame.role != "reader" {
		t.Fatalf("readRoleFrame answers role %q and %v; want role reader", frame.role, err)
	}
	if got := frame.envelope(); !reflect.DeepEqual(got, want) {
		t.Errorf("the relay's copy reads\n%+v\nwant every field its sender wrote\n%+v", got, want)
	}
	if got, ok := takenRoleMessage(data, "reader"); !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("a takeover pass takes %t and copies\n%+v\nwant every field its sender wrote\n%+v", ok, got, want)
	}
}

// The relay's first read takes a key with escapes where encoding/json takes it
// (jsonStringTextValid), and reads it, with no allocation, as encoding/json reads it, for each of
// the keys it keeps (keyNames).
func TestAKeyWithEscapesNamesAFieldAsEncodingJSONReadsIt(t *testing.T) {
	tokens := []string{
		`"topic"`, `"top\u0069c"`, `"\u0074\u006f\u0070\u0069\u0063"`, `"\u0074OPIC"`, `"top\u0049c"`, `"topi\u0063 "`,
		`"topic\u0000"`, `"top\u0130c"`, `"\u0174opic"`, `"\ud835\udcc9opic"`, `"t\/opic"`, `"ro\u006ce"`, `"r\u006Fle"`,
		`"role\n"`, `"event\u005fid"`, `"event_\u0069d"`, `"event\\_id"`, `"\"role\""`, `"top\u0069c\u0069"`, `""`,
	}
	refused := []string{`"top\qic"`, `"top\u00g9c"`, `"top\u006"`, `"top\U0069c"`, "\"top\\u0069c\x01\"", "\"\x1f\\n\""}
	for _, token := range append(slices.Clone(tokens), refused...) {
		if got, want := jsonStringTextValid([]byte(token[1:len(token)-1])), json.Valid([]byte(token)); got != want {
			t.Errorf("jsonStringTextValid takes the key %q: %t; encoding/json takes it: %t", token, got, want)
		}
	}
	for _, token := range tokens {
		var decoded string
		if err := json.Unmarshal([]byte(token), &decoded); err != nil {
			t.Fatalf("encoding/json refuses the key %s: %v", token, err)
		}
		for _, name := range []string{"topic", "role", "event_id"} {
			if got := keyNames([]byte(token), name); got != (decoded == name) {
				t.Errorf("keyNames(%s, %q) = %t; encoding/json reads the key as %q", token, name, got, decoded)
			}
		}
	}
}

// BenchmarkTheRelaysFirstReadOfAFrame times readRoleFrame, the read the session relay makes of
// every frame before it reads a claim (takeOverFrame), of a frame that is no role message with 8,
// 1,000 and 100,000 distinct short keys (manyKeysFrame).
func BenchmarkTheRelaysFirstReadOfAFrame(b *testing.B) {
	for _, keys := range []int{8, 1_000, 100_000} {
		data := manyKeysFrame(contracts.AgentTopicPrefix+"ses_bench", keys)
		b.Run(fmt.Sprintf("%d keys, %d bytes", keys, len(data)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := readRoleFrame(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
