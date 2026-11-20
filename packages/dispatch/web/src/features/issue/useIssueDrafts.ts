import { useCallback, useEffect, useReducer, useRef, useState } from "react";

import type { Issue } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export type IssueUpdateInput = Partial<Pick<Issue, "parent" | "route" | "status" | "title">>;

export interface IssueUpdateMutation {
  mutate: (input: IssueUpdateInput) => void;
  /** The write's Retry of its newest press (`useKeyedWrite`), under that press's key where the
   *  reader stopped it. */
  retry: () => void;
  variables: IssueUpdateInput | undefined;
}

export interface IssueDrafts {
  discardTitle: () => void;
  discardRoute: () => void;
  discardParent: () => void;
  onIssueSettled: (error: unknown) => void;
  onIssueSuccess: (next: Issue, input: IssueUpdateInput) => void;
  requestParentSubmit: () => boolean;
  requestRouteSubmit: () => boolean;
  requestStatusSubmit: (status: string) => boolean;
  /** Asks for the title's draft to be sent, answering whether it was sent or queued behind the
   *  write out. */
  requestTitleSubmit: () => boolean;
  retry: () => void;
  parent: string;
  parentDirty: boolean;
  route: string;
  routeDirty: boolean;
  routeIsValid: boolean;
  title: string;
  titleDirty: boolean;
  titleError: string | null;
  /** Whether a title the reader chose at the end of an edit waits to be sent. */
  titleQueued: boolean;
  writeParent: (next: string) => void;
  writeRoute: (next: string) => void;
  writeTitle: (next: string) => void;
}

const routePattern = /^(role:[a-z0-9-]+|session:[0-9a-f-]{16,})$/;

/** Whose value a field's draft is (decision 332). "issue": the issue's value, which the draft
 *  follows; nothing is out or queued while it is. "typed": text the reader typed that differs,
 *  as typed, from the value it was judged against at its last keystroke, or that was typed while
 *  a value was queued. A space typed before the next word makes the draft typed, though an end
 *  sends it trimmed: it is the reader's text, and handed back, the draft would take a rename
 *  arriving before the next keystroke. "ended": a choice the reader ended, normalized as the end
 *  sends it, while it is queued, out, refused by Dispatch and not sent again, or left unsent
 *  because the issue came to hold it; and, while a write is out, the value out, which a discard,
 *  an unchanged end or a keystroke back to it with nothing queued puts in the draft. A refused
 *  end (an empty title, a closed issue) keeps whichever the draft was. Of the answers (a
 *  landing, a refusal, another reader's value), only the settle of the field's own write hands a
 *  typed or ended draft back to the issue, never another reader's value; with nothing out, the
 *  reader's own keystroke back to the issue's value, exactly as typed, with nothing queued, an
 *  unchanged end and a discard do too. */
type Own = "issue" | "typed" | "ended";

/** One field's draft (decisions 315 and 332). */
interface FieldState {
  /** The issue's value as last read: a prop, or a write's landing. */
  server: string;
  /** The value of this field's write still out, or null. */
  out: string | null;
  /** The field's text: the reader's text as typed while it is theirs (`own`), and otherwise the
   *  value the last end, discard, landing or issue's value put there. */
  draft: string;
  /** Whose the draft is. Only a draft that is the issue's follows the issue's value. */
  own: Own;
  /** The reader's last ended choice, normalized, until it is sent, discarded or replaced, a
   *  refused end drops it, or the reference becomes it; else null. */
  queued: string | null;
}

/** What one field's draft is: the value a text sends, which values Dispatch takes, and the write
 *  that sends one. */
interface FieldRules {
  normalize: (text: string) => string;
  valid: (value: string) => boolean;
  wire: (value: string) => IssueUpdateInput;
}

type Field = "title" | "route" | "parent";

type FieldEvent =
  | { type: "write"; text: string }
  | { type: "request"; closed: boolean }
  | { type: "discard" }
  | { type: "sent"; value: string }
  | { type: "server"; value: string }
  | { type: "settled"; landed: boolean };

const fieldRules: Readonly<Record<Field, FieldRules>> = {
  title: {
    normalize: (text) => text.trim(),
    valid: (value) => value !== "",
    wire: (title) => ({ title }),
  },
  route: {
    normalize: (text) => text,
    valid: (value) => value === "" || routePattern.test(value),
    wire: (route) => ({ route }),
  },
  // An empty draft clears the parent: the wire form is null, never "".
  parent: {
    normalize: (text) => text.trim(),
    valid: () => true,
    wire: (parent) => ({ parent: parent === "" ? null : parent }),
  },
};
/** The three fields, in the order a drain sends them. */
const fields: readonly Field[] = ["title", "route", "parent"];

/** Until the write out lands the issue still holds the old value, so a draft is judged against
 *  the value out while there is one: a reader who types the old value back is undoing the write. */
const reference = (state: FieldState): string => state.out ?? state.server;

/** Whether the draft differs from the reference once normalized: whether an end has a value to
 *  send, whether the page marks the draft changed, and whether a settle hands the draft back. A
 *  keystroke, and a settle's check that its write landed with the draft, compare the draft as
 *  typed instead (`reduceField`). */
const changed = (rules: FieldRules, state: FieldState): boolean =>
  rules.normalize(state.draft) !== reference(state);

/** Drops a queued value the reference has become: the issue holds it, or is about to, so there
 *  is nothing left to send. */
const dropQueuedReference = (state: FieldState): FieldState =>
  state.queued === reference(state) ? { ...state, queued: null } : state;

/** Whose a draft put back to the reference, or typed back to it with nothing queued, is: the
 *  value out is the reader's ended choice, and with nothing out the reference is the issue's. */
const ownAtReference = (state: FieldState): Own => (state.out === null ? "issue" : "ended");

/** The draft put back to the reference, asking for nothing: an unchanged end, or a discard. */
const putBack = (state: FieldState): FieldState => ({
  ...state,
  draft: reference(state),
  own: ownAtReference(state),
  queued: null,
});

/** What the end of an edit does with a field's draft: queues it to be sent, finds nothing to
 *  send, or refuses it (a closed issue, or a value Dispatch would not take). */
type RequestOutcome = "queued" | "unchanged" | "refused";

function requestOutcome(rules: FieldRules, state: FieldState, closed: boolean): RequestOutcome {
  if (closed || !rules.valid(rules.normalize(state.draft))) return "refused";
  return changed(rules, state) ? "queued" : "unchanged";
}

/**
 * One field's draft, as one pure step per event. `reference` is the value out, else the
 * issue's, and a draft is `changed` when its text normalized differs from it. `queued` is the
 * reader's last ended choice not yet sent (decision 324), which the hook's drain sends once the
 * guard lets a write go. `own` says whose the draft is (decision 332).
 *
 * - `write(text)`, the reader's typing: the draft is the text. Typing ends no edit, so it neither
 *   chooses a value nor withdraws one, even typed back to the value out or the issue's: the value
 *   queued stays the reader's last ended choice, and the end of the edit chooses again. Moved
 *   with each keystroke, the queued value went as the write ahead of it landed, half typed, and
 *   the title the reader had saved never went; withdrawn by a draft typed back, an undo queued
 *   under the issue's own value was lost to a keystroke and its deletion.
 *   What a keystroke decides is whose the draft is. One that leaves the text, as typed, equal to
 *   the reference, with nothing queued, hands it back. With nothing out it is then the issue's,
 *   so a character typed and deleted leaves the draft following the issue, and the end of the
 *   edit sends no old title over the one another reader set since. With a write out it is then
 *   the value out, the reader's ended choice, as a discard or an unchanged end makes it, so a
 *   refusal of that write leaves the refused title in the field, and its landing makes the draft
 *   the issue's. With a value queued no keystroke hands the draft back: while a value waits to be
 *   sent the draft does not follow the issue's value (decision 324). Any other keystroke makes
 *   the draft typed.
 *   This comparison and the settle's check that its write landed with the draft take the text as
 *   typed; an end, a send and the drain take it normalized, so the end of the edit still sends a
 *   title trimmed. The reference is normalized, so a text with a space before or after it never
 *   equals it and stays typed: a space typed before the next word is the reader's text, and
 *   handed back, it gave way to a rename that arrived before the next keystroke, where "an editor
 *   the reader has typed in never changes under them" (decision 332). Dispatch's hook before
 *   this change compared the raw text here too.
 * - `request`, the end of an edit, a blur, a switch's send: the reader's choice, which replaces
 *   any value queued. Refused for a closed issue or a value Dispatch would not take, it asks for
 *   nothing, keeps whose the draft is, and drops the value queued: an end on an empty title is
 *   the reader's last act, and it cleared the title (decision 324). An unchanged draft takes the
 *   reference and asks for nothing; a changed one is queued as its text normalized, an ended
 *   choice, which the hook's drain then sends, and the draft takes that text too, as every end
 *   trims. The draft is then the value its write carries, so that write's settle hands it back
 *   as it lands, if the draft is still that value and nothing is queued then. So the field holds
 *   what the end sent: left as typed, an end on "a " sent "a" and left "a " in the field, a
 *   title the reader's record of their choice does not hold (the sequence test's I7 reads it at
 *   the next end).
 * - `discard`, Escape or Cancel: the draft takes the reference and asks for nothing. While a
 *   write is out that is the value out, the value the issue is about to hold (decision 316).
 * - A draft that `request` or `discard` puts back to the reference is the reader's ended choice
 *   while a write is out, since the value out is one, and the issue's when nothing is out.
 * - `sent(value)`, the guard let the write go: the value is out, and nothing is queued.
 * - `server(value)`, a prop change or a landing's answer: a draft that is the issue's takes the
 *   value while nothing is out. A typed or ended draft stays, whatever the value, and stays the
 *   reader's even when the value equals it, since no keystroke judged it: handed back then, it
 *   followed that reader's next title, which the reader never typed, and the end of the edit sent
 *   nothing. While a value is out the reference stays that value.
 * - `settled(landed)`, this field's write ended: a landed write's draft that is still its value,
 *   as typed, takes the value it landed with. A refusal keeps the draft, which is then judged
 *   against the issue's value, so Retry and the end of the next edit send it.
 *   Here, and on no `server` event, the draft is handed back to the issue: it is the issue's
 *   again once nothing is queued, it is the issue's value, and either the write landed
 *   with it or it was the reader's ended choice, as an undo to the issue's own value is once a
 *   refusal of the write ahead of it leaves the issue on that value. Otherwise it stays whose it
 *   was: text the reader typed on after the write went stays typed, their open text.
 *   The settle's check that its write landed with the draft compares it as typed, as the
 *   keystroke does: a space typed after the value while its write is out is the reader's text,
 *   which the write did not carry, so the draft keeps it and stays theirs as the write lands.
 *   Dispatch's hook before this change trims the draft as the write lands, which took the space
 *   out from under the reader and joined their next word to the title. In what a landing does to
 *   text typed while its write was out, this is the one place this change means to differ from
 *   that hook.
 *
 * After `server` and `settled` a queued value is kept: it was valid when the reader ended the
 * edit that chose it, and a new reference does not undo that choice. It is dropped only once the
 * new reference is that value, which leaves it nothing to send.
 */
function reduceField(rules: FieldRules, state: FieldState, event: FieldEvent): FieldState {
  switch (event.type) {
    case "write": {
      const back = state.queued === null && event.text === reference(state);
      return { ...state, draft: event.text, own: back ? ownAtReference(state) : "typed" };
    }
    case "request": {
      const outcome = requestOutcome(rules, state, event.closed);
      if (outcome === "queued") {
        const value = rules.normalize(state.draft);
        return { ...state, draft: value, own: "ended", queued: value };
      }
      if (outcome === "refused") return { ...state, queued: null };
      return putBack(state);
    }
    case "discard":
      return putBack(state);
    case "sent":
      return { ...state, out: event.value, queued: null };
    case "server": {
      const follows = state.out === null && state.own === "issue";
      return dropQueuedReference({
        ...state,
        draft: follows ? event.value : state.draft,
        server: event.value,
      });
    }
    case "settled": {
      if (state.out === null) return state;
      const took = event.landed && state.draft === state.out;
      const next = dropQueuedReference({
        ...state,
        draft: took ? state.server : state.draft,
        out: null,
      });
      const back = next.queued === null && !changed(rules, next) && (took || state.own === "ended");
      return back ? { ...next, own: "issue" } : next;
    }
  }
}

const fieldAt = (value: string): FieldState => ({
  server: value,
  out: null,
  draft: value,
  own: "issue",
  queued: null,
});

export function useIssueDrafts(issue: Issue, updateIssue: IssueUpdateMutation): IssueDrafts {
  // Read and written synchronously: a request and the drain it starts run in one task, and so do
  // a landing and its settle.
  const states = useRef<Record<Field, FieldState>>({
    title: fieldAt(issue.title),
    route: fieldAt(issue.route ?? ""),
    parent: fieldAt(issue.parent ?? ""),
  });
  const [, drawAgain] = useReducer((count: number) => count + 1, 0);
  const [titleError, setTitleError] = useState<string | null>(null);
  const updateIssueGuard = useSubmitGuard();

  const apply = useCallback((field: Field, event: FieldEvent) => {
    const state = states.current[field];
    const next = reduceField(fieldRules[field], state, event);
    if (
      next.server === state.server &&
      next.out === state.out &&
      next.draft === state.draft &&
      next.own === state.own &&
      next.queued === state.queued
    ) {
      return;
    }
    states.current[field] = next;
    drawAgain();
  }, []);

  // A value is marked out inside the guard, as its write goes: a write the guard holds back is
  // not out, and its value stays queued.
  function drainPendingSubmits(): void {
    for (const field of fields) {
      const value = states.current[field].queued;
      if (value === null) continue;
      updateIssueGuard.guard(() => {
        apply(field, { type: "sent", value });
        updateIssue.mutate(fieldRules[field].wire(value));
      });
      return;
    }
  }

  // The end of an edit: a changed draft is queued, and the drain sends it at once if the guard
  // lets a write go, else as the write out lands; after a refusal it waits for the next request.
  // A queued draft was sent or waits to be: what a caller needs is that it is asked for, not
  // which.
  function submit(field: Field): RequestOutcome {
    const closed = issue.closed_at !== null;
    const outcome = requestOutcome(fieldRules[field], states.current[field], closed);
    apply(field, { type: "request", closed });
    if (outcome === "queued") drainPendingSubmits();
    return outcome;
  }

  const requestTitleSubmit = () => {
    const outcome = submit("title");
    setTitleError(outcome === "refused" ? "Title is required." : null);
    return outcome === "queued";
  };
  const requestRouteSubmit = () => submit("route") === "queued";
  const requestParentSubmit = () => submit("parent") === "queued";

  // The field a write carried takes the value it carried: a write that landed with no answer is
  // passed the issue as it was, and a later read brings any value Dispatch normalized. The other
  // fields take the answer's: another field's write can land while this field's write waits
  // behind it in the issue's line (a title saved after a status pick), and while a value is out
  // a draft stays judged against it. An Escape or a Cancel while a write is out puts back the
  // value out (decision 316), the value the issue is about to hold, so the landing finds that
  // draft still the value out, and `settled` gives it the value the write landed with.
  const onIssueSuccess = (next: Issue, input: IssueUpdateInput) => {
    for (const field of fields) {
      const carried = input[field];
      const value = (carried === undefined ? next[field] : carried) ?? "";
      apply(field, { type: "server", value });
    }
  };
  // One write of these fields is out at a time (the guard), so one field at most has a value out,
  // and `settled` leaves the others as they are.
  const onIssueSettled = (error: unknown) => {
    for (const field of fields) apply(field, { type: "settled", landed: error === null });
    updateIssueGuard.release();
    if (error === null) {
      drainPendingSubmits();
    }
  };
  const retry = () => {
    const { variables } = updateIssue;
    if (variables?.title !== undefined) {
      requestTitleSubmit();
      drainPendingSubmits();
      return;
    }
    if (variables?.route !== undefined) {
      requestRouteSubmit();
      drainPendingSubmits();
      return;
    }
    if (variables?.parent !== undefined) {
      requestParentSubmit();
      drainPendingSubmits();
      return;
    }
    updateIssueGuard.retryLast(updateIssue);
  };
  const discardTitle = () => {
    apply("title", { type: "discard" });
    setTitleError(null);
  };
  const discardRoute = () => apply("route", { type: "discard" });
  const discardParent = () => apply("parent", { type: "discard" });
  const writeTitle = (next: string) => {
    apply("title", { type: "write", text: next });
    setTitleError(null);
  };
  const writeRoute = (next: string) => apply("route", { type: "write", text: next });
  const writeParent = (next: string) => apply("parent", { type: "write", text: next });
  const requestStatusSubmit = (status: string): boolean =>
    updateIssueGuard.guard(() => updateIssue.mutate({ status }));

  useEffect(() => {
    apply("title", { type: "server", value: issue.title });
  }, [apply, issue.title]);
  useEffect(() => {
    apply("route", { type: "server", value: issue.route ?? "" });
  }, [apply, issue.route]);
  useEffect(() => {
    apply("parent", { type: "server", value: issue.parent ?? "" });
  }, [apply, issue.parent]);

  const { title, route, parent } = states.current;
  return {
    discardTitle,
    discardRoute,
    discardParent,
    onIssueSettled,
    onIssueSuccess,
    requestParentSubmit,
    requestRouteSubmit,
    requestTitleSubmit,
    retry,
    parent: parent.draft,
    parentDirty: changed(fieldRules.parent, parent),
    route: route.draft,
    routeDirty: changed(fieldRules.route, route),
    routeIsValid: fieldRules.route.valid(route.draft),
    title: title.draft,
    requestStatusSubmit,
    titleDirty: changed(fieldRules.title, title),
    titleError,
    titleQueued: title.queued !== null,
    writeParent,
    writeRoute,
    writeTitle,
  };
}
