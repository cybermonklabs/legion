package harness

import (
	"fmt"
	"testing"
)

// sampleCases are 400 cases in 80 split groups of 1 to 9 cases, and one group of 120: two case
// groups, each inside and outside a conversation.
func sampleCases() []*Case {
	var out []*Case
	add := func(split string, n int, group string, conversation bool) {
		for i := range n {
			out = append(out, &Case{ID: fmt.Sprintf("%s/%s-%d", group, split, i), Group: group, SplitGroup: split, Conversation: conversation})
		}
	}
	for g := range 80 {
		add(fmt.Sprintf("T%02d", g), 1+g%9, []string{GroupQuestion, GroupAnswer}[g%2], g%3 == 0)
	}
	add("BIG", 120, GroupAnswer, true)
	return out
}

func TestTheSampleKeepsWholeSplitGroupsNearEachStratumsShare(t *testing.T) {
	cases := sampleCases()
	got, s := drawSample(cases, 100, 7)
	bySplit := map[string]int{}
	for _, c := range cases {
		bySplit[c.SplitGroup]++
	}
	drawn := map[string]int{}
	for _, c := range got {
		drawn[c.SplitGroup]++
	}
	for g, n := range drawn {
		if n != bySplit[g] {
			t.Errorf("split group %s: %d of its %d cases drawn", g, n, bySplit[g])
		}
	}
	if drawn["BIG"] > 0 {
		t.Error("the group of 120 swelled a sample of 100")
	}
	if s.Cases != len(got) || s.SplitGroups != len(drawn) || s.Cases < 80 || s.Cases > 120 {
		t.Errorf("the sample: %+v", s)
	}
	for _, st := range s.Strata {
		if d := st.Drawn - st.Target; d < -10 || d > 10 {
			t.Errorf("stratum %s/%t: drew %d for a target of %d", st.Group, st.Conversation, st.Drawn, st.Target)
		}
	}
	again, _ := drawSample(cases, 100, 7)
	other, _ := drawSample(cases, 100, 8)
	if fmt.Sprint(ids(again)) != fmt.Sprint(ids(got)) {
		t.Error("one seed drew two samples")
	}
	if fmt.Sprint(ids(other)) == fmt.Sprint(ids(got)) {
		t.Error("two seeds drew one sample")
	}
	if all, s := drawSample(cases, 1000, 7); len(all) != len(cases) || s.SplitGroups != len(bySplit) {
		t.Errorf("a sample larger than the part is the part: %d cases", len(all))
	}
}

func ids(cases []*Case) []string {
	out := make([]string, len(cases))
	for i, c := range cases {
		out[i] = c.ID
	}
	return out
}
