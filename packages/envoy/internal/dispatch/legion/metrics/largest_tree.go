package metrics

// The largest tree of an answer (dispatch://INFRA-979): the tree root with the most finished
// issues, beside the rest, so a page can say when one tree fills the window (a standing
// reviewer that files one short child per pull request it reviews, say) and offer to leave it
// out (Population.ExcludeTrees).

// LargestTree is the tree with the most finished issues of the answer, its share, and its median
// work time against the rest's.
type LargestTree struct {
	Root            string  `json:"root"`
	Title           string  `json:"title"`
	Finished        int     `json:"finished"`
	MedianHours     float64 `json:"median_hours"`
	RestMedianHours float64 `json:"rest_median_hours"`
}

// largestTree is the answer's largest tree by finished issues, nil when nothing finished. A tie
// goes to the root first in key order, so one answer is the answer.
func (c *computation) largestTree(finished []*Issue) *LargestTree {
	counts := map[string]int{}
	var order []string
	for _, issue := range finished {
		root := c.treeRoot(issue.Key)
		if counts[root] == 0 {
			order = append(order, root)
		}
		counts[root]++
	}
	best := ""
	for _, root := range order {
		if counts[root] > counts[best] || (counts[root] == counts[best] && root < best) {
			best = root
		}
	}
	if best == "" {
		return nil
	}
	var in, rest []float64
	for _, issue := range finished {
		row, ok := c.workTimeOf(issue)
		if !ok {
			continue
		}
		if c.treeRoot(issue.Key) == best {
			in = append(in, row.Hours)
		} else {
			rest = append(rest, row.Hours)
		}
	}
	tree := &LargestTree{Root: best, Finished: counts[best], MedianHours: median(in), RestMedianHours: median(rest)}
	if issue := c.population.Issues[best]; issue != nil {
		tree.Title = issue.Title
	}
	return tree
}
