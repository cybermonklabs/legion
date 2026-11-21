package texts

import (
	"encoding/json"
	"testing"
)

// R2 of dispatch://INFRA-3686: "The fields are those of the kind, the issue, the act, the actor,
// the time, the references ..., the versions and the state that the reader acts on that its event
// has." An update-stalled notice of a round this daemon opened names the drift by its items
// (images, settings) and holds no text of it: the daemon's updateStalledNotice
// (internal/daemon/podupdate.go) leaves Reasons empty for such a round. The notice is published as
// its JSON, which the card's encoder prints field by field, so a `reasons` key it holds is a
// `reasons: ""` line on the card. Its event has no reasons, so the notice holds no such key.
func TestAnUpdateStalledNoticeOfANewRoundStatesNoEmptyReasons(t *testing.T) {
	notice := UpdateStalledRecord{
		NoticeHead: NoticeHead{Kind: UpdateStalled, Issue: "LEGION-209"},
		Holds:      HoldsNoProcess,
		Update: &UpdateRound{Round: "round-2187820f-74bb-a446-9f66-3a37df0bfd29", Pod: "pod-b4442a92",
			Images: []string{"4c1b8e2a6f9d"}, Settings: []string{"account"},
			Waiting: []UpdateWaiting{{Claim: "legion-legion-209-implementer", Role: "implementer", State: "ready"}}},
	}
	raw, err := json.Marshal(notice)
	if err != nil {
		t.Fatalf("encode the update-stalled notice: %v", err)
	}
	var payload struct {
		Update map[string]json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("read the update-stalled notice %s: %v", raw, err)
	}
	if reasons, ok := payload.Update["reasons"]; ok {
		t.Errorf("the update-stalled notice of a round that recorded its drift as items holds reasons %s, which its card prints as a field: %s", reasons, raw)
	}
}
