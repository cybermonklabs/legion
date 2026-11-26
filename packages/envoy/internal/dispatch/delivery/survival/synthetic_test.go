package survival_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// synthetic is a branch of n days built in memory, for the tests that need a long history and no
// git: one to four commits a day that insert, remove and replace lines of a few files and now and
// then make, rename or delete a file, most of them pull requests of two groups with a handful of
// issues, and about one in twenty dated before the commit before it. Its patches are the
// zero-context hunks git writes of those edits, kept consistent with each file's length, so the
// replay reads them as it reads git's. The same seed builds the same branch.
func synthetic(seed uint64, n int) replayed {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	lengths := map[string]int{}
	var live []string // the files of the tree, in the order they were made
	made := 0
	var out replayed
	newFile := func() string {
		made++
		return fmt.Sprintf("dir%d/file-%d.txt", made%3, made)
	}
	issues := []string{"", "KEY-1", "KEY-2", "KEY-3"}
	var last time.Time
	for d := range n {
		count := 1 + rng.IntN(4)
		var stamps []time.Time
		for range count {
			stamps = append(stamps, day(d).Add(time.Duration(rng.IntN(24*60))*time.Minute))
		}
		slices.SortFunc(stamps, func(a, b time.Time) int { return a.Compare(b) })
		for _, stamp := range stamps {
			if !last.IsZero() && rng.IntN(20) == 0 {
				stamp = last.Add(-time.Duration(1+rng.IntN(20*60)) * time.Minute)
			}
			last = stamp
			var files []survival.FileDiff
			touched := map[string]bool{}
			for range 1 + rng.IntN(3) {
				action := rng.IntN(20)
				switch {
				case len(live) < 3 || (action == 0 && len(live) < 12):
					path := newFile()
					size := rng.IntN(21)
					file := survival.FileDiff{NewPath: path, Status: survival.StatusAdded}
					if size > 0 {
						file.Hunks = []survival.Hunk{{OldStart: 1, NewStart: 1, Added: size}}
					}
					files = append(files, file)
					lengths[path] = size
					live = append(live, path)
					touched[path] = true
					continue
				}
				index := rng.IntN(len(live))
				path := live[index]
				if touched[path] {
					continue
				}
				touched[path] = true
				switch {
				case action == 1 && len(live) > 3:
					file := survival.FileDiff{OldPath: path, Status: survival.StatusRemoved}
					if lengths[path] > 0 {
						file.Hunks = []survival.Hunk{{OldStart: 1, Removed: lengths[path], NewStart: 1}}
					}
					files = append(files, file)
					delete(lengths, path)
					live = slices.Delete(live, index, index+1)
				case action == 2:
					renamed := newFile()
					file := survival.FileDiff{OldPath: path, NewPath: renamed, Status: survival.StatusRenamed}
					if rng.IntN(2) == 0 {
						file.Hunks, lengths[renamed] = edit(rng, lengths[path])
					} else {
						lengths[renamed] = lengths[path]
					}
					files = append(files, file)
					delete(lengths, path)
					live[index] = renamed
					touched[renamed] = true
				default:
					hunks, length := edit(rng, lengths[path])
					files = append(files, survival.FileDiff{OldPath: path, NewPath: path,
						Status: survival.StatusModified, Hunks: hunks})
					lengths[path] = length
				}
			}
			commit := survival.Commit{SHA: fmt.Sprintf("%040x", len(out.chain)+1), CommittedAt: stamp}
			if rng.IntN(5) < 3 {
				head, author := "agent/work", "agent-app[bot]"
				if rng.IntN(3) == 0 {
					head, author = "other/work", "someone"
				}
				commit.Change = pr(len(out.chain)+1, head, author, issues[rng.IntN(len(issues))])
			}
			out.chain = append(out.chain, commit)
			out.patches = append(out.patches, files)
		}
	}
	return out
}

// edit is one to three hunks of a file of length lines, apart from each other, each removing up to
// six lines and adding up to five, with the file's length after them.
func edit(rng *rand.Rand, length int) ([]survival.Hunk, int) {
	var hunks []survival.Hunk
	at, delta := 1, 0
	for range 1 + rng.IntN(3) {
		if at > length+1 {
			break
		}
		start := min(at+rng.IntN((length+1-at)/2+1), length+1)
		removed := 0
		if left := length - start + 1; left > 0 {
			removed = rng.IntN(min(left, 6) + 1)
		}
		added := rng.IntN(6)
		if removed == 0 && added == 0 {
			added = 1
		}
		hunks = append(hunks, survival.Hunk{OldStart: start, Removed: removed, NewStart: start + delta, Added: added})
		delta += added - removed
		at = start + removed + 1
	}
	return hunks, length + delta
}
