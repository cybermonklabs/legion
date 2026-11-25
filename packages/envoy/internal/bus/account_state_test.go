package bus

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountStateAnswer reads the server's answer by its JetStream code and its HTTP status, through
// either package's APIError and through any wrapping, and never by its text. An account at one of
// its JetStream limits, and a server that fails a request it took (a 5xx), is the account's or the
// server's state, which an operator or the server lifts with no change to the asker, so the
// listener's start tries it again. An account that requires a byte limit is that state only where
// the byte limit is the asker's code, a bucket's; at the notification stream ENVOY_STREAM_MAX_BYTES
// answers it. An account limit a setting answers (10120, the copies' tier; 10122, the stream's byte
// limit), the server's refusal of the configuration asked for (10052, a 500 that
// ENVOY_SESSION_MAX_AGE can make), a bad request, an answer ClusterStateAnswer reads, and an error
// that only reads like an account's limit are not that state: a start that retried them would wait
// on a configuration that never succeeds.
func TestAccountStateAnswerReadsTheAccountsLimitsAndTheServersFailures(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name            string
		err             error
		byteLimitIsCode bool
		code            int
		ok              bool
	}{
		{"legacy account resources exceeded", &nats.APIError{Code: 400, ErrorCode: 10002, Description: "resource limits exceeded for account"}, false, 10002, true},
		{"legacy maximum consumers, wrapped", fmt.Errorf("max_consumers 24000: %w", &nats.APIError{Code: 400, ErrorCode: 10026, Description: "maximum consumers limit reached"}), false, 10026, true},
		{"legacy maximum streams, wrapped", fmt.Errorf("KV_envoy_interests create: %w", &nats.APIError{Code: 400, ErrorCode: 10027, Description: "maximum number of streams reached"}), true, 10027, true},
		{"jetstream maximum streams", &jetstream.APIError{Code: 400, ErrorCode: 10027, Description: "maximum number of streams reached"}, true, 10027, true},
		{"legacy memory resources", &nats.APIError{Code: 500, ErrorCode: 10028, Description: "insufficient memory resources available"}, false, 10028, true},
		{"jetstream storage resources, wrapped", fmt.Errorf("open bucket: %w", &jetstream.APIError{Code: 500, ErrorCode: 10047, Description: "insufficient storage resources available"}), true, 10047, true},
		{"legacy max bytes required at a bucket", &nats.APIError{Code: 400, ErrorCode: 10113, Description: "account requires a stream config to have max bytes set"}, true, 10113, true},
		{"jetstream max bytes required at a bucket", &jetstream.APIError{Code: 400, ErrorCode: 10113}, true, 10113, true},
		{"legacy stream create failed, a 500", &nats.APIError{Code: 500, ErrorCode: 10049, Description: "stream create failed"}, false, 10049, true},
		{"jetstream not enabled for the account, a 503", &jetstream.APIError{Code: 503, ErrorCode: 10039}, false, 10039, true},
		{"legacy max bytes required at the stream", &nats.APIError{Code: 400, ErrorCode: 10113, Description: "account requires a stream config to have max bytes set"}, false, 0, false},
		{"legacy no tiered limit", &nats.APIError{Code: 400, ErrorCode: 10120, Description: "no JetStream default or applicable tiered limit present"}, false, 0, false},
		{"jetstream no tiered limit at a bucket", &jetstream.APIError{Code: 400, ErrorCode: 10120}, true, 0, false},
		{"legacy stream byte limit over the account's", fmt.Errorf("ENVOY_STREAM_MAX_BYTES=1073741824: %w", &nats.APIError{Code: 400, ErrorCode: 10122, Description: "stream max bytes exceeds account limit max stream bytes"}), false, 0, false},
		{"jetstream bad request", &jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeBadRequest, Description: "bad request"}, true, 0, false},
		{"legacy invalid configuration, a 500 a setting makes", fmt.Errorf("set KV_envoy_sessions max age: %w", &nats.APIError{Code: 500, ErrorCode: nats.JSStreamInvalidConfig, Description: "duplicates window can not be larger then max age"}), true, 0, false},
		{"legacy stream name in use", &nats.APIError{Code: 400, ErrorCode: nats.JSErrCodeStreamNameInUse}, true, 0, false},
		{"legacy no peers, the cluster's", &nats.APIError{Code: 500, ErrorCode: 10005, Description: "no suitable peers for placement"}, false, 0, false},
		{"jetstream not leader, the cluster's", &jetstream.APIError{Code: 500, ErrorCode: 10009}, true, 0, false},
		{"the text alone", errors.New("nats: maximum number of streams reached"), true, 0, false},
		{"no answer", nats.ErrTimeout, true, 0, false},
		{"nil", nil, true, 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			code, ok := AccountStateAnswer(row.err, row.byteLimitIsCode)
			if code != row.code || ok != row.ok {
				t.Fatalf("AccountStateAnswer(%v, %t) = (%d, %t), want (%d, %t)", row.err, row.byteLimitIsCode, code, ok, row.code, row.ok)
			}
		})
	}
}
