import type { ISSUE_STATUS_OPTIONS } from "@legion/contracts/dispatch-tools";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  ApiError,
  api,
  apiErrorMessage,
  REQUEST_DEADLINE_MS,
  statusChangeRequest,
} from "../../../api/client";
import { mergeIssue } from "../../../api/issue-cache";
import type {
  IssueDetails,
  IssueSummary,
  ParentStatusConflictIssue,
  StatusChangeAnswer,
  StatusChangeEntry,
  StatusChangeInput,
} from "../../../api/types";
import { newSendKey } from "../../../lib/send-key";
import { statusText } from "../../project/board-model";
import { SendStoppedError, stoppedWriteNote } from "../../writes/keyed-write";
import { useKeyedWrite } from "../../writes/useKeyedWrite";
import { signedInLogin, useWrites } from "../../writes/write-store";

/** The code of Dispatch's refusal of a write that would put a parent's status out of line with
 *  its children's (R24): a parent is done only when each child is done, and while a child is in
 *  work its parent is in work. */
const PARENT_STATUS_CONFLICT = "PARENT_STATUS_CONFLICT";

/** Whether `error` is that refusal, which names the issues that hold the write (`issues`). */
export function isParentStatusConflict(error: unknown): error is ApiError {
  return error instanceof ApiError && error.status === 409 && error.code === PARENT_STATUS_CONFLICT;
}

/** The options of `POST /api/v1/issues/{key}/status-change`, each the member of its body that
 *  names it: move the open issues under the issue with it, unlink its open children, or move its
 *  parents into work. */
export type ParentStatusOption = (typeof ISSUE_STATUS_OPTIONS)[number];

/** Whether the issue page knows `issue` has a parent or an open child: only then can the rule
 *  refuse a write of its status. */
export function issueHasParentOrOpenChild(
  issue: Pick<IssueDetails, "children" | "parent">
): boolean {
  return issue.parent !== null || (issue.children ?? []).some((child) => child.status !== "done");
}

/** The same for a board card, which reads its children as a count (`progress.children`). */
export function cardHasParentOrOpenChild(
  issue: Pick<IssueSummary, "parent" | "progress">
): boolean {
  return issue.parent !== null || cardHasOpenChild(issue);
}

/** Whether a card's count of its children (`progress.children`) shows one that is not done. A
 *  card with no count shows none. */
export function cardHasOpenChild(issue: Pick<IssueSummary, "progress">): boolean {
  const children = issue.progress.children;
  return children !== null && children.done < children.total;
}

/**
 * The dry run of a status change: what `input` would change, or the refusal it would get, with
 * nothing written. Dispatch answers it as the rule is on, whatever its setting, so a write the
 * rule would refuse is answered `409 PARENT_STATUS_CONFLICT` here. It goes under a key of its own,
 * as every `POST` does, and is held to the silence deadline of a read, since it is one.
 */
export function dryRunStatusChange(
  client: QueryClient,
  key: string,
  input: StatusChangeInput
): Promise<StatusChangeAnswer> {
  return api.send(statusChangeRequest(key, { ...input, dry_run: true }), {
    deadlineMs: REQUEST_DEADLINE_MS,
    key: newSendKey(),
    login: signedInLogin(client),
  });
}

/** The issue and the status picked for it. */
export interface StatusPick {
  readonly key: string;
  readonly status: string;
}

/** One status write a surface makes through the hook. */
export interface StatusMove extends StatusPick {
  /** Whether the page knows the issue has a parent or an open child
   *  (`issueHasParentOrOpenChild`, `cardHasParentOrOpenChild`): only then is the dry run sent
   *  before the write. */
  readonly related: boolean;
  /** The write the surface makes when the rule asks nothing: the issue's `PATCH`. Its refusal is
   *  the surface's to show, and a `409 PARENT_STATUS_CONFLICT` of it the surface hands to `ask`
   *  (an issue header's through `useIssueStatusMove`'s `onError`). */
  readonly patch: () => unknown;
}

/** How a move ended. */
export type StatusMoveEnd =
  /** It went to the surface's `patch`. */
  | { readonly kind: "patched" }
  /** The person chose an option, and Dispatch made that status change; `null` where it landed
   *  with no answer to give. */
  | { readonly kind: "changed"; readonly answer: StatusChangeAnswer | null }
  /** Nothing was written: Keep as it is, Cancel, Escape or a navigation, after any answer of the
   *  question; or the surface went away while the question was open, or before it could open. */
  | { readonly kind: "kept" }
  /** The dry run was refused for another reason than the rule, so nothing was written. */
  | { readonly kind: "refused"; readonly refusal: unknown }
  /** Another move of this hook was asking, so this one did nothing. */
  | { readonly kind: "busy" };

/** What one option's dry run answered, while the question is open. */
export type ParentStatusCheck =
  | { readonly state: "checking" }
  | { readonly state: "ready"; readonly changed: readonly StatusChangeEntry[] }
  | { readonly state: "refused"; readonly message: string };

/** One choice of the question: its option and what its dry run answered. */
export interface ParentStatusChoice {
  readonly option: ParentStatusOption;
  readonly check: ParentStatusCheck;
}

/** The question the page asks, as `ParentStatusDialog` draws it. */
export interface ParentStatusQuestion extends StatusPick {
  /** The issues that hold the write, as Dispatch named them. */
  readonly issues: readonly ParentStatusConflictIssue[];
  /** What an earlier answer of this question already changed: a write refused part way. */
  readonly changed: readonly StatusChangeEntry[];
  readonly choices: readonly ParentStatusChoice[];
  /** The option whose write is out. */
  readonly writing: ParentStatusOption | undefined;
  /** Why the chosen write was refused. The question then takes no other write. */
  readonly refusal: string | undefined;
  readonly choose: (option: ParentStatusOption) => void;
  /** Keep as it is, Cancel, Escape and a navigation: closes, and writes nothing. */
  readonly close: () => void;
}

/** What the hook gives its surface. */
export interface ParentStatusMove {
  /** Makes `move`: for an issue with a parent or an open child, the dry run first, then the
   *  surface's `patch` where the rule asks nothing, or the question where it would refuse. A move
   *  made while an earlier one is checking is shown as pending at once and waits for that check,
   *  so each move is made, in the order they came. */
  readonly move: (move: StatusMove) => Promise<StatusMoveEnd>;
  /** Asks the question for a write Dispatch refused `409 PARENT_STATUS_CONFLICT` (`conflict`),
   *  as a refused dry run asks it: the page took the issue for one the rule could not refuse. */
  readonly ask: (pick: StatusPick, conflict: ApiError) => Promise<StatusMoveEnd>;
  /** The status a move is checking, asking about or writing: shown as the issue's pending one. */
  readonly pending: StatusPick | undefined;
  /** Why the last move's dry run was refused, until the next move. */
  readonly refusal: unknown;
  readonly question: ParentStatusQuestion | undefined;
}

/** The options the question offers for the issues that hold a write: the children's two where a
 *  listed issue is a child, the parents' one where a listed issue is a parent. */
function choicesFor(issues: readonly ParentStatusConflictIssue[]): readonly ParentStatusOption[] {
  const child = issues.some((issue) => issue.relation === "child");
  const parent = issues.some((issue) => issue.relation === "parent");
  return [
    ...(child ? (["include_children", "unlink_children"] as const) : []),
    ...(parent ? (["include_parent"] as const) : []),
  ];
}

/** The route's body for `status` with `option` passed. */
export function optionInput(status: string, option: ParentStatusOption): StatusChangeInput {
  const input: StatusChangeInput = { status };
  input[option] = true;
  return input;
}

/** One status change the question writes: the issue and the route's body. */
interface StatusChangePress {
  readonly key: string;
  readonly input: StatusChangeInput;
}

/** The question while it is open. */
interface Asking {
  readonly pick: StatusPick;
  readonly issues: readonly ParentStatusConflictIssue[];
  readonly changed: readonly StatusChangeEntry[];
  readonly checks: Readonly<Partial<Record<ParentStatusOption, ParentStatusCheck>>>;
  readonly writing: ParentStatusOption | undefined;
  readonly refusal: string | undefined;
}

/** Reads again each issue `changed` names: what a status change wrote, or a refused one wrote
 *  before its refusal. */
function readChangedAgain(client: QueryClient, changed: readonly StatusChangeEntry[]): void {
  for (const entry of changed) {
    void client.invalidateQueries({ queryKey: ["issue", entry.key] });
  }
  if (changed.length > 0) void client.invalidateQueries({ queryKey: ["issues"] });
}

/** What a status change Dispatch made leaves in the page's reads: the named issue as Dispatch
 *  answered it, and each issue it changed and the issue lists read again. One that landed with no
 *  answer names no issue, so every issue read is read again. None of these reads is awaited: a
 *  board's list waits for its moves, and the move that made this change is one of them. */
function landStatusChange(client: QueryClient, answer: StatusChangeAnswer | null): void {
  if (answer === null) {
    void client.invalidateQueries({ queryKey: ["issue"] });
  } else {
    mergeIssue(client, answer.issue);
    for (const entry of answer.changed ?? []) {
      void client.invalidateQueries({ queryKey: ["issue", entry.key] });
    }
  }
  void client.invalidateQueries({ queryKey: ["issues"] });
}

/** What the question says of a chosen write that did not land, where the rule was not the
 *  reason. One the reader stopped may have been made, so the issues are read again. */
function writeRefusal(client: QueryClient, error: unknown): string {
  if (error instanceof SendStoppedError) {
    landStatusChange(client, null);
    return stoppedWriteNote("this status change", "made", {
      anew: "The page reads the issues again.",
    });
  }
  return apiErrorMessage(error, "Could not change the status.");
}

/**
 * The one way the page writes an issue's status (R24, D13). Dispatch keeps a parent's status in
 * line with its children's, so for an issue the page knows has a parent or an open child, a move
 * sends the route's dry run first. Where the rule asks nothing, the surface's own `PATCH` follows.
 * Where the rule would refuse it (`409 PARENT_STATUS_CONFLICT`), the hook asks the person what to
 * do: it lists the issues that hold the move, sends the dry run of each choice the listed issues
 * allow, and lists what each choice changes. A choice confirmed is one status change with that
 * option, written through the write store; once it lands, each issue it changed and the issue
 * lists are read again. Keep as it is, Cancel, Escape and a navigation write nothing, and so does
 * the surface going away while it asks: a question it leaves open ends as kept, and one that
 * would open after it has gone ends so at once, since nobody is left to answer. A move that needs
 * no answer still makes its write once the surface has gone: its dry run runs, and where the rule
 * asks nothing the surface's `PATCH` goes. A `PATCH` the surface made for an issue it knew no
 * parent or open child of, refused by the rule all the same, opens the same question (`ask`).
 */
export function useParentStatusMove(): ParentStatusMove {
  const client = useQueryClient();
  const writes = useWrites();
  const [pending, setPending] = useState<StatusPick>();
  const [refusal, setRefusal] = useState<unknown>();
  const [asking, setAsking] = useState<Asking>();
  /** Whether a move is checking or asking: current in the task that starts one, so a second
   *  press in that task waits for the check (`checking`) or, while the question is open, does
   *  nothing. */
  const busy = useRef(false);
  /** The dry run of the move that is checking, which settles once that move has made its write
   *  or opened the question: a move made meanwhile waits for it. */
  const checking = useRef<Promise<void> | undefined>(undefined);
  /** Ends the move the question is open for. */
  const settle = useRef<((end: StatusMoveEnd) => void) | undefined>(undefined);
  /** Which round of the choices' dry runs is the question's now: an answer of an earlier round,
   *  or one that comes once the question has closed, is not read. */
  const round = useRef(0);
  /** Whether the surface is still drawn: once it has gone, no question opens, since nobody is
   *  left to answer it. */
  const live = useRef(true);
  const change = useKeyedWrite({
    inline: true,
    label: ({ input, key }: StatusChangePress) => `${key} set to ${statusText(input.status)}`,
    target: ({ key }) => `issue:${key}`,
    request: ({ input, key }) => statusChangeRequest(key, input),
  });
  const { mutateAsync: writeChange } = change;

  /** Sends the dry run of each choice `issues` allow, as a new round, and answers what each
   *  choice reads until its dry run answers. */
  const checkChoices = useCallback(
    (pick: StatusPick, issues: readonly ParentStatusConflictIssue[]): Asking["checks"] => {
      round.current += 1;
      const at = round.current;
      const options = choicesFor(issues);
      const checked = (option: ParentStatusOption, check: ParentStatusCheck) => {
        if (round.current !== at) return;
        setAsking((current) =>
          current === undefined
            ? current
            : { ...current, checks: { ...current.checks, [option]: check } }
        );
      };
      for (const option of options) {
        dryRunStatusChange(client, pick.key, optionInput(pick.status, option)).then(
          (answer) => checked(option, { changed: answer.changed ?? [], state: "ready" }),
          (error: unknown) =>
            checked(option, {
              message: apiErrorMessage(error, "Dispatch could not check this choice."),
              state: "refused",
            })
        );
      }
      return Object.fromEntries(options.map((option) => [option, { state: "checking" }]));
    },
    [client]
  );

  const open = useCallback(
    (pick: StatusPick, conflict: ApiError): Promise<StatusMoveEnd> => {
      // A question that would open once the surface has gone - its dry run, or a write the surface
      // made, answered after it - has nobody to ask: it ends at once as kept, and writes nothing.
      if (!live.current) {
        busy.current = false;
        return Promise.resolve({ kind: "kept" });
      }
      busy.current = true;
      const issues = conflict.issues ?? [];
      setPending(pick);
      setAsking({
        changed: [],
        checks: checkChoices(pick, issues),
        issues,
        pick,
        refusal: undefined,
        writing: undefined,
      });
      const { promise, resolve } = Promise.withResolvers<StatusMoveEnd>();
      settle.current = resolve;
      return promise;
    },
    [checkChoices]
  );

  const end = useCallback((result: StatusMoveEnd) => {
    round.current += 1;
    busy.current = false;
    setPending(undefined);
    setAsking(undefined);
    const resolve = settle.current;
    settle.current = undefined;
    resolve?.(result);
  }, []);
  // The surface going away - a navigation that unmounts it - ends an open question as Keep as it
  // is ends it, so its move ends, and with it a board's turn that waits for it: the board's later
  // moves and its list's reads do not wait on a question nobody can see. A choice's write already
  // out still lands, and its landing reads the issues again.
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
      if (settle.current !== undefined) end({ kind: "kept" });
    };
  }, [end]);

  const move = useCallback(
    async (next: StatusMove): Promise<StatusMoveEnd> => {
      const pick = { key: next.key, status: next.status };
      /** Takes `pick` off the pending status, unless a later move or the question shows its own. */
      const unshow = () => setPending((shown) => (shown === pick ? undefined : shown));
      if (checking.current !== undefined) {
        // A move made while an earlier one checks: the surface shows it at once, as it shows a
        // write waiting its turn, and it goes once that check has made its write.
        setPending(pick);
        while (checking.current !== undefined) await checking.current;
        unshow();
      }
      if (busy.current) return { kind: "busy" };
      setRefusal(undefined);
      if (!next.related) {
        await next.patch();
        return { kind: "patched" };
      }
      busy.current = true;
      setPending(pick);
      const checked = Promise.withResolvers<void>();
      checking.current = checked.promise;
      /** Lets a move that waits for this check go: called once this move has made its write or
       *  opened the question, so a later move's write never goes before it. */
      const release = () => {
        checking.current = undefined;
        checked.resolve();
      };
      try {
        await dryRunStatusChange(client, next.key, { status: next.status });
      } catch (error) {
        if (isParentStatusConflict(error)) {
          const asked = open(pick, error);
          release();
          return asked;
        }
        busy.current = false;
        unshow();
        setRefusal(error);
        release();
        return { kind: "refused", refusal: error };
      }
      // The rule asks nothing of this move as the issues stood at the dry run. The `PATCH` goes,
      // and Dispatch judges it as it stands then: a refusal of it asks (`ask`). It goes whether or
      // not the surface is still drawn: the person's pick needs no answer of theirs.
      busy.current = false;
      unshow();
      const patched = next.patch();
      release();
      await patched;
      return { kind: "patched" };
    },
    [client, open]
  );

  const ask = useCallback(
    (pick: StatusPick, conflict: ApiError): Promise<StatusMoveEnd> => {
      if (busy.current) return Promise.resolve({ kind: "busy" });
      // Once the surface has gone, nobody is left to ask: the refused write stays the shell's
      // indicator's to show.
      if (!live.current) return Promise.resolve({ kind: "kept" });
      // A refused write the surface left for the shell's indicator is this question's now: the
      // person's choice is a write of its own.
      for (const row of writes.rows()) {
        if (row.status === "refused" && row.refusal === conflict) writes.supersede(row.id);
      }
      setRefusal(undefined);
      return open({ key: pick.key, status: pick.status }, conflict);
    },
    [open, writes]
  );

  const choose = (option: ParentStatusOption) => {
    if (asking === undefined || asking.writing !== undefined || asking.refusal !== undefined)
      return;
    if (asking.checks[option]?.state !== "ready") return;
    const { pick } = asking;
    setAsking({ ...asking, writing: option });
    writeChange({ input: optionInput(pick.status, option), key: pick.key }).then(
      (answer) => {
        landStatusChange(client, answer);
        end({ answer, kind: "changed" });
      },
      (error: unknown) => {
        const changed = error instanceof ApiError ? (error.changed ?? []) : [];
        readChangedAgain(client, changed);
        if (isParentStatusConflict(error)) {
          // A step of the call was refused after the steps before it committed: the question
          // names what changed and the new conflict, and checks each choice again.
          const issues = error.issues ?? [];
          const checks = checkChoices(pick, issues);
          setAsking((current) =>
            current === undefined
              ? current
              : {
                  ...current,
                  changed: [...current.changed, ...changed],
                  checks,
                  issues,
                  writing: undefined,
                }
          );
          return;
        }
        const reason = writeRefusal(client, error);
        setAsking((current) =>
          current === undefined
            ? current
            : {
                ...current,
                changed: [...current.changed, ...changed],
                refusal: reason,
                writing: undefined,
              }
        );
      }
    );
  };

  const close = () => {
    if (asking?.writing !== undefined) return;
    end({ kind: "kept" });
  };

  const question: ParentStatusQuestion | undefined =
    asking === undefined
      ? undefined
      : {
          changed: asking.changed,
          choices: choicesFor(asking.issues).map((option) => ({
            check: asking.checks[option] ?? { state: "checking" },
            option,
          })),
          choose,
          close,
          issues: asking.issues,
          key: asking.pick.key,
          refusal: asking.refusal,
          status: asking.pick.status,
          writing: asking.writing,
        };
  return { ask, move, pending, question, refusal };
}
