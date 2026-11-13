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
  "hover-preview.stub.e2e.ts › a dispatch:// reference in a comment on the issue page opens no card on hover, and keyboard focus previews the target issue; Escape closes it @pr":
    "renamed from \"hovering a dispatch:// reference in a comment previews the target issue; Escape closes it\": the issue page opens no card on hover (dispatch://INFRA-1196 R6), so the row asserts that a resting pointer opens none, and reads the card main's row read after the hover - its target, title, status, priority, label, link and `aria-describedby` - after keyboard focus instead; Escape, the scroll that keeps a focused card and the blur are main's",
  "hover-preview.stub.e2e.ts › a card survives a held-button drag that leaves its reference and comes back @pr":
    "moved off the issue page, which opens no card on hover (dispatch://INFRA-1196 R6): the reference is the project List's key link for the target, so the row waits on no conversation's read-mark dwell and its press lands beside that link rather than inside a turn's collapsed button. The gesture, the paused clock and what it asserts - the card still open after twice the close delay - are main's",
  "hover-preview.stub.e2e.ts › a card survives a held-button move from the card back onto its reference @pr":
    "moved off the issue page, which opens no card on hover (dispatch://INFRA-1196 R6): the reference is the project List's key link for the target, so the row waits on no conversation's read-mark dwell. The gesture from beside the link onto the card and back, the paused clock and what it asserts are main's",
  "agent-name.stub.e2e.ts › a claimed List row and Board card open their issue by keyboard, and the claim's agent link opens the agent @pr":
    'the List\'s row is read inside the issues table this branch ships (`getByRole("table", { name: "Project issues table" })`), since `[data-issue-row]` alone now also matches the Board\'s own rows; every key, link and address it asserts is main\'s',
  "keyboard-issue.stub.e2e.ts › j and k cross a collapsed band instead of dead-ending in it, and reach its rows once it opens @pr":
    "the bands are the table's status groups: the row opens `?group=status&sort=key&nofilter=1`, collapses Todo through its heading button rather than a `details` summary, and walks the same four rows in the same order with the same keys. What it asserts - that j steps over a collapsed group and k comes back, and that opening it puts its rows between the two they sit between - is main's",
  "keyboard-issue.stub.e2e.ts › the project List view roves with j/k and opens the focused row with Enter and o @pr":
    "the three rows are read inside the issues table rather than through the old `Todo issues` list; the roving, Enter, o and the shortcuts dialog are main's",
  "keyboard-issue.stub.e2e.ts › with every band collapsed j and k do nothing and ? greys them, and opening one restores both @pr":
    "the bands are the table's status groups, collapsed through their heading buttons; what the row asserts - no rows, the keys doing nothing, the two shortcuts greyed, and both restored when one group opens - is main's",
  "keyboard-palette.stub.e2e.ts › a List row's own row runs on the row that opened the palette, and its movement keys are no rows @pr":
    "the row is read inside the issues table; everything the palette does in it is main's",
  "polish.stub.e2e.ts › project filters stay collapsed until needed and margin asks use the compact composer @pr":
    "the project list half is the table: there is no collapsed filter strip to open, so the row asserts the table is drawn with no popover open, picks a label in the column filter and reads the trigger's own name. The board, inbox, margin and agents halves, and every screenshot, are main's",
  "project.stub.e2e.ts › a 1,000-character table search stays inside the page @phone @pr":
    'renamed from "a 1,000-character search filter\'s chip ends in an ellipsis inside its row": the table has no filter chip, so the claim the same 1,000 characters make is that the search box and the page stay inside the viewport at both widths',
  "project.stub.e2e.ts › a table keeps its key whole and uses its title cell without overflowing the phone @phone @pr":
    "renamed from \"a list row keeps its key whole and gives the title the row's width\": the row is a table row, so the title's room is its own cell rather than the row, and the row also asserts that the box scrolls sideways instead of the page",
  "project.stub.e2e.ts › clearing the table search keeps the next keystroke in the search box @pr":
    "renamed from \"clearing the issue search keeps the strip open and the next keystroke in the search box\": the table's toolbar holds the search box with no strip to keep open, and what is left - the address, the cleared value, focus and the next keystroke - is main's",
  "project.stub.e2e.ts › status groups stay expandable and the project key sits beside its name @pr":
    "renamed from \"status bands stay expandable...\": the bands are the table's groups, reached by letting Done through the Status filter and grouping by status, and collapsed through the heading button rather than a `details` summary. The header half - the key pill beside the name, the gap and the title attribute - is main's",
  "project.stub.e2e.ts › the project page fits a phone screen with touch-sized tabs, filters and document controls @phone @phone-only @pr":
    "the Issues tab draws the table rather than a collapsed filter strip, so the row waits for the table instead of opening the strip, and the Status option it measures carries its count in its name; the fit and every other touch target are main's",
  "keyboard-palette.stub.e2e.ts › g d opens the route's project Documents, from an issue key or a project path, and g p picks a project @pr":
    "changed in one assertion, and this is the one row of the move that changed at all: the count of rows the query leaves is the Projects group's rather than main's dialog-wide one. Measured in this issue's pod, the row runs in two modes and they differ: on replay the bar's search is aborted, because no recording answers `/api/v1/search`, and `Operations` is the only row; in a recording run against the real harness the bar also offers `Make a new issue from \"operations\"`. The Projects group holds one row in both. Everything else is main's, Enter included: `g d` reads the route's project before and after, the group is visible, and Enter on the highlighted row opens `/projects/OPS/issues`",
};
