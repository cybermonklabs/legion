package urlsetting_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/urlsetting"
)

const guardedRedacted = "(not printed: the value carries a credential or is not a plain http(s) URL)"

// The guarded URL's user, password, query and fragment are made at each run, so no credential is
// written in this file, and guardedMarkers holds each of them.
var (
	guardedUser     = "guard-user-" + rand.Text()
	guardedPassword = "guard-" + rand.Text()
	guardedQuery    = "guard-query-" + rand.Text()
	guardedFragment = "guard-fragment-" + rand.Text()
	guardedRaw      = (&url.URL{Scheme: "https", User: url.UserPassword(guardedUser, guardedPassword), Host: "example.test", Path: "/path", RawQuery: guardedQuery, Fragment: guardedFragment}).String()
	guardedMarkers  = []string{guardedUser, guardedPassword, guardedQuery, guardedFragment}
)

type guardedHolder struct {
	value urlsetting.Value
}

type plainValue string

type plainHolder struct {
	value plainValue
}

func containsMarker(text string) string {
	for _, marker := range guardedMarkers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

func format(verb string, value any) string { return fmt.Sprintf(verb, value) }

func TestValuePrintsOnlyTheCleanedForm(t *testing.T) {
	value := urlsetting.New(guardedRaw)

	text, err := value.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		URL urlsetting.Value `json:"url"`
	}{URL: value})
	if err != nil {
		t.Fatal(err)
	}
	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Info("configured", "url", value)

	printed := map[string]string{
		"String":                value.String(),
		"GoString":              value.GoString(),
		"format %v":             format("%v", value),
		"format %+v":            format("%+v", value),
		"format %#v":            format("%#v", value),
		"format %s":             format("%s", value),
		"unexported holder %v":  format("%v", guardedHolder{value: value}),
		"unexported holder %+v": format("%+v", guardedHolder{value: value}),
		"unexported holder %#v": format("%#v", guardedHolder{value: value}),
		"unexported holder %s":  format("%s", guardedHolder{value: value}),
		"MarshalText":           string(text),
		"MarshalJSON":           string(encoded),
		"slog JSON handler":     logOutput.String(),
	}
	for name, output := range printed {
		if marker := containsMarker(output); marker != "" {
			t.Errorf("%s leaked %q in %q", name, marker, output)
		}
	}
	expected := map[string]string{
		"String":      guardedRedacted,
		"GoString":    guardedRedacted,
		"format %v":   guardedRedacted,
		"format %+v":  guardedRedacted,
		"format %#v":  `"(not printed: the value carries a credential or is not a plain http(s) URL)"`,
		"format %s":   guardedRedacted,
		"MarshalText": guardedRedacted,
	}
	for name, want := range expected {
		if got := printed[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var decoded struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.URL != guardedRedacted {
		t.Errorf("JSON URL = %q, want %q", decoded.URL, guardedRedacted)
	}
	var logged map[string]any
	if err := json.Unmarshal(logOutput.Bytes(), &logged); err != nil {
		t.Fatal(err)
	}
	if got, _ := logged["url"].(string); got != guardedRedacted {
		t.Errorf("slog URL = %q, want %q", got, guardedRedacted)
	}
}

func TestPlainStringControlsLeakThroughEveryOutput(t *testing.T) {
	plain := plainValue(guardedRaw)
	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Info("configured", "url", string(plain))
	encoded, err := json.Marshal(struct {
		URL plainValue `json:"url"`
	}{URL: plain})
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"format %v":             format("%v", plain),
		"format %+v":            format("%+v", plain),
		"format %#v":            format("%#v", plain),
		"format %s":             format("%s", plain),
		"unexported holder %v":  format("%v", plainHolder{value: plain}),
		"unexported holder %+v": format("%+v", plainHolder{value: plain}),
		"unexported holder %#v": format("%#v", plainHolder{value: plain}),
		"unexported holder %s":  format("%s", plainHolder{value: plain}),
		"MarshalText":           string(plain),
		"MarshalJSON":           string(encoded),
		"slog JSON handler":     logOutput.String(),
	}
	for name, output := range outputs {
		if marker := containsMarker(output); marker == "" {
			t.Errorf("control %s did not leak a marker in %q", name, output)
		}
	}
}

func TestValueKeepsUnsetAndEmptyValuesEmpty(t *testing.T) {
	for _, value := range []urlsetting.Value{{}, urlsetting.New("")} {
		if got := fmt.Sprintf("%s", value); got != "" {
			t.Errorf("empty value printed %q, want empty", got)
		}
		if !value.Empty() {
			t.Error("empty value did not report empty")
		}
		if got := value.Raw(); got != "" {
			t.Errorf("empty raw value = %q, want empty", got)
		}
	}
}
