/** What a move changed in a row's assertions, for the rows where the usual sentence is false.
 *
 *  `e2e/row-map.md` says of every moved row that the move touched no line of what it asserts,
 *  and that sentence is what dispatch://INFRA-1407's k2 is read from: a reviewer reads the map
 *  rather than the diff, so a row the sentence covers is a row nobody looks at. A row whose
 *  assertions really did move therefore needs its own line, and this is where it is written.
 *
 *  A note is needed when a row's assertions are not the ones its pre-move copy made. The usual
 *  cause is a forward merge: `main` changes a row this layer has already moved, and the change
 *  cannot be copied across as it stands because the stubbed row runs against a recording and a
 *  scene the file shares. Nothing can decide that for you - the generation has no copy of the
 *  row before the move - so the note is a person's sentence, as `e2e/stub/real-stack-reasons.ts`
 *  is. What the generation does hold you to is the other direction: a note here for a row the
 *  listing does not name fails it, so a renamed or deleted row cannot leave a sentence behind
 *  that reads as current.
 *
 *  The key is the row's spec file and its title, as the listing gives them, joined by ` › `.
 *
 *  `unchangedByTheMove` is what the map says of every other moved row, and it lives here
 *  beside the exceptions so that one file holds both halves of the claim: the generation
 *  writes it, and `e2e/__tests__/row-map.test.ts` holds a row with a note to carrying its note
 *  and not that sentence. */
export const unchangedByTheMove =
  "unchanged - the move renames the file, lifts the shared seeding into the fixture and adds the lane tag to the title, and touches no line of what the row asserts";

/** The rows the sentence above is false for, keyed by the spec file and the title the listing
 *  gives, joined by ` › `. */
export const movedRowNotes: Record<string, string> = {
  "keyboard-palette.stub.e2e.ts › g d opens the route's project Documents, from an issue key or a project path, and g p picks a project @pr":
    "changed in one assertion, and this is the one row of the move that changed at all: the count of rows the query leaves is the Projects group's rather than main's dialog-wide one. Measured in this issue's pod, the row runs in two modes and they differ: on replay the bar's search is aborted, because no recording answers `/api/v1/search`, and `Operations` is the only row; in a recording run against the real harness the bar also offers `Make a new issue from \"operations\"`. The Projects group holds one row in both. Everything else is main's, Enter included: `g d` reads the route's project before and after, the group is visible, and Enter on the highlighted row opens `/projects/OPS/issues`",
};
