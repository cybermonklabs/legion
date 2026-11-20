package urlsetting

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/redact"
)

// Value is a URL-capable setting. It keeps the configured text behind a pointer so fmt cannot
// reveal it through reflection when it appears in an unexported enclosing field. Its output and
// diagnostic encodings are always the one cleaned form; Raw is for reviewed transport adapters.
type Value struct {
	raw *string
}

// New keeps raw as a URL-capable setting. Empty input becomes Value's zero value, so an unset and
// an explicitly empty setting keep the same behavior as an empty string.
func New(raw string) Value {
	if raw == "" {
		return Value{}
	}
	return Value{raw: &raw}
}

// Raw returns the configured text. It is for reviewed adapters that build a request or parse the
// configured value, never for diagnostics.
func (v Value) Raw() string {
	if v.raw == nil {
		return ""
	}
	return *v.raw
}

// Empty reports whether this setting is unset or explicitly empty.
func (v Value) Empty() bool { return v.raw == nil }

func (v Value) cleaned() string {
	if v.raw == nil {
		return ""
	}
	return redact.URL(*v.raw)
}

// String returns the only form of the setting that diagnostics may print.
func (v Value) String() string { return v.cleaned() }

// GoString returns the only form of the setting that diagnostics may print.
func (v Value) GoString() string { return v.cleaned() }

// Format prints the cleaned form under every fmt verb.
func (v Value) Format(state fmt.State, verb rune) {
	fmt.Fprintf(state, fmt.FormatString(state, verb), v.cleaned())
}

// LogValue returns the cleaned form for structured logs.
func (v Value) LogValue() slog.Value { return slog.StringValue(v.cleaned()) }

// MarshalText returns the cleaned form for diagnostic text encoders.
func (v Value) MarshalText() ([]byte, error) { return []byte(v.cleaned()), nil }

// MarshalJSON returns the cleaned form for diagnostic JSON encoders.
func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.cleaned()) }
