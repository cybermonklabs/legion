package harness

import (
	"slices"
	"strconv"
	"strings"
)

// Sample is a stratified sample of a part's cases (score --sample): the strata are the case
// group and the conversation flag, and the draw takes whole split groups, so a score's interval
// is clustered over the same groups on a sample as on the whole part.
type Sample struct {
	// Size is the cases asked for, and Seed the seed of the draw.
	Size int    `json:"size"`
	Seed uint64 `json:"seed"`
	// Cases and SplitGroups are what the draw took.
	Cases       int `json:"cases"`
	SplitGroups int `json:"split_groups"`
	// Strata are each stratum's cases in the part, its share of the size, and the cases drawn.
	Strata []SampleStratum `json:"strata"`
}

// SampleStratum is one stratum of a sample.
type SampleStratum struct {
	Group        string `json:"group"`
	Conversation bool   `json:"conversation"`
	Cases        int    `json:"cases"`
	Target       int    `json:"target"`
	Drawn        int    `json:"drawn"`
}

type stratumKey struct {
	group        string
	conversation bool
}

// drawSample is about size cases of cases, by whole split groups: each stratum's target is its
// share of size, rounded, and at least one case of a stratum that has any; the split groups are
// taken in the order of a SHA-256 of the seed and the group's key, and a split group is taken
// when it brings the strata nearer their targets (the sum over the strata of the distance of
// each one's drawn cases from its target falls), so one large group does not swell the sample.
// A size of the part or more is the part. The sample is in id order.
func drawSample(cases []*Case, size int, seed uint64) ([]*Case, Sample) {
	s := Sample{Size: size, Seed: seed}
	counts := map[stratumKey]int{}
	bySplit := map[string][]*Case{}
	for _, c := range cases {
		counts[stratumKey{c.Group, c.Conversation}]++
		bySplit[c.SplitGroup] = append(bySplit[c.SplitGroup], c)
	}
	targets := map[stratumKey]int{}
	for k, n := range counts {
		t := int(float64(size)*float64(n)/float64(len(cases)) + 0.5)
		targets[k] = min(max(t, 1), n)
	}
	drawn := map[stratumKey]int{}
	var out []*Case
	if size >= len(cases) {
		out = cases
		for k, n := range counts {
			drawn[k] = n
		}
		s.SplitGroups = len(bySplit)
	} else {
		splits := make([]string, 0, len(bySplit))
		for g := range bySplit {
			splits = append(splits, g)
		}
		key := func(g string) string { return sha256Hex([]byte(strconv.FormatUint(seed, 10) + "/" + g)) }
		slices.SortFunc(splits, func(a, b string) int { return strings.Compare(key(a), key(b)) })
		for _, g := range splits {
			add := map[stratumKey]int{}
			for _, c := range bySplit[g] {
				add[stratumKey{c.Group, c.Conversation}]++
			}
			change := 0
			for k, n := range add {
				change += abs(drawn[k]+n-targets[k]) - abs(drawn[k]-targets[k])
			}
			if change >= 0 {
				continue
			}
			s.SplitGroups++
			for _, c := range bySplit[g] {
				drawn[stratumKey{c.Group, c.Conversation}]++
				out = append(out, c)
			}
		}
		slices.SortFunc(out, func(a, b *Case) int { return strings.Compare(a.ID, b.ID) })
	}
	s.Cases = len(out)
	for k, n := range counts {
		s.Strata = append(s.Strata, SampleStratum{Group: k.group, Conversation: k.conversation, Cases: n, Target: targets[k], Drawn: drawn[k]})
	}
	slices.SortFunc(s.Strata, func(a, b SampleStratum) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(strconv.FormatBool(a.Conversation), strconv.FormatBool(b.Conversation))
	})
	return out, s
}

func abs(n int) int { return max(n, -n) }
