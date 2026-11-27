package webhook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher publishes a webhook's envelopes, each within the context its handler gives it, which
// carries the request's own end and the route's budget for its caller
// (*bus.Client.PublishEachWithin, which also keeps the bus's own five-second bound). It calls
// published, when that is not nil, once for each topic it published the envelope on, with the
// stream's acknowledgement there (bus.StoredAck): whether the stream already held the envelope
// under its Nats-Msg-Id, and whether that answer is to the bus's resend after the connection
// closed under its first send, which may have stored that very copy. It reports the envelope's
// own topic, and any copy it publishes beside it (ghrename.Publisher's copy under a repository's
// former name). The GitHub route logs one `github published` line for each.
type Publisher interface {
	PublishEachWithin(ctx context.Context, item contracts.Envelope, published func(topic string, duplicate, resent bool)) error
}

// BodyTooLarge is the text every listener route answers, with 413, to a body past its limit: the
// limit in bytes and in MiB. Every limit is a whole number of MiB. The webhook routes answer it as
// text and the /v1 routes as their JSON error, so both name the limit in the same words.
func BodyTooLarge(limit int64) string {
	return fmt.Sprintf("body too large: the limit is %d bytes (%d MiB)", limit, limit>>20)
}

// deliveryFailed answers a delivery whose envelope the listener did not publish, and logs
// `<action> refused`, `<action> in process` or `<action> failed` (failureOutcome). Slack and Ghost
// Wispr deliveries log it on every attempt; a GitHub delivery logs it once (failedDeliveries).
func deliveryFailed(w http.ResponseWriter, action string, err error) {
	result, status, answer := failureOutcome(err)
	log.Print(failureLine(action, result, err))
	http.Error(w, answer, status)
}

// failureLine is the line a failed delivery logs, `<action> <result>: <error>`, whose `refused`
// and `failed` results the listener's `webhook-refused` and `publish-failure` alarms count. A
// GitHub delivery that fails again logs this same line behind its own `redelivery <delivery>: `
// prefix (*failedDeliveries.deliveryFailed), which no alarm counts, so both lines are written here.
func failureLine(action, result string, err error) string {
	return fmt.Sprintf("%s %s: %v", action, result, err)
}

// failureOutcome is how a delivery whose envelope, or CI observation, the listener did not publish
// is logged and answered. A refusal (bus.ErrRefused: too large to publish whole, or a subject NATS
// does not accept) is refused the same way on every redelivery while the server's limits stand,
// so it is a 422 `refused`, and its answer is the error, which says why; Dispatch's redelivery
// sweep asks for it again like any failed delivery. Any other failure may pass on a redelivery,
// so it is a 503 `failed`. A stream at its byte limit (bus.ErrStreamFull) is such a failure: it
// takes the redelivery once age has made room, and until then that is a 503 as well. The
// listener's `webhook-refused` and `publish-failure` alarms count these lines.
//
// A publish a clustered stream refused because an earlier publish under the same message id, an
// earlier delivery of the same event, is still in process (bus.ErrDuplicateInProcess) is no
// failure: that publish stores the one copy. It is logged `in process`, which neither alarm
// counts, and answered as a failure is, 503, since this request did not see the copy stored; a
// redelivery, where its sender makes one, is answered 200 with that copy as a duplicate.
func failureOutcome(err error) (result string, status int, answer string) {
	switch {
	case errors.Is(err, bus.ErrRefused):
		return "refused", http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, bus.ErrDuplicateInProcess):
		return "in process", http.StatusServiceUnavailable, "service unavailable"
	}
	return "failed", http.StatusServiceUnavailable, "service unavailable"
}
