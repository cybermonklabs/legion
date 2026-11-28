import type { QueryClient } from "@tanstack/react-query";
import { useEffect, useState, useSyncExternalStore } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { goingConversationsQuery } from "../../api/queries";
import type { GoingContextInput } from "../../api/types";
import { goingBounds } from "../going/going-store";
import { showNotice } from "../going/Notice";
import { STARTED_RUN, withRunFrame } from "../going/run-frames";
import {
  endWords,
  providerRefused,
  providerRefusedWords,
  type RunState,
  reasonCode,
} from "../going/run-states";
import { buildIssuePath } from "../refs/routes";

/**
 * Enter on the line "File as a new issue" (spec R9, design v3, state 13): a new-issue run of the
 * person's words (`POST /api/v1/going/runs` with `kind` `new_issue`, recording the line `file`),
 * which files them at once and waits for nobody. While it runs, the bar's first row reads "Filing
 * in DEMO…"; once its end names the issue it filed, the bar closes and the notice reads "Filed
 * DEMO-46: ‹its title› · Open · Undo" for `FILED_NOTICE_MS`, its Undo the run's own (R6). A run
 * that ends without an issue, or that never starts, stays in the bar as a failure in words, with
 * Try again.
 *
 * A filing belongs to the open of the bar it was made in (`useBarOpen`): that open alone draws
 * it, and its filing closes that open alone. The filing is held outside the bar, so it goes on
 * when the person closes the bar before it ends: its notice still comes, and one that filed
 * nothing says so in the notice, since no open of the bar draws it. The run is a run of History
 * like any other, so History lists it with its Undo.
 */

/** How long the notice of a filed issue shows. */
export const FILED_NOTICE_MS = 10_000;

/** Words that say a new issue stands at the top of its project, under no issue. */
const TOP_LEVEL = /\b(?:top[- ]?level|no parent|without (?:a |any )?parent)\b/iu;

/** An issue's key as words write one, in either case: `DEMO-12`, `demo-12`. */
const KEY_IN_WORDS = /\b[A-Za-z][A-Za-z0-9]*-\d+\b/gu;

/**
 * The issue a filing of `words` files its new issue under, as the new-issue run decides it
 * (`newIssuePrompt`, `going/agent/prompt.go`): the issue of the page the words are asked from
 * (`pageIssue`, "" for a page of none), unless the words say the issue is top-level or name
 * another parent, which the run keeps (R9). The line on top says it before Enter, so the person
 * sees the parent their filing takes. Words that name any other issue may name it as the parent,
 * so they answer none rather than a parent the run may not take; undefined is no parent the line
 * can name.
 */
export function filingParent(words: string, pageIssue: string): string | undefined {
  if (pageIssue === "" || TOP_LEVEL.test(words)) {
    return undefined;
  }
  const named = words.match(KEY_IN_WORDS) ?? [];
  return named.every((key) => key.toUpperCase() === pageIssue.toUpperCase())
    ? pageIssue
    : undefined;
}

/** One open of the bar, which each filing belongs to. */
export type BarOpen = symbol;

/** The filing the bar shows: out, or ended with nothing filed and why, in words. `open` is the
 *  open of the bar it was made in, `context` the chip's context it was filed with, which its Try
 *  again files with again, and `project` the project it files in, "" where the chip names none. */
export type Filing =
  | {
      readonly state: "filing";
      readonly open: BarOpen;
      readonly context: GoingContextInput;
      readonly words: string;
      readonly project: string;
    }
  | {
      readonly state: "failed";
      readonly open: BarOpen;
      readonly context: GoingContextInput;
      readonly words: string;
      readonly project: string;
      readonly reason: string;
    };

/** The filing the bar shows, and the filing that last ended filed, which closes the open of the
 *  bar it was made in. */
interface FilingStore {
  readonly filing: Filing | undefined;
  readonly filed: Filing | undefined;
}

let store: FilingStore = { filed: undefined, filing: undefined };
const listeners = new Set<() => void>();

/** The open of the bar on the screen; undefined while the bar is closed. */
let shownOpen: BarOpen | undefined;

function set(next: FilingStore): void {
  store = next;
  for (const listener of listeners) {
    listener();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function readStore(): FilingStore {
  return store;
}

/** The filing store, read by a component and drawn again on each change. */
export function useFiling(): FilingStore {
  return useSyncExternalStore(subscribe, readStore, readStore);
}

/**
 * This open of the bar, on the screen from its mount until the bar closes. The bar draws the
 * filing made in this open alone, so a filing of an earlier open never takes its first row; a
 * failed filing of this open goes as the bar closes, which the person saw and closed the bar on.
 */
export function useBarOpen(): BarOpen {
  const [open] = useState(() => Symbol("an open of the bar"));
  useEffect(() => {
    shownOpen = open;
    return () => {
      if (shownOpen === open) {
        shownOpen = undefined;
      }
      if (store.filing?.open === open && store.filing.state === "failed") {
        set({ ...store, filing: undefined });
      }
    };
  }, [open]);
  return open;
}

/** Lets the filing the bar shows go: a failure the person typed past. */
export function dropFiling(): void {
  if (store.filing !== undefined) {
    set({ ...store, filing: undefined });
  }
}

/** The near-copy gate's refusal of the create, as a new-issue run's reason carries it
 *  (`goingNewIssueEnd`, `api/going_new_issue.go`, after `issue_create.go`'s 409): the key and the
 *  title of the issue the new one may repeat. */
const NEAR_COPY = /^POSSIBLE_DUPLICATE: possible duplicate of (\S+): (.*)$/su;

/** A code a reason starts with: "NEW_ISSUE_NOT_FILED: ", "FORBIDDEN: ". */
const LEADING_CODE = /^[A-Z][A-Z0-9_]*: */u;

/** What a new-issue run that filed nothing says before its create's refusal, which the bar's own
 *  "Not filed" says already. */
const NOT_FILED_WORDS = /^the issue was not filed: */u;

/** The code of a new-issue run's reason that says its create filed nothing (`goingNewIssueEnd`);
 *  any other code is the run's own failure or stop, before or beside its create. */
const NOT_FILED_CODE = "NEW_ISSUE_NOT_FILED";

/** A create Dispatch answered with a status and no words of its own, such as a 503 from a proxy
 *  in front of it: "its create answered 503". */
const CREATE_ANSWERED = /^its create answered (\d+)$/u;

/** A create that never ran, because the run ended first: "its create is skipped and did not
 *  run". */
const CREATE_NOT_RUN = /^its create is \S+ and did not run$/u;

/** A create's status in words: Dispatch failing or out of reach, busy, or refusing it. */
function createStatusWords(status: number): string {
  if (status >= 500) return "Dispatch could not file it just now.";
  if (status === 429) return "Dispatch is busy.";
  return "Dispatch refused it.";
}

/** Why a new-issue run that ended `state` at `level` filed nothing, from its end's reason, in
 *  words the person reads after "Not filed": never a code, a status or a service's own words, nor
 *  "not filed" a second time, and the near copy by its key and title where the create was refused
 *  as one. A run that failed or stopped before its create says why as the bar's own run says it
 *  (`endWords`), as the short answer's failure does; a refusal of the model's provider offers no
 *  Continue here, where Try again is the one next step. */
function reasonWords(state: RunState, level: string, reason: string): string {
  if (reasonCode(reason) !== NOT_FILED_CODE) {
    if (providerRefused(reason)) return `${providerRefusedWords(reason)}.`;
    return endWords(state, reason, level, "")?.text ?? "Dispatch filed nothing.";
  }
  const why = reason.replace(LEADING_CODE, "").replace(NOT_FILED_WORDS, "").trim();
  const nearCopy = NEAR_COPY.exec(why);
  if (nearCopy !== null) {
    return `a near copy is filed already: ${nearCopy[1]}, ${nearCopy[2]}`;
  }
  const answered = CREATE_ANSWERED.exec(why);
  if (answered !== null) return createStatusWords(Number(answered[1]));
  if (CREATE_NOT_RUN.test(why)) return "Dispatch stopped before it filed the issue.";
  const words = why.replace(LEADING_CODE, "").trim();
  return words === "" ? "Dispatch filed nothing." : words;
}

/**
 * Files `words` as one new issue, with the chip's `context`, in `project` (the chip's, for the
 * bar's words), as a filing of `open`. It does nothing while another filing of that open is out;
 * a filing of an earlier open still out goes on, no longer the one the bar shows. The run's
 * stream is read to its end, whose `issue` names what was filed; History is read again once the
 * run ends.
 */
export async function fileLineWords({
  context,
  login,
  open,
  project,
  queryClient,
  words,
}: {
  readonly context: GoingContextInput;
  readonly login: string | undefined;
  readonly open: BarOpen;
  readonly project: string;
  readonly queryClient: QueryClient;
  readonly words: string;
}): Promise<void> {
  if (store.filing?.state === "filing" && store.filing.open === open) {
    return;
  }
  const filing: Filing = { context, open, project, state: "filing", words };
  set({ ...store, filing });
  // A filing that filed nothing says why in its open of the bar while that open is on the
  // screen, and in the notice once it is not; the store lets it go once no open draws it.
  const fail = (reason: string) => {
    if (shownOpen === open) {
      if (store.filing === filing) {
        set({ ...store, filing: { ...filing, reason, state: "failed" } });
      }
      return;
    }
    if (store.filing === filing) {
      set({ ...store, filing: undefined });
    }
    showNotice({ ms: FILED_NOTICE_MS, text: `Not filed: ${reason}` });
  };
  let frames = STARTED_RUN;
  try {
    const run = await api.createGoingRun(
      { context, kind: "new_issue", line: "file", words },
      goingBounds(login)
    );
    await api.streamGoingRunEvents(
      run.id,
      (name, data) => {
        frames = withRunFrame(frames, name, data);
      },
      new AbortController().signal
    );
    const end = frames.end;
    if (end?.state === "done" && end.issue !== "") {
      set({ filed: filing, filing: store.filing === filing ? undefined : store.filing });
      // The notice names what was filed: the issue's key at once, and its title, which the run's
      // model wrote and its end does not carry, once the notice has read it (review R4).
      showNotice({
        actions: [{ label: "Open", to: buildIssuePath({ key: end.issue, kind: "issue" }) }],
        issue: end.issue,
        ms: FILED_NOTICE_MS,
        text: `Filed ${end.issue}`,
        undo: { run: run.id, undone: `Undone · ${end.issue} closed` },
      });
      return;
    }
    fail(reasonWords(frames.state, run.level, end?.reason ?? ""));
  } catch (error) {
    fail(apiErrorMessage(error, "Dispatch could not be reached."));
  } finally {
    void queryClient.invalidateQueries({ queryKey: goingConversationsQuery().queryKey });
  }
}
