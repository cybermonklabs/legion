package api

func init() {
	registerEntryTestRows(entryTestRows{
		entry: treeInBacklogOrIceboxContract,
		retiredValues: []retiredEnumValue{{
			answer: AnswerState, keyPath: "admission.held.*.reason", value: "tree_parked",
		}},
	})
}
