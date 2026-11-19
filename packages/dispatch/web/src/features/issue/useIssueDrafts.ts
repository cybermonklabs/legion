import { useCallback, useEffect, useReducer, useRef, useState } from "react";

import type { Issue } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export type IssueUpdateInput = Partial<Pick<Issue, "parent" | "route" | "status" | "title">>;

export interface IssueUpdateMutation {
  mutate: (input: IssueUpdateInput) => void;
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

/** Whose value a field's draft is (decision 332): "issue", a value the issue gave the field;
 *  "typed", text the reader typed and has not ended since, or ended on a value the hook refused
 *  (an empty title, a closed issue); "ended", a choice the reader ended - queued, out, or refused
 *  by Dispatch and not yet sent. */
type Own = "issue" | "typed" | "ended";

/** One field's draft (decisions 315 and 332). */
interface FieldState {
  /** The issue's value as last read: a prop, or a write's landing. */
  server: string;
  /** The value of this field's write still out, or null. */
  out: string | null;
  /** The reader's text, raw. */
  draft: string;
  /** Whose the draft is. Only a draft that is the issue's follows the issue's value. */
  own: Own;
  /** The reader's last ended choice, normalized, until it is sent, discarded or replaced, or the
   *  reference becomes it; else null. */
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

const changed = (rules: FieldRules, state: FieldState): boolean =>
  rules.normalize(state.draft) !== reference(state);

/** Drops a queued value the reference has become: the issue holds it, or is about to, so there
 *  is nothing left to send. */
const dropQueuedReference = (state: FieldState): FieldState =>
  state.queued === reference(state) ? { ...state, queued: null } : state;

/** What an answer leaves of a field's draft: a queued value the reference has become is
 *  dropped, and an ended choice the issue now holds, with nothing out and nothing left to send,
 *  is the issue's value. */
function answered(rules: FieldRules, state: FieldState): FieldState {
  const next = dropQueuedReference(state);
  const held =
    next.own === "ended" &&
    next.out === null &&
    next.queued === null &&
    rules.normalize(next.draft) === next.server;
  return held ? { ...next, own: "issue" } : next;
}

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
 * guard lets a write go. `own` says whose the draft is (decision 332): the issue's, text the
 * reader typed and has not ended since, or a choice the reader ended.
 *
 * - `write(text)`, the reader's typing: the draft is the text, and typed; nothing else moves.
 *   Typing ends no edit, so it neither chooses a value nor withdraws one, even typed back to the
 *   value out or the issue's: the value queued stays the reader's last ended choice, and the end
 *   of the edit chooses again. Moved with each keystroke, the queued value went as the write
 *   ahead of it landed, half typed, and the title the reader had saved never went; withdrawn by
 *   a draft typed back, an undo queued under the issue's own value was lost to a keystroke and
 *   its deletion.
 * - `request`, the end of an edit, a blur, a switch's send: the reader's choice, which replaces
 *   any value queued. Refused for a closed issue or a value Dispatch would not take, it asks for
 *   nothing and keeps whose the draft is; an unchanged draft takes the reference and asks for
 *   nothing; a changed one is queued as its text normalized, an ended choice, which the hook's
 *   drain then sends.
 * - `discard`, Escape or Cancel: the draft takes the reference and asks for nothing. While a
 *   write is out that is the value out, the value the issue is about to hold (decision 316).
 * - A draft that `request` or `discard` puts back to the reference is the reader's ended choice
 *   while a write is out, since the value out is one, and the issue's when nothing is out.
 * - `sent(value)`, the guard let the write go: the value is out, and nothing is queued.
 * - `server(value)`, a prop change or a landing's answer: a draft that is the issue's takes the
 *   value while nothing is out. A typed or ended draft stays, whatever the value: judged against
 *   the issue's value now, a draft another reader set the issue to read as not changed, and,
 *   moved on to that reader's next title, it showed a title the reader never typed, which the end
 *   of the edit then did not send. While a value is out the reference stays that value.
 * - `settled(landed)`, this field's write ended: a landed write's draft that is still its value
 *   takes the value it landed with, and an ended choice is then the issue's; text the reader
 *   typed again after the write went stays typed, their open text. A refusal keeps the draft,
 *   and whose it is, which is then changed against the issue's value: the reader's unsent
 *   choice, which Retry and the end of the next edit send.
 *
 * After `server` and `settled` a queued value is kept: it was valid when the reader ended the
 * edit that chose it, and a new reference does not undo that choice. It is dropped only once the
 * new reference is that value, which leaves it nothing to send. An ended choice the issue then
 * holds, with nothing out and nothing queued, is the issue's value, and follows it from then on.
 */
function reduceField(rules: FieldRules, state: FieldState, event: FieldEvent): FieldState {
  switch (event.type) {
    case "write":
      return { ...state, draft: event.text, own: "typed" };
    case "request": {
      const outcome = requestOutcome(rules, state, event.closed);
      if (outcome === "queued") {
        return { ...state, own: "ended", queued: rules.normalize(state.draft) };
      }
      if (outcome === "refused") return { ...state, queued: null };
      return {
        ...state,
        draft: reference(state),
        own: state.out === null ? "issue" : "ended",
        queued: null,
      };
    }
    case "discard":
      return {
        ...state,
        draft: reference(state),
        own: state.out === null ? "issue" : "ended",
        queued: null,
      };
    case "sent":
      return { ...state, out: event.value, queued: null };
    case "server": {
      const follows = state.out === null && state.own === "issue";
      return answered(rules, {
        ...state,
        draft: follows ? event.value : state.draft,
        server: event.value,
      });
    }
    case "settled": {
      if (state.out === null) return state;
      const took = event.landed && rules.normalize(state.draft) === state.out;
      return answered(rules, {
        ...state,
        draft: took ? state.server : state.draft,
        out: null,
        own: took && state.own === "ended" ? "issue" : state.own,
      });
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
