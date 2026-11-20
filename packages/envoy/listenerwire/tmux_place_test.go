package listenerwire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The window's size travels with the place, from the session's client through the listener to
// Dispatch: a window whose one pane floats states no size in its layout, and the machine page
// draws it at these two numbers. This holds the wire type to the JSON a client sends, by the
// JSON alone and never by a Go field name, so a member dropped from TmuxPlace, or written under
// another name, fails here whether or not the OpenAPI document is checked.
func TestTmuxPlaceCarriesTheWindowSizeAcrossTheWire(t *testing.T) {
	for _, test := range []struct {
		name string
		sent string
	}{
		{
			name: "a client that reads the window's size",
			sent: `{"socket":"/tmp/tmux-1000/default","server_pid":"4242","session_name":"main",` +
				`"window_id":"@3","window_index":2,"window_name":"agents","pane_id":"%5","pane_index":1,` +
				`"layout":"{\"V\":2,\"L\":{\"t\":\"p\",\"w\":28,\"h\":8,\"x\":11,\"y\":6,\"i\":0,\"z\":0,\"I\":\"%5\"}}",` +
				`"window_width":80,"window_height":24,"zoomed":false}`,
		},
		{
			// A client older than the size sends neither, and the place passes on neither.
			name: "a client older than the size",
			sent: `{"socket":"/tmp/tmux-1000/default","server_pid":"4242","session_name":"main",` +
				`"window_id":"@3","window_index":2,"window_name":"agents","pane_id":"%5","pane_index":1,` +
				`"layout":"c3a1,80x24,0,0,5","zoomed":false}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := json.NewDecoder(bytes.NewReader([]byte(test.sent)))
			// A member the type does not hold is refused rather than read past.
			decoder.DisallowUnknownFields()
			var place TmuxPlace
			if err := decoder.Decode(&place); err != nil {
				t.Fatalf("decoding the place a client sends: %v", err)
			}
			passedOn, err := json.Marshal(place)
			if err != nil {
				t.Fatalf("encoding the place: %v", err)
			}
			var want, got map[string]any
			if err := json.Unmarshal([]byte(test.sent), &want); err != nil {
				t.Fatalf("reading the sent place: %v", err)
			}
			if err := json.Unmarshal(passedOn, &got); err != nil {
				t.Fatalf("reading the place passed on: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the place passed on is %s, want what the client sent, %s", passedOn, test.sent)
			}
		})
	}
}
