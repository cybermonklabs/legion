/**
 * Part R's words for the new issue page: the node menu of Graph's focused row, its grey reasons,
 * the delete's count and confirm, and the line a refused write draws (INFRA-5705, decision 427).
 * `page-words-parts.ts` joins them into `page-words.ts`.
 *
 * This file follows part T's: it holds no table of its own and exports nothing but its default,
 * so `page-words.ts` stays as it is. Every word is a key of `add` under a table that
 * `page-words.ts` declares, and the code of this part reads it from that table
 * (`CONTROL_LABELS`, `ANSWER_LINES`).
 *
 * - `CONTROL_LABELS` holds what the page draws as a control on load or on the menu's opening: the
 *   menu's own name, its entries in the order R1 gives, the kinds Retype offers, and the short
 *   pieces of a count. A phrase of it ends with no full stop.
 * - `ANSWER_LINES` holds what the page says in answer to the reader's own act: each grey entry's
 *   reason, the delete's count lines and its confirm, each refused write's line and the line of a
 *   change that was not made. Each is one sentence at most. A phrase with a count or a code in
 *   it is made of the pieces, as part T's `page_row` and `page_rows` are: the code that draws it
 *   puts the count or the code between them.
 * - No phrase says "node" or "requirement": the page's word for a node is "row", and for a
 *   requirement "statement" (`page-words.test.ts`).
 * - A key of an entry that a refusal code names is `refused_` and the code in lower case.
 *
 * `add` holds plain strings alone: the join copies it with `structuredClone`.
 */
import type { PartWords } from "./page-words-parts";

export default {
  add: {
    CONTROL_LABELS: {
      /** The control of the focused row in Graph that opens the menu (R1). Lens is part W's act
       *  (`LENS_WORDS.act`), and the menu's last entry. */
      edit_menu: "Row menu",
      /** What the key table (`?`) says `m` does: it opens the focused row's menu (R10). */
      edit_menu_key: "Open the focused row's menu",
      /** The menu's entries, in R1's order. */
      edit_new_below: "New row below",
      edit_new_under: "New row under",
      edit_retype: "Retype",
      edit_indent: "Indent",
      edit_outdent: "Outdent",
      edit_move_up: "Move up",
      edit_move_down: "Move down",
      edit_delete: "Delete",
      /** The kinds Retype offers: each of the four that the editor's schema lets it make. */
      retype_criterion: "Criterion",
      retype_statement: "Statement",
      retype_check: "Check",
      retype_plan_task: "Plan task",
      /** What Delete says while the server counts what the delete takes, and the unit after the
       *  count it names: "Delete 1 row", "Delete 4 rows". */
      delete_counting: "counting",
      delete_row: "row",
      delete_rows: "rows",
      /** The confirm's control that sends the delete. */
      confirm_delete: "Delete",
      /** The title of the Deleted link that opens the version that last held a row (R9): the
       *  link's own words are `target_gone`. */
      last_held_title: "Opens the version that last held it",
      /** The kind of the container that a refused move or delete would leave empty, named in
       *  the line that links to its row (R8): "This would leave this container empty: list
       *  item, The first item of the list". */
      container_list_item: "list item",
      container_callout: "callout",
    },
    ANSWER_LINES: {
      /** Why an entry is grey, one line for each reason. The key is what `placement.ts` answers
       *  for the entry. */
      grey_kind:
        "Only a criterion, a statement, a check, a plan task or a paragraph is edited here.",
      grey_own_issue: "This row is edited on its own issue.",
      grey_closed: "The issue is closed.",
      grey_own_row: "This row moves from its own row.",
      grey_not_placed: "This row is not placed yet.",
      grey_below_document: "A new row cannot stand below a row of this document.",
      grey_no_rank: "This row has no place in the order yet.",
      grey_no_sibling_above: "No row above it to go under.",
      grey_directly_under_issue: "It is already directly under the issue.",
      grey_zoom_root: "It is already directly under the row this view is zoomed to.",
      grey_first: "It is already the first.",
      grey_last: "It is already the last.",
      grey_not_paragraph: "Only a paragraph takes a new kind.",
      /** Why New row below, New row under and Retype are grey on a row whose entry the row
       *  takes, where no editor of the issue's room is given to the menu (R3, R5): each writes
       *  through that editor alone, so the menu writes nothing for it. */
      grey_no_editor: "The editor is not available for this row.",
      grey_criterion_parent: "A criterion goes under a question, a comment or a message.",
      grey_view_criterion: "This issue's agent writes its spec, so it takes no criterion.",
      grey_statement_parent:
        "A statement goes under a criterion, a question, a comment or a message.",
      grey_check_parent: "A check goes under a statement.",
      grey_plan_task_parent: "A plan task goes under a statement.",
      /** The line of a new row that stands after the spec's blocks, not right below the row it
       *  was made from (R3), and of a block saved in the room that no read placed within 2
       *  minutes. */
      spec_band: "This row stands after the spec's blocks.",
      saved_not_placed: "The block is saved and not yet placed.",
      /** What the row says of the moves a refusal dropped (R4): the count goes before it, "1
       *  move was not sent", "2 moves were not sent". */
      dropped_move: "move was not sent",
      dropped_moves: "moves were not sent",
      /** The delete's lines: the count could not be read (a press on it reads it again), the
       *  confirm's title, the confirm's line before the count and its unit
       *  (`CONTROL_LABELS.delete_row`), and the line when what stands under the row moved. */
      delete_count_failed: "The count could not be read.",
      delete_confirm_title: "Delete this row and what is under it?",
      confirm_takes: "This delete takes",
      /** What the line of a refused container says before the container it names: its kind and
       *  its first line follow. */
      container_empties: "This would leave this container empty:",
      /** The line of a change that was not made: any other code, a server error and no answer.
       *  The code, where there is one, follows in brackets after `not_made_code`. A press on the
       *  line tries the act again. */
      not_made: "The change was not made",
      not_made_code: "code",
      /** One line for each refusal code of the two routes of a row, and `NODE_GONE`. */
      refused_node_gone: "This row is gone.",
      refused_not_found: "This row is gone, or you may not see it.",
      refused_precondition_required: "The page did not say which version of the row it read.",
      refused_precondition_failed: "The row moved since this page read it.",
      refused_move_input: "The move did not say where the row goes.",
      refused_invalid_pointer: "The row cannot go there.",
      refused_container_would_empty:
        "This would leave the row's container with no first paragraph.",
      refused_node_not_movable: "This kind of row does not move.",
      refused_document_writer_not_architect:
        "Only a person or an architect of this issue can change this.",
      refused_answered_ask_kept: "A person answered this question, so an agent cannot change it.",
      refused_parent_contended: "Another change was in the way, so nothing changed.",
      refused_parent_document_removable:
        "A row cannot go under a document that a removal could take.",
      refused_parent_not_reached: "The new parent is gone or cannot be read.",
      refused_node_not_deletable: "This kind of row cannot be deleted.",
      refused_delete_input: "The delete did not say how many rows it takes.",
      refused_node_holds_result: "A result is never deleted, so this row stays.",
      refused_state_not_permitted: "The state of this issue does not allow this.",
      refused_delete_count_moved: "What is under this row changed, so check the new count.",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
