/**
 * Part M's words for the new issue page. `page-words-parts.ts` joins them into `page-words.ts`.
 *
 * - `add` holds the part's new keys for the tables of `page-words.ts`. Key them by the table's
 *   name, and nest them as the table is: `add: { CONTROL_LABELS: { load_older: "Load older" } }`.
 * - A table of the part's own is a named export of this file. Make it with `freeze` from
 *   `./page-structure`. List it in `unasked` if the page draws it on load, or in `answers` if
 *   the page draws it only after an act of the reader. `page-words.ts` exports it again.
 * - Put a new key for a table of this file in this file, from any pull request.
 * - Import nothing from `page-words.ts`, not even a type. Import only the type `PartWords` from
 *   `./page-words-parts`, and values from `./page-structure`. A value import from
 *   `page-words.ts` or `./page-words-parts` makes an import cycle.
 */
import type { PartWords } from "./page-words-parts";

export default {
  add: {
    EMPTY_STATE_LINES: {
      /** What the list of who worked the issue, in Details' People section, says where nobody has
       *  (`IssueCollaboratorList.tsx`): the Collaborators tab's words. */
      no_collaborators: "No one has worked this issue yet.",
    },
    STATE_LINES: {
      /** What the list of who worked the issue says where Legion's record of its agents could not
       *  be read, above the agents it lists without it (`IssueCollaboratorList.tsx`): the
       *  Collaborators tab's words. */
      legion_record_unreadable: "Legion's record of this issue's agents could not be read.",
      /** What Details' People section says where the issue's subscribed agents could not be read,
       *  which it reads on every load for their count (`details-more.tsx`): today's issue
       *  header's words, with Retry beside them. */
      subscribers_unreadable: "Subscribed agents unavailable — Envoy listener unreachable.",
    },
    ANSWER_LINES: {
      /** What the history in Details says while the issue's log is read, which Details opening
       *  asks for, and when it could not be; and what its Show more says while the next page of
       *  the log, which its press reads, is out (`IssueHistory.tsx`): the Activity tab's words. */
      loading_history: "Loading activity…",
      history_unreadable: "Could not load the activity.",
      loading_older: "Loading…",
      /** What Details' list of the runs of the issue's steps says while its read, which Details
       *  opening asks for, is out, and when it could not be read (`DetailsSteps.tsx`). */
      loading_steps: "Loading steps…",
      steps_unreadable: "Could not load the steps.",
      /** What the list of who worked the issue says while its read, which the reader opened, is
       *  out, and when it failed (`IssueCollaboratorList.tsx`): the Collaborators tab's words. */
      loading_collaborators: "Loading collaborators…",
      collaborators_unreadable: "Couldn't load who worked this issue.",
      /** What Details' People section says when Dispatch refused the reader's Unsubscribe
       *  (`details-more.tsx`): today's issue header's words, with Retry beside them. */
      unsubscribe_failed: "Could not unsubscribe this agent.",
      /** What the facts of a question the reader is on say when the read of its edits and of who
       *  its answer reaches failed, with Retry beside it. That read is the question's own, which
       *  the facts send only while Details is open and only where the page holds no read of the
       *  issue's questions with their threads (`FocusedNodeFacts.tsx`). */
      ask_thread_unreadable: "Could not load this question's edits and who it reaches.",
    },
    CONTROL_LABELS: {
      /** The words of the list of who worked the issue, in Details' People section
       *  (`IssueCollaboratorList.tsx`): its three lists, of Legion's
       *  agents, of every other agent and of people, as the Collaborators tab drew them; how a
       *  person or an agent worked the issue, a reader's words for each of the server's `reasons`
       *  tokens, which that tab drew as they came; each Legion role's list, by the role's token; and
       *  what a Legion agent says of when it started and of the agent that replaced it. */
      collaborator_reasons: {
        answered: "answered",
        assignee: "assigned",
        claimed: "claimed it",
        posted: "wrote here",
      },
      collaborator_replaced_by: "replaced by",
      collaborator_roles: {
        architect: "architect",
        implementer: "implementer",
        merger: "merger",
        planner: "planner",
        reviewer: "reviewer",
        tester: "tester",
      },
      collaborator_started: "started",
      collaborators_legion: "Legion agents",
      collaborators_others: "Other agents",
      collaborators_people: "People",
      /** The history's list as a reader hears it named, the Activity tab's name for the same list
       *  (`IssueHistory.tsx`); the line it draws where the reader's read mark stood as it first
       *  drew in this visit; and what one author's uploads within a minute say before the files
       *  they open (`history-entries.tsx`'s `Entry`). */
      history_list: "Issue activity",
      history_unread: "New since you last read",
      history_uploaded: "uploaded",
      /** What the history heads today's entries and yesterday's with (`history-model.ts`), the
       *  Activity tab's words: an older day is headed by its date,
       *  written out in the reader's locale (`dayLabel`). */
      history_today: "Today",
      history_yesterday: "Yesterday",
      /** What a line of the history calls a block it names by a UUID that no node of the issue
       *  carries now, in place of that id and the word before it (`history-model.ts`): an id is no
       *  name, so the line says what the block is rather than which one. */
      history_gone_block: "a block that is gone",
      /** Details' list of the runs Legion made of the issue's steps, under the history
       *  (`DetailsSteps.tsx`), where today's page has a Steps tab: its heading, which its count of
       *  runs follows, "Steps (4)"; the heading of each task's runs, before the task's own name,
       *  "Task default"; the label of each field of a run, the tab's columns, and of the step a
       *  failed or stopped run sent its task back to; and how a run ended, or that it runs, in the
       *  tab's words. */
      steps: "Steps",
      steps_task: "Task",
      step_fields: {
        agent: "Agent",
        attempt: "Attempt",
        ended: "Ended",
        model: "Model",
        result: "Result",
        returned_to: "Returned to",
        role: "Role",
        started: "Started",
        step: "Step",
        type: "Type",
      },
      step_results: {
        failed: "failed",
        passed: "passed",
        running: "running",
        stopped: "stopped",
      },
      /** Where a step the plan gives a task stands before any run of it, after the step's place
       *  and name in Details' list of the task's planned steps (`DetailsSteps.tsx`); a step a run
       *  has reached stands where that run does, in `step_results`' words. The Steps tab's words. */
      step_not_started: "not started",
      /** Draws the next entries of the history under the last one drawn, which reads the next
       *  page of the issue's log first where every entry read is drawn; and the word after the
       *  count of the entries read and not drawn that it says in brackets, "Show more (25 left)"
       *  (`IssueHistory.tsx`). Every other list of Details draws its next 20 entries by the same
       *  words, with the count of its entries not drawn (`details-leaves.tsx`'s `useListPages`). */
      show_more: "Show more",
      show_more_left: "left",
      /** What the list of keys says of `t v` and `t o`, which open Details at the history and at
       *  the collaborators (`details-more.tsx`): each a whole phrase, so
       *  the list reads it as written. */
      open_history: "Open the issue's history in Details",
      open_collaborators: "Open the issue's collaborators in Details",
      /** What the facts of the row the reader is on call its kind, before the kind's own name,
       *  "Kind: Question" (`FocusedNodeFacts.tsx`). The issue's own facts name it by its kind
       *  before its title instead, "Issue: …". */
      node_kind: "Kind",
      /** What a question an agent session asked draws behind its asker's name, in the facts of
       *  the row the reader is on, once a press on that name opens them, as the ask card draws
       *  them (`FocusedNodeFacts.tsx`): the control that copies the session's ID, and the one
       *  that copies the title the session gave itself. The control that copies its tmux target
       *  draws the target itself. */
      session_id: "Session ID",
      session_title: "Session title",
      /** What the shell's indicator calls that Retry's write: the delivery sent to its agent again
       *  (`FocusedNodeFacts.tsx`). */
      retry_delivery: "Delivery to an agent again",
      /** What a handoff's opened fields call the fields every Legion handoff record carries: the
       *  four `legion handoff write` stamps, the commit a file-backed role's work was done against
       *  (`head`), and each entry of its `proof`, counted from 1 (`HistoryHandoff.tsx`). Any other
       *  field is called by the words its key spells. */
      handoff_fields: {
        completed: "Completed",
        head: "Commit",
        issue: "Issue",
        phase: "Phase",
        proof: "Proof",
        schemaVersion: "Format version",
      },
      /** Who a delivery of a mention or a message went to, where the delivery names no session: a
       *  role no session held, or no target at all. An address is no name, so the history's
       *  line and the focused row's delivery say what it is rather than where it was sent. */
      unnamed_recipient: "an agent",
      /** What the shell's indicator calls an Unsubscribe of Details' People section while its write
       *  is out, between the agent's name and the issue's key (`useIssueSubscribers.ts`): today's
       *  issue header's words. */
      unsubscribed_from: "unsubscribed from",
    },
  },
  unasked: [],
  answers: [],
} as const satisfies PartWords;
