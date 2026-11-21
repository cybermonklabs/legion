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
  "agent-name.stub.e2e.ts › a claimed List row and Board card open their issue by keyboard, and the claim's agent link opens the agent @pr":
    'the List\'s row is read inside the issues table this branch ships (`getByRole("table", { name: "Project issues table" })`), since `[data-issue-row]` alone now also matches the Board\'s own rows; every key, link and address it asserts is main\'s',
  "keyboard-inbox.stub.e2e.ts › inbox selection › a bulk snooze refused two different ways names the first reason and says so @pr":
    "the bulk Snooze is a menu now (dispatch://INFRA-2160), so the row picks Tomorrow from that menu where main chose it in a select; the two refusals and the bar's text it asserts are main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › keys still work with a pointer-ticked checkbox holding focus @pr":
    "the bulk Snooze is a menu now (dispatch://INFRA-2160), so after each `h` the row reads the menu's first choice focused where main read the bulk picker focused; the ticks, the `j` and the row it selects or focuses are main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › Escape leaves the bulk picker for the row it was opened from, keeping the marks @pr":
    "`h` opens a Snooze menu now (dispatch://INFRA-2160), the row's and the bar's, so the row reads each menu's first choice focused where main read the picker focused. The Escape that hands the focus back to the row each was opened from, the mark it keeps, and the two Escapes that leave the row and clear the marks are main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › a mark made under a wider filter neither counts nor snoozes once its row is out of the list @pr":
    "`h` opens the row's Snooze menu now (dispatch://INFRA-2160), so the row reads that menu's first choice focused where main read the row's picker focused; the narrowing, the counts and the write that is not made are main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › Escape dismisses a refusal the reader has already unticked @pr":
    "the bulk Snooze is a menu now (dispatch://INFRA-2160), so the row picks Tomorrow from that menu where main chose it in a select; the refusal, the unticks and the Escape that dismisses it are main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › h does not pull the reader out of an ask they are answering @pr":
    "the bulk Snooze is a menu now (dispatch://INFRA-2160), so the row asserts that `h` opens no bulk Snooze menu where main asserted that the bulk picker did not take the focus; the radio that keeps the focus is main's",
  "keyboard-inbox.stub.e2e.ts › inbox selection › Escape from a pointer-opened bulk picker goes back to its Snooze, not a stale row @pr":
    'renamed from "Escape from a pointer-opened bulk picker goes to the list, not a stale row": the bulk Snooze is a menu now (dispatch://INFRA-2160), which the row opens with a click where main focused the picker, and Escape from its first choice hands the focus back to the Snooze. So the row asserts the Snooze focused, where main asserted the third row focused. The `h` from the first row and the marks before it are main\'s',
  "hover-preview.stub.e2e.ts › a card survives a held-button drag that leaves its reference and comes back @pr":
    "the Conversation tab keeps each `New` mark it drew for the whole visit now (`visitBoundary`, dispatch://INFRA-2160), so before the gesture the row waits for the dwell's read mark to be written (`PUT /api/v1/me/issues/{key}/state`) and reads the turn's `New` still drawn (a count of 1), where main waited for the dwell to drop the mark (a count of 0); the drag, its path and the card it keeps open are main's",
  "hover-preview.stub.e2e.ts › a card survives a held-button move from the card back onto its reference @pr":
    "the Conversation tab keeps each `New` mark it drew for the whole visit now (`visitBoundary`, dispatch://INFRA-2160), so before the gesture the row waits for the dwell's read mark to be written (`PUT /api/v1/me/issues/{key}/state`) and reads the turn's `New` still drawn (a count of 1), where main waited for the dwell to drop the mark (a count of 0); the move from the card back onto the reference and the card it keeps open are main's",
  "keyboard-issue.stub.e2e.ts › j and k cross a collapsed band instead of dead-ending in it, and reach its rows once it opens @pr":
    "the bands are the table's status groups: the row opens `?group=status&sort=key&nofilter=1`, collapses Todo through its heading button rather than a `details` summary, and walks the same four rows in the same order with the same keys. What it asserts - that j steps over a collapsed group and k comes back, and that opening it puts its rows between the two they sit between - is main's",
  "keyboard-issue.stub.e2e.ts › the project List view roves with j/k and opens the focused row with Enter and o @pr":
    "the three rows are read inside the issues table rather than through the old `Todo issues` list, and the `?` help names the project scope's row actions after no list now (dispatch://INFRA-2160): it lists `Next row` and one `Open` row with its `o` where main read `Next issue` and `Open issue`. The roving, Enter and o are main's",
  "keyboard-issue.stub.e2e.ts › a closed issue greys the shortcuts its header disables, and keeps the ones it does not @pr":
    "the issue's status and priority are menus now (dispatch://INFRA-2160), so the row reads `Open the status menu` greyed where main read `Focus the status`, and reads `Open the priority menu` greyed as well; `t c` is one action of both issue pages now, so the row reads it enabled as `Conversation: open its tab (on the new page, every filter off)` where main read `Conversation tab`; the other four rows it reads are main's",
  "keyboard-issue.stub.e2e.ts › with every band collapsed j and k do nothing and ? greys them, and opening one restores both @pr":
    "the bands are the table's status groups, collapsed through their heading buttons, and the two shortcuts are `Next row` and `Previous row` where main named them `Next issue` and `Previous issue` (dispatch://INFRA-2160); what the row asserts - no rows, the keys doing nothing, the two shortcuts greyed, and both restored when one group opens - is main's",
  "keyboard-issue.stub.e2e.ts › the Board lists its own roving keys once: the List's are not registered while it shows @pr":
    "the List's roving keys are named `Next row`, `Previous row` and `Open the focused row` now (dispatch://INFRA-2160), so those are the three labels the row reads absent where main read `Next issue`, `Previous issue` and `Open the focused issue`; the Board's own `Open issue` and `Toggle List / Board` are main's",
  "keyboard-palette.stub.e2e.ts › a List row's own row runs on the row that opened the palette, and its movement keys are no rows @pr":
    "the row is read inside the issues table, and the project scope's row actions name no list now (dispatch://INFRA-2160): the row offers and clicks `Open` with its `o` where main read `Open issue`, and reads `Next row`, `Previous row` and `Open the focused row` absent where main read the same three ending in `issue`. The issue the row opens is main's",
  "keyboard-palette.stub.e2e.ts › the Inbox offers the selection's snooze and Clear the selection, but not the key that marks a row @pr":
    "renamed from \"the Inbox offers the selection's snooze but not the keys that mark and clear it\": the bar's Clear selection is a row of the palette now (dispatch://INFRA-2160, R11), so off the row, with the mark made, the row reads it offered with its Esc where main read it absent. The bulk Snooze is a menu now too, so the snooze's row opens that menu on its first choice where main focused the picker. The snooze's row, the absent mark key and the Escape that leaves the row are main's",
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
