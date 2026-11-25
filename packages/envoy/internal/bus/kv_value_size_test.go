package bus

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A KV write whose value the bus refuses for its size is refused the same way however often it is
// sent, so every write of a handle names it the ErrTooLarge it is: a value past the server's max
// payload, which nats.go refuses before sending it, and a value past the bucket's largest value,
// which the stream refuses. Each refusal names the value's size and the bound it is past. A value
// under both bounds is the control: the same calls store it.
func TestAKeyValueWriteOfAValueTooLargeForTheBusIsErrTooLarge(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const valueBound = 4096
	kv, err := EnsureKeyValue(t.Context(), client.Conn, js, &nats.KeyValueConfig{Bucket: "kv_value_size", Storage: nats.MemoryStorage, MaxValueSize: valueBound})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue("kv_value_size") })
	maxPayload := int(client.Conn.MaxPayload())

	writes := map[string]func(key string, value []byte) error{
		"Put":       func(key string, value []byte) error { _, err := kv.Put(key, value); return err },
		"PutString": func(key string, value []byte) error { _, err := kv.PutString(key, string(value)); return err },
		"Create":    func(key string, value []byte) error { _, err := kv.Create(key, value); return err },
		"Update": func(key string, value []byte) error {
			revision, err := kv.Put(key, []byte("v"))
			if err != nil {
				t.Fatalf("seed %s: %v", key, err)
			}
			_, err = kv.Update(key, value, revision)
			return err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			key := strings.ToLower(name)
			if err := write(key+"_small", make([]byte, valueBound/2)); err != nil {
				t.Fatalf("%s of a value under both bounds: %v, want it stored", name, err)
			}
			for _, refused := range []struct {
				what  string
				size  int
				bound int
			}{
				{"the bucket's largest value", valueBound + 1, valueBound},
				{"the server's max payload", maxPayload + 1, maxPayload},
			} {
				err := write(key+"_large", make([]byte, refused.size))
				if !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrRefused) {
					t.Fatalf("%s of a value of %d bytes, past %s: %v, want an ErrRefused naming ErrTooLarge", name, refused.size, refused.what, err)
				}
				if text := err.Error(); !strings.Contains(text, strconv.Itoa(refused.size)) || !strings.Contains(text, strconv.Itoa(refused.bound)) {
					t.Fatalf("%s's refusal %q names neither the value's %d bytes nor %s, %d", name, text, refused.size, refused.what, refused.bound)
				}
				t.Logf("%s past %s: %v", name, refused.what, err)
			}
		})
	}
}
