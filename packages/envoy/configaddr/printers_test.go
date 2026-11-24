package configaddr_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/configaddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// held holds an Address in an unexported field, as a value a caller formats may.
type held struct {
	addr configaddr.Address
}

// TestEachPrinterOfAnAddressPrintsNoneOfItsCredentials: an address carrying the probe's user
// information, query value and fragment value prints none of them through any of its printers -
// String; Format under every verb R2 names, directly and in an unexported field of a value and a
// pointer to one; LogValue and a slog line that holds it; Call of a request URL built from it; and
// Error, Cause and Named of the error a request built from it returns, which net/http quotes the
// URL in whole. Captures configaddr.<printer>[/<form>].
func TestEachPrinterOfAnAddressPrintsNoneOfItsCredentials(t *testing.T) {
	addr := configaddr.New("ENVOY_URL", probe)
	callURL := addr.Raw() + "/v1/messages/send"
	refused := &url.Error{Op: "Post", URL: callURL, Err: syscall.ECONNREFUSED}
	unparsed := &url.Error{Op: "parse", URL: callURL, Err: url.EscapeError("%zz")}
	t.Run("String", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.String", addr.String())
	})
	t.Run("Format", func(t *testing.T) {
		for name, verb := range map[string]string{"s": "%s", "v": "%v", "plus-v": "%+v", "sharp-v": "%#v", "q": "%q"} {
			redacttest.RequireNone(t, "configaddr.Format/"+name, fmt.Sprintf(verb, addr))
			redacttest.RequireNone(t, "configaddr.Format/held-"+name, fmt.Sprintf(verb, held{addr}))
			redacttest.RequireNone(t, "configaddr.Format/held-pointer-"+name, fmt.Sprintf(verb, &held{addr}))
		}
	})
	t.Run("LogValue", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.LogValue", addr.LogValue().String())
		var logged bytes.Buffer
		slog.New(slog.NewTextHandler(&logged, nil)).Info("delivered", "listener", addr)
		redacttest.RequireNone(t, "configaddr.LogValue/slog", logged.String())
	})
	t.Run("Call", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.Call", addr.Call(callURL))
	})
	t.Run("Error", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.Error", addr.Error(refused).Error())
	})
	t.Run("Cause", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.Cause", addr.Cause(unparsed).Error())
	})
	t.Run("Named", func(t *testing.T) {
		redacttest.RequireNone(t, "configaddr.Named", addr.Named(refused, "send the message").Error())
	})
}
