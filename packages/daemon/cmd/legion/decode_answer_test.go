package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADaemonAnswerThatDoesNotDecodeNamesNoMemberItWrote: decodeAnswer's error names what it
// decodes into and never a member name or a value the answer wrote. An answer whose only fault is
// a member the type does not have, named by a made-up credential, is said to be of another
// release (unknown-member), decided by the answer decoding once that refusal is lifted and never
// by encoding/json's text, which quotes the member. An answer whose map holds a value of the wrong
// kind under a key that is the credential names the member by oauth2call.DecodeError's fixed text
// (map-key), where encoding/json's path to it, which decodeAnswer printed, holds the key: since Go
// 1.27 runs json v1 on v2, that path holds the answer's own map keys. The twin shows
// encoding/json's error holds the key. Captures legion.decode-answer/<case>.
func TestADaemonAnswerThatDoesNotDecodeNamesNoMemberItWrote(t *testing.T) {
	const credential = "c8q2v-not-a-secret-token-4h7m"
	type counts struct {
		Rounds map[string]int `json:"rounds"`
	}
	mapKey := []byte(`{"rounds":{"` + credential + `":"many"}}`)
	t.Run("map-key-twin", func(t *testing.T) {
		var into counts
		err := json.Unmarshal(mapKey, &into)
		if err == nil || !strings.Contains(err.Error(), credential) {
			t.Fatalf("encoding/json's error is %v; the row needs one that holds the key %q", err, credential)
		}
	})
	for _, c := range []struct {
		name, answer, want string
	}{
		{"unknown-member", `{"rounds":{},"` + credential + `":true}`, "it holds a member the main.counts this legion reads does not have"},
		{"map-key", string(mapKey), "a main.counts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var into counts
			err := decodeAnswer([]byte(c.answer), &into)
			if err == nil {
				t.Fatal("decodeAnswer took the answer")
			}
			oauth2calltest.RequireNoPiece(t, "legion.decode-answer/"+c.name, err.Error(), credential)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("decodeAnswer's error is %q, want it to hold %q", err, c.want)
			}
		})
	}
}
