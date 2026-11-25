package bus

import (
	"testing"

	"github.com/nats-io/nats.go"
)

// consumerCreateSubject answers the subject nats.go@v1.55.0 sends a named consumer's create to
// (jsm.go:505-534) for every server version nats.go tells apart (serverMinVersion,
// nats.go:2788-2817): 2.9.0 and later, a prerelease of 2.9.0 among them, since nats.go reads only
// the leading numbers; an older server; a version missing its patch, which nats.go reads as
// -1.-1.-1; and the empty version a connection that is not connected reports. The rows on a 2.15
// server are also sent to a real one (TestAConsumerCreateTakesOnlyTheRefusalOfTheSubjectItSends);
// the rows of the other versions have no server here to send to, so they are read off jsm.go.
func TestAConsumerCreateSubjectIsTheOneNatsGoPicks(t *testing.T) {
	t.Parallel()
	const stream, name, filter = "S", "c", "events.x"
	durable := nats.ConsumerConfig{Durable: name}
	durableFiltered := nats.ConsumerConfig{Durable: name, FilterSubject: filter}
	durableEverySubject := nats.ConsumerConfig{Durable: name, FilterSubject: ">"}
	named := nats.ConsumerConfig{Name: name}
	namedFiltered := nats.ConsumerConfig{Name: name, FilterSubject: filter}
	for _, row := range []struct {
		version string
		cfg     nats.ConsumerConfig
		want    string
	}{
		{"2.15.1", durable, "$JS.API.CONSUMER.CREATE.S.c"},
		{"2.15.1", durableFiltered, "$JS.API.CONSUMER.CREATE.S.c.events.x"},
		{"2.15.1", durableEverySubject, "$JS.API.CONSUMER.CREATE.S.c"},
		{"2.15.1", named, "$JS.API.CONSUMER.CREATE.S.c"},
		{"2.15.1", namedFiltered, "$JS.API.CONSUMER.CREATE.S.c.events.x"},
		{"v2.9.0", durable, "$JS.API.CONSUMER.CREATE.S.c"},
		{"2.9.0-RC.1", durableFiltered, "$JS.API.CONSUMER.CREATE.S.c.events.x"},
		{"2.8.4", durable, "$JS.API.CONSUMER.DURABLE.CREATE.S.c"},
		{"2.8.4", durableFiltered, "$JS.API.CONSUMER.DURABLE.CREATE.S.c"},
		{"2.8.4", namedFiltered, "$JS.API.CONSUMER.CREATE.S"},
		{"2.9", durable, "$JS.API.CONSUMER.DURABLE.CREATE.S.c"},
		{"", durable, "$JS.API.CONSUMER.DURABLE.CREATE.S.c"},
		{"", named, "$JS.API.CONSUMER.CREATE.S"},
	} {
		if got := consumerCreateSubject(row.version, stream, &row.cfg); got != row.want {
			t.Errorf("the create of durable %q, name %q, filter %q on a server reporting %q goes to %s, want %s",
				row.cfg.Durable, row.cfg.Name, row.cfg.FilterSubject, row.version, got, row.want)
		}
	}
}
