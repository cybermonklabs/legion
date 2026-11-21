package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Publisher publishes a webhook's envelopes, each within the context its handler gives it, which
// carries the request's own end and the route's budget for its caller (*bus.Client.PublishWithin,
// which also keeps the bus's own five-second bound).
type Publisher interface {
	PublishWithin(context.Context, contracts.Envelope) error
}

// readBody reads r's whole body for a route that checks its signature over the bytes it read. It
// reads the body through http.MaxBytesReader, so it reads no more than limit bytes of it. A body
// past limit is answered 413 naming the limit (refuseBodyTooLarge), and a read that failed any
// other way 400 "invalid body". It reports whether the handler goes on.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuseBodyTooLarge(w, limit)
			return nil, false
		}
		http.Error(w, "invalid body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// refuseBodyTooLarge answers a body past the route's limit: 413, with BodyTooLarge's text.
func refuseBodyTooLarge(w http.ResponseWriter, limit int64) {
	http.Error(w, BodyTooLarge(limit), http.StatusRequestEntityTooLarge)
}

// BodyTooLarge is the text every listener route answers, with 413, to a body past its limit: the
// limit in bytes and in MiB. Every limit is a whole number of MiB. The webhook routes answer it as
// text and the /v1 routes as their JSON error, so both name the limit in the same words.
func BodyTooLarge(limit int64) string {
	return fmt.Sprintf("body too large: the limit is %d bytes (%d MiB)", limit, limit>>20)
}

// deliveryFailed answers a delivery whose envelope the listener did not publish, and logs
// `<action> refused` or `<action> failed` (failureOutcome). Slack and Ghost Wispr deliveries log
// it on every attempt; a GitHub delivery logs it once (failedDeliveries).
func deliveryFailed(w http.ResponseWriter, action string, err error) {
	result, status, answer := failureOutcome(err)
	log.Print(failureLine(action, result, err))
	http.Error(w, answer, status)
}

// failureLine is the line a failed delivery logs, `<action> <result>: <error>`, which the
// listener's `webhook-refused` and `publish-failure` alarms count. A GitHub delivery that fails
// again logs this same line behind its own `redelivery <delivery>: ` prefix
// (*failedDeliveries.deliveryFailed), which no alarm counts, so both lines are written here.
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
func failureOutcome(err error) (result string, status int, answer string) {
	if errors.Is(err, bus.ErrRefused) {
		return "refused", http.StatusUnprocessableEntity, err.Error()
	}
	return "failed", http.StatusServiceUnavailable, "service unavailable"
}
