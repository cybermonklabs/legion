package delivery

import "testing"

// Each new merge reader must see the same complete metadata whether the preceding GitHub
// answers were stored by a writer or carried by a read-only request.
func TestMergeReadsOfRequestHeadsMatchStoredMergeMetadata(t *testing.T) {
	t.Parallel()
	merged := credited(head(5, sha("1")), foldIssue)
	merged.HeadRef, merged.BaseRef, merged.State = "legion/merge", "integration/x", "merged"
	merged.MergeCommitSHA, merged.MergedAt = sha("2"), read(1)
	merged.EventAt, merged.CheckedAt = read(3), read(10)

	laterWithoutMerge := merged
	laterWithoutMerge.EventAt, laterWithoutMerge.CheckedAt = read(4), read(11)
	laterWithoutMerge.MergeCommitSHA, laterWithoutMerge.MergedAt = "", nil

	unknownMerge := merged
	unknownMerge.MergeCommitSHA, unknownMerge.MergedAt = "", nil
	unknownMerge.EventAt = read(2)

	changedPlacement := merged
	changedPlacement.HeadRef = "legion/renamed"
	changedPlacement.MergeCommitSHA, changedPlacement.MergedAt = sha("3"), read(2)
	changedPlacement.CheckedAt = read(12)

	for _, row := range []struct {
		name string
		steps []foldStep
		answers int
	}{
		{"a later answer with no merge fields keeps the stored merge", []foldStep{
			delivered(merged), readThrough(laterWithoutMerge),
		}, 1},
		{"a refreshed legacy row gains its merge commit and time", []foldStep{
			delivered(unknownMerge), readThrough(merged),
		}, 1},
		{"changed merge metadata is not collapsed as an equal answer", []foldStep{
			readThrough(merged), readThrough(changedPlacement),
		}, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool, ctx, attributor, answers := foldSetup(t, row.steps)
			if got := len(answers.columns.repo); got != row.answers {
				t.Fatalf("request holds %d answers, want %d: merge metadata is part of answer identity", got, row.answers)
			}
			questions := foldQuestions(t, row.steps)
			if len(questions) == 0 {
				t.Fatal("no head questions were enumerated")
			}
			want := foldOracle(t, ctx, pool, attributor, row.steps, questions)
			for _, question := range questions {
				got, err := foldRead(ctx, pool, attributor, answers, question)
				if err != nil {
					t.Errorf("%s, the read-only read: %v", question, err)
					continue
				}
				if got != want[question.String()] {
					t.Errorf("%s: read-only metadata\n\t%s\nwant the actual writer's metadata\n\t%s", question, got, want[question.String()])
				}
			}
		})
	}
}
