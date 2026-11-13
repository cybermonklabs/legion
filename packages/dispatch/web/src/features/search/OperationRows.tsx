import { useQuery } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useCallback, useEffect } from "react";

import { goingSettingsQuery } from "../../api/queries";
import type {
  GoingDecisionEvent,
  GoingMissingArgument,
  GoingOffer,
  GoingProposalEvent,
  SearchResult,
} from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useWrites } from "../writes/write-store";
import {
  actionName,
  actionOn,
  type GoingApproval,
  runGoingWrite,
  runGoingWrites,
  shownValue,
  type Turn,
  type TurnLine,
  targetOf,
  undoGoingTurn,
  undoneText,
  useTurn,
} from "./going-turns";
import type { FileNewIssue } from "./NewIssueOffer";
import { fileNewIssue, newIssueFields } from "./new-issue";
import { barNoteClassName, type OfferRow, useWordsState } from "./offer";
import { handoffBody, type SendToAgent } from "./send-to-agent";
import { answerSettled, type GoingAnswer } from "./useGoingAnswer";

/** The group the bar's operation rows sit in, above the hits, and the operation row's own id. */
export const OPERATION_GROUP_ID = "search-group-going";
export const OPERATION_OPTION_ID = "search-option-going-run";

/** The id of the offer row of `action`, one of the two best below the action threshold. */
export function offerOptionId(action: string): string {
  return `search-option-going-offer-${action}`;
}

type Decision = GoingDecisionEvent["data"];
type Proposal = GoingProposalEvent["data"];

/** The two actions the bar sends through offers of its own, which always ask (`NewIssueOffer`,
 *  `SendToAgentOffer`), rather than through a row of the operation group. */
const OWN_OFFERS: Readonly<Record<string, true>> = {
  dispatch_issue: true,
  dispatch_send_to_agent: true,
};

/** The value of each invocation's missing argument the person gave, by invocation. */
type Fills = Readonly<Record<number, string>>;

/** What the person asked to run: the action the decision chose, `undefined` while the decision
 *  that names it has not arrived, and how the write came to run. */
interface Wanted {
  readonly action: string | undefined;
  readonly approved: GoingApproval;
  readonly fills: Fills;
}

/**
 * Where the bar's operation stands for the words in the box, once the person submitted it or
 * picked an offer: waiting for the decision or the proposal, asking for the one argument an
 * invocation is missing, asking first because the person's setting says so, an action the
 * proposal offered nothing to run for, or one whose every invocation names an item neither the
 * page nor the words named, which runs only when the person picks it (`picks`). Nothing submitted
 * is no state at all, and so is a submit that had nothing to run (`idle`); once a write has gone,
 * its lines say the rest.
 */
type Stage =
  | ({ readonly kind: "preparing"; readonly words: string } & Wanted)
  | ({ readonly kind: "filling"; readonly words: string; readonly action: string } & Omit<
      Wanted,
      "action"
    >)
  | {
      readonly kind: "confirming";
      readonly words: string;
      readonly action: string;
      readonly fills: Fills;
    }
  | { readonly kind: "unoffered"; readonly words: string; readonly action: string }
  | { readonly kind: "picks"; readonly words: string; readonly action: string }
  | { readonly kind: "idle"; readonly words: string };

/** What a submit or a pick comes to now: a stage to show, or the writes to make. */
type Step =
  | Stage
  | {
      readonly kind: "run";
      readonly action: string;
      readonly approved: GoingApproval;
      readonly fills: Fills;
    };

/** The offer of `action` the proposal offered something to run for. */
function offerOf(proposal: Proposal | undefined, action: string): GoingOffer | undefined {
  return proposal?.offers.find((offer) => offer.action === action && offer.offered);
}

/** The invocations of `offer` a write of the bar runs, by their index in it: every one where the
 *  person picked the action, and otherwise - a submit, and its confirmation - only those the
 *  proposal marked automatic. An invocation naming an item neither the page nor the person's
 *  words named came from text someone stored, and runs only when the person picks it. */
function runIndexes(offer: GoingOffer | undefined, approved: GoingApproval): number[] {
  return (offer?.invocations ?? []).flatMap((invocation, index) =>
    approved === "person" || invocation.automatic ? [index] : []
  );
}

/** `offer` with only the invocations at `indexes`, for a row that names their items alone. */
function offerWith(offer: GoingOffer, indexes: readonly number[]): GoingOffer {
  return { ...offer, invocations: offer.invocations.filter((_, index) => indexes.includes(index)) };
}

/** The value a fill sends for the argument it fills: a number field's as a number. */
function fillValue(missing: GoingMissingArgument, text: string): unknown {
  return missing.kind === "number" ? Number(text) : text;
}

/** The items an offer acts on, in the words of one row: `CORE-1, CORE-2 and CORE-3`. */
function targetsOf(offer: GoingOffer | undefined): string {
  const targets = [
    ...new Set(
      (offer?.invocations ?? []).map((invocation) =>
        targetOf(invocation.arguments, invocation.target_argument)
      )
    ),
  ].filter((target) => target !== "");
  if (targets.length <= 1) return targets[0] ?? "";
  return `${targets.slice(0, -1).join(", ")} and ${targets.at(-1)}`;
}

function rowLabel(action: string, offer: GoingOffer | undefined): string {
  const targets = targetsOf(offer);
  return `Run ${actionName(action)}${targets === "" ? "" : ` on ${targets}`}`;
}

/**
 * The step a submit (`wanted.action` undefined: the operation the decision chose at or above the
 * action threshold) or a pick comes to, from what the run has streamed so far. A run that has not
 * decided, or a decision whose proposal has not arrived, is `preparing`; an answer that ended
 * without either has nothing to run. A submit runs only the invocations the proposal marked
 * automatic (`runIndexes`), and one with none is `picks`: the person picks the rest.
 */
function nextStep(
  words: string,
  wanted: Wanted,
  answer: GoingAnswer | undefined,
  confirmWrites: boolean | undefined
): Step {
  if (answer === undefined || answer.runId === undefined || answer.decision === undefined) {
    return answer !== undefined && answerSettled(answer)
      ? { kind: "idle", words }
      : { ...wanted, kind: "preparing", words };
  }
  // The decision's choice at or above the action threshold, which Dispatch's `act` says, and
  // never one of the writes that always ask.
  const action =
    wanted.action ?? (answer.decision.act ? answer.decision.actions[0]?.action : undefined);
  if (action === undefined) return { kind: "idle", words };
  if (answer.proposal === undefined) {
    return answerSettled(answer)
      ? { action, kind: "unoffered", words }
      : { ...wanted, action, kind: "preparing", words };
  }
  const offer = offerOf(answer.proposal, action);
  if (offer === undefined) return { action, kind: "unoffered", words };
  const runs = runIndexes(offer, wanted.approved);
  if (runs.length === 0) return { action, kind: "picks", words };
  const unfilled = runs.some(
    (index) =>
      offer.invocations[index]?.missing !== null && (wanted.fills[index] ?? "").trim() === ""
  );
  if (unfilled)
    return { action, approved: wanted.approved, fills: wanted.fills, kind: "filling", words };
  if (wanted.approved === "auto") {
    if (confirmWrites === undefined) return { ...wanted, action, kind: "preparing", words };
    if (confirmWrites) return { action, fills: wanted.fills, kind: "confirming", words };
  }
  return { action, approved: wanted.approved, fills: wanted.fills, kind: "run" };
}

/** One row of the bar's operation group, as the list draws it: its label, its trailing control,
 *  and the row the arrows, Enter and a click press. */
export interface OperationOption {
  readonly hint: ReactNode;
  readonly label: string;
  readonly row: OfferRow;
}

/** The bar's operations for the words in the box. */
export interface GoingOperations {
  /** The group of rows above the hits: the operation with its Run button where the decision
   *  acts, or the two best actions to pick from below the threshold; none while there is none. */
  readonly section:
    | { readonly id: string; readonly label: string; readonly options: readonly OperationOption[] }
    | undefined;
  /** The bar's submit (`$mod+Enter`, the Run button, Enter on the operation row). */
  readonly submit: () => void;
  /** What the operation is doing and what it did: preparing, the missing argument, the confirm
   *  step, and one line per write with its undo. */
  readonly panel: ReactNode;
  /** How the new-issue offer files, where the decision chose a new issue: offered once the
   *  proposal offers it, its create a write of the run. Undefined leaves the offer as it is. */
  readonly newIssue:
    | { readonly offered: boolean; readonly file: FileNewIssue | undefined }
    | undefined;
  /** How the send-to-agent offer sends, where the proposal offers the send: a write of the run.
   *  Undefined leaves the offer as it is. */
  readonly sendToAgent: SendToAgent | undefined;
}

/**
 * The bar's operations, from the `decision` and `proposal` frames of the answer to the words in
 * the box. Where the decision acts - its choice at or above the action threshold and none of the
 * writes that always ask - the operation is one row above the hits with a Run button, and it runs
 * on submit: `$mod+Enter` anywhere in the bar, the Run button, or Enter on the row, which the
 * highlight reaches only when the person moves it there. A submit runs only the invocations the
 * proposal marked automatic - each item they name is the page's or named in the words - and the
 * rest, which only text someone stored asked for, are a row the person picks. Below the threshold
 * the two best actions are rows of their own, and neither runs until the person picks it. A new
 * issue and a send to the agent on the issue go through the bar's own two offers, which always
 * ask.
 *
 * A submit before the proposal arrives says it is preparing and runs when the proposal lands; one
 * with no run yet asks at once. An invocation missing one argument asks for it first, and with
 * the person's setting on (`GET /api/v1/me/going/settings`) every write asks first and is sent
 * confirmed. Each invocation is one write (`runGoingWrite`), and each write one line; a submit
 * pressed again, while the turn's writes are out or once they landed, writes none of them again,
 * and makes again only one Dispatch refused.
 */
export function useGoingOperations({
  answer,
  askNow,
  hits,
  words,
}: {
  /** The answer to the words in the box, or the replay of the last one. */
  readonly answer: GoingAnswer | undefined;
  /** Asks for the words at once, answering whether an answer is coming (`useGoingAnswer`). */
  readonly askNow: () => boolean;
  /** The hits the bar lists, which an argument naming an item is picked from. */
  readonly hits: readonly SearchResult[];
  /** The words in the box, trimmed. */
  readonly words: string;
}): GoingOperations {
  const writes = useWrites();
  // A replay draws an answer the person already had, whose writes were theirs to make then.
  const current = answer !== undefined && !answer.replayed ? answer : undefined;
  const runId = current?.runId;
  const decision: Decision | undefined = runId === undefined ? undefined : current?.decision;
  const proposal = decision === undefined ? undefined : current?.proposal;
  const settings = useQuery({ ...goingSettingsQuery(), enabled: decision !== undefined });
  // A setting that could not be read is no reason to hold the write: Dispatch refuses a write
  // sent at once while the setting is on, and the line says so.
  const confirmWrites = settings.isError ? false : settings.data?.confirm_writes;
  const [stage, setStage] = useWordsState<Stage>(words);
  const turn = useTurn(runId);
  const ran = new Set(turn?.lines.map((line) => line.action) ?? []);
  // Each line's own invocation, `action:index`, so an action whose automatic invocations ran
  // still offers the ones the person has to pick.
  const ranInvocations = new Set(turn?.lines.map((line) => line.key) ?? []);
  // The field a missing argument is asked in takes focus as it appears, so the person types the
  // value where the bar asked for it; a ref of one identity, so a later render moves nothing.
  const focusField = useCallback((field: HTMLInputElement | HTMLSelectElement | null) => {
    field?.focus();
  }, []);

  const run = (action: string, approved: GoingApproval, fills: Fills) => {
    const offer = offerOf(proposal, action);
    if (runId === undefined || current === undefined || offer === undefined) return;
    setStage({ kind: "idle", words });
    void runGoingWrites(
      writes,
      runIndexes(offer, approved).flatMap((index) => {
        const invocation = offer.invocations[index];
        if (invocation === undefined) return [];
        return [
          {
            action,
            approved,
            arguments: invocation.arguments,
            ...(invocation.missing === null
              ? {}
              : {
                  fill: {
                    field: invocation.missing.field,
                    value: fillValue(invocation.missing, (fills[index] ?? "").trim()),
                  },
                }),
            invocation: index,
            runId,
            target: invocation.target_argument,
            words: current.words,
          },
        ];
      })
    );
  };
  const apply = (step: Step) => {
    if (step.kind === "run") run(step.action, step.approved, step.fills);
    else setStage(step);
  };
  const begin = (wanted: Wanted) => apply(nextStep(words, wanted, current, confirmWrites));

  // A submit made before the run could say what to run waits for it: the decision, its proposal
  // and the setting, each as it arrives.
  const preparing = stage?.kind === "preparing" ? stage : undefined;
  // biome-ignore lint/correctness/useExhaustiveDependencies: `apply` reads this render's run, and the step changes only with what it is computed from
  useEffect(() => {
    if (preparing === undefined) return;
    const step = nextStep(words, preparing, current, confirmWrites);
    if (step.kind !== "preparing") apply(step);
  }, [preparing, current, confirmWrites, words]);

  const submit = () => {
    if (stage?.kind === "filling") {
      begin({ action: stage.action, approved: stage.approved, fills: stage.fills });
      return;
    }
    if (stage?.kind === "confirming" || stage?.kind === "preparing") return;
    if (current === undefined && !askNow()) return;
    begin({ action: undefined, approved: "auto", fills: {} });
  };

  const options: OperationOption[] = [];
  let sectionLabel = "";
  if (decision !== undefined) {
    const acted = decision.act ? decision.actions[0]?.action : undefined;
    if (acted !== undefined) {
      sectionLabel = "Operation";
      const actedOffer = offerOf(proposal, acted);
      const automatic = runIndexes(actedOffer, "auto");
      if (!ran.has(acted) && (proposal === undefined || automatic.length > 0)) {
        const waiting = stage?.kind === "preparing";
        const label = rowLabel(acted, actedOffer && offerWith(actedOffer, automatic));
        options.push({
          hint: waiting ? (
            <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>Preparing…</span>
          ) : (
            <button
              className={secondaryButtonCompact}
              data-testid="going-run"
              onClick={(event) => {
                // The row's own press is the same submit; this one must not reach it as well.
                event.stopPropagation();
                submit();
              }}
              type="button"
            >
              Run
            </button>
          ),
          label,
          row: { id: OPERATION_OPTION_ID, kind: "offer", label, select: submit },
        });
      }
      // The invocations no submit runs - each names an item neither the page nor the words named -
      // are a row of their own, which runs them only when the person picks it.
      const offered = (actedOffer?.invocations ?? []).flatMap((invocation, index) =>
        invocation.automatic || ranInvocations.has(`${acted}:${index}`) ? [] : [index]
      );
      if (actedOffer !== undefined && offered.length > 0) {
        const label = rowLabel(acted, offerWith(actedOffer, offered));
        options.push({
          hint: <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>Pick to run</span>,
          label,
          row: {
            id: offerOptionId(acted),
            kind: "offer",
            label,
            select: () => begin({ action: acted, approved: "person", fills: {} }),
          },
        });
      }
    } else {
      sectionLabel = "Suggested actions: pick one to run it";
      for (const { action } of decision.actions) {
        if (action === "none" || OWN_OFFERS[action] === true || ran.has(action)) continue;
        if (proposal !== undefined && offerOf(proposal, action) === undefined) continue;
        const label = rowLabel(action, offerOf(proposal, action));
        options.push({
          hint: <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>Pick to run</span>,
          label,
          row: {
            id: offerOptionId(action),
            kind: "offer",
            label,
            select: () => begin({ action, approved: "person", fills: {} }),
          },
        });
      }
    }
  }

  const named = (action: string) => decision?.actions.some((decided) => decided.action === action);
  const issueOffer = offerOf(proposal, "dispatch_issue");
  const sendOffer = offerOf(proposal, "dispatch_send_to_agent");
  const newIssue: GoingOperations["newIssue"] =
    runId === undefined || current === undefined || named("dispatch_issue") !== true
      ? undefined
      : {
          file:
            issueOffer === undefined
              ? undefined
              : (store, write) =>
                  fileNewIssue(store, write, async (through, filing) => {
                    const invocation = issueOffer.invocations[0];
                    const written = await runGoingWrite(through, {
                      action: "dispatch_issue",
                      approved: "person",
                      arguments: invocation?.arguments ?? {},
                      invocation: 0,
                      replace: newIssueFields(filing),
                      runId,
                      target: invocation?.target_argument ?? "",
                      words: current.words,
                    });
                    // The write's line names the issue it made: the key is all the filing reads.
                    return written === null || written.item === "" ? null : { key: written.item };
                  }),
          offered: issueOffer !== undefined,
        };
  const sendToAgent: SendToAgent | undefined =
    runId === undefined || current === undefined || sendOffer === undefined
      ? undefined
      : async (store, { block, issue, session, words: typed }) => {
          const invocation = sendOffer.invocations[0];
          const written = await runGoingWrite(store, {
            action: "dispatch_send_to_agent",
            approved: "person",
            arguments: invocation?.arguments ?? {},
            invocation: 0,
            replace: { body: handoffBody(issue, block, typed), delivery: "aside", session },
            runId,
            target: invocation?.target_argument ?? "",
            words: current.words,
          });
          // Dispatch writes the message under a 201 even where its delivery failed, so the line's
          // record of that delivery says whether the words reached the session.
          const delivery = written?.delivery;
          if (delivery?.state !== "failed") return undefined;
          return delivery.error ?? "Dispatch gave no reason.";
        };

  const stageShown = stage !== undefined && stage.kind !== "idle";
  const panel =
    !stageShown && (turn === undefined || turn.lines.length === 0) ? null : (
      <section
        aria-label="Operation"
        className={`max-h-[30vh] shrink-0 overflow-y-auto border-t px-3 py-2 text-sm ${borderDefault}`}
        data-run={runId}
        data-testid="going-operation"
      >
        {stage?.kind === "preparing" ? (
          <p className={barNoteClassName} data-testid="going-operation-preparing" role="status">
            Preparing the operation: it runs as soon as the answer says what to run.
          </p>
        ) : null}
        {stage?.kind === "unoffered" ? (
          <p className={barNoteClassName} data-testid="going-operation-unoffered" role="status">
            Nothing to run: the answer gave {actionName(stage.action)} nothing it can run with.
          </p>
        ) : null}
        {stage?.kind === "picks" ? (
          <p className={barNoteClassName} data-testid="going-operation-picks" role="status">
            Nothing ran at once: the answer has {actionName(stage.action)} act on items neither this
            page nor your words named. Pick it to run it.
          </p>
        ) : null}
        {stage?.kind === "filling" ? (
          <MissingArguments
            focusField={focusField}
            fills={stage.fills}
            hits={hits}
            offer={offerOf(proposal, stage.action)}
            onFill={(fills) => setStage({ ...stage, fills })}
            onSubmit={submit}
            runs={runIndexes(offerOf(proposal, stage.action), stage.approved)}
          />
        ) : null}
        {stage?.kind === "confirming" ? (
          <ConfirmStep
            action={stage.action}
            offer={offerOf(proposal, stage.action)}
            onCancel={() => setStage({ kind: "idle", words })}
            onConfirm={() => run(stage.action, "confirmed", stage.fills)}
            runs={runIndexes(offerOf(proposal, stage.action), "confirmed")}
          />
        ) : null}
        {turn === undefined || turn.lines.length === 0 ? null : (
          <TurnLines
            onUndo={() => void undoGoingTurn(writes, turn.runId, turn.words)}
            turn={turn}
          />
        )}
      </section>
    );

  return {
    newIssue,
    panel,
    section:
      options.length === 0 ? undefined : { id: OPERATION_GROUP_ID, label: sectionLabel, options },
    sendToAgent,
    submit,
  };
}

/** The field an invocation's one missing argument is asked in: a text, a number, one of an
 *  enumeration's values, or an item the bar's hits list - for each invocation the write will run
 *  (`runs`, `runIndexes`). */
function MissingArguments({
  focusField,
  fills,
  hits,
  offer,
  onFill,
  onSubmit,
  runs,
}: {
  readonly focusField: (field: HTMLInputElement | HTMLSelectElement | null) => void;
  readonly fills: Fills;
  readonly hits: readonly SearchResult[];
  readonly offer: GoingOffer | undefined;
  readonly onFill: (fills: Fills) => void;
  readonly onSubmit: () => void;
  readonly runs: readonly number[];
}): ReactNode {
  if (offer === undefined) return null;
  const asked = offer.invocations.flatMap((invocation, index) =>
    invocation.missing === null || !runs.includes(index)
      ? []
      : [{ index, invocation, missing: invocation.missing }]
  );
  const submitted = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    onSubmit();
  };
  return (
    <form className="space-y-2" data-testid="going-operation-fill" onSubmit={submitted}>
      {asked.map(({ index, invocation, missing }, position) => {
        const target = targetOf(invocation.arguments, invocation.target_argument);
        const label = `Fill ${missing.field === "@session" ? "the session" : missing.field}${
          target === "" ? "" : ` for ${target}`
        }`;
        const value = fills[index] ?? "";
        const change = (next: string) => onFill({ ...fills, [index]: next });
        const ref = position === 0 ? focusField : undefined;
        const field = `block w-full rounded-md border px-2 py-1 text-sm ${inputClasses(false)} ${textPrimaryOnSurface}`;
        const items =
          missing.kind === "item" ? hits.filter((hit) => hit.kind === missing.item) : [];
        const id = `going-fill-${index}`;
        return (
          <div className={`grid gap-1 text-xs ${textMutedOnSurface}`} key={id}>
            <label htmlFor={id}>{label}</label>
            {missing.kind === "enum" || items.length > 0 ? (
              <select
                className={field}
                id={id}
                onChange={(event) => change(event.target.value)}
                ref={ref}
                value={value}
              >
                <option value="">Pick one</option>
                {missing.kind === "enum"
                  ? (missing.choices ?? []).map((choice) => (
                      <option key={choice} value={choice}>
                        {choice}
                      </option>
                    ))
                  : items.map((hit) => (
                      <option key={hit.id} value={hit.id}>
                        {hit.id}
                      </option>
                    ))}
              </select>
            ) : (
              <input
                className={field}
                id={id}
                onChange={(event) => change(event.target.value)}
                ref={ref}
                type={missing.kind === "number" ? "number" : "text"}
                value={value}
              />
            )}
          </div>
        );
      })}
      <button className={secondaryButtonCompact} type="submit">
        Run
      </button>
    </form>
  );
}

/** The step a person whose setting asks before every write takes before the bar writes: the
 *  writes a confirm would make (`runs`: the automatic ones, as a submit would), each in its line,
 *  and Confirm or Cancel. An invocation the person has to pick is not among them. */
function ConfirmStep({
  action,
  offer,
  onCancel,
  onConfirm,
  runs,
}: {
  readonly action: string;
  readonly offer: GoingOffer | undefined;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
  readonly runs: readonly number[];
}): ReactNode {
  // Each line named once, ahead of the list: an offer's invocations are its own, in its order.
  const asking = (offer?.invocations ?? []).flatMap((invocation, index) =>
    runs.includes(index)
      ? [
          {
            key: `${action}:${index}`,
            text: `${actionOn(action, targetOf(invocation.arguments, invocation.target_argument))}: asks first`,
          },
        ]
      : []
  );
  return (
    <div data-testid="going-confirm-step">
      <p className={barNoteClassName}>Your setting asks before every write. Run these?</p>
      <ul className="space-y-1">
        {asking.map((line) => (
          <li
            className={`text-xs ${textPrimaryOnSurface}`}
            data-state="asking"
            data-testid="going-write-line"
            key={line.key}
          >
            {line.text}
          </li>
        ))}
      </ul>
      <div className="mt-2 flex gap-2">
        <button
          className={secondaryButtonCompact}
          data-testid="going-confirm"
          onClick={onConfirm}
          type="button"
        >
          Confirm
        </button>
        <button className={secondaryButtonCompact} onClick={onCancel} type="button">
          Cancel
        </button>
      </div>
    </div>
  );
}

/** What one line of a turn says: the item, the field, the value before and the value now once
 *  Dispatch answered; its refusal where it refused; and how its undo went. */
function lineText(line: TurnLine): string {
  const named = actionOn(line.action, line.target);
  switch (line.state) {
    case "sending":
      return `${named}${line.change === "" ? "" : ` (${line.change})`}: sending…`;
    case "stopped":
      return `${named}: stopped before it was sent`;
    case "refused":
      return `${named}: refused: ${line.refusal?.message ?? ""}`;
    case "done": {
      const written = line.written;
      if (written === undefined) return `${named}: done; Dispatch kept no answer naming it`;
      const item = written.item === "" ? named : written.item;
      const fields = (written.fields ?? []).join(", ");
      return fields === ""
        ? `${item}: done`
        : `${item} · ${fields}: ${shownValue(written.before, written.fields)} → ${shownValue(
            written.now,
            written.fields
          )}`;
    }
  }
}

/** One line per write of a turn, and the turn's Undo. */
function TurnLines({
  onUndo,
  turn,
}: {
  readonly onUndo: () => void;
  readonly turn: Turn;
}): ReactNode {
  const undo = turn.undo;
  return (
    <div className="mt-1">
      <ul className="space-y-1" data-testid="going-write-lines">
        {turn.lines.map((line) => (
          <li
            className={`text-xs ${line.state === "refused" ? dangerText : textPrimaryOnSurface}`}
            data-action={line.action}
            data-before={
              line.written === undefined
                ? undefined
                : shownValue(line.written.before, line.written.fields)
            }
            data-code={line.refusal?.code}
            data-field={
              line.written === undefined ? undefined : (line.written.fields ?? []).join(", ")
            }
            data-item={line.written?.item ?? line.target}
            data-now={
              line.written === undefined
                ? undefined
                : shownValue(line.written.now, line.written.fields)
            }
            data-state={line.state}
            data-testid="going-write-line"
            data-undo={
              line.undo === undefined ? undefined : line.undo.outcome.ok ? "ok" : "refused"
            }
            key={line.key}
            role={line.state === "refused" ? "alert" : undefined}
          >
            {lineText(line)}
            {line.undo === undefined ? null : (
              <span className={line.undo.outcome.ok ? textMutedOnSurface : dangerText}>
                {` · ${undoneText(line.undo)}`}
              </span>
            )}
          </li>
        ))}
      </ul>
      <div className="mt-2 flex items-center gap-2">
        {undo?.state === "undone" ? null : (
          <button
            className={secondaryButtonCompact}
            data-testid="going-undo"
            disabled={undo?.state === "undoing"}
            onClick={onUndo}
            type="button"
          >
            Undo
          </button>
        )}
        <UndoSaid undo={undo} />
      </div>
    </div>
  );
}

/** What a turn's undo says beside its lines. */
export function UndoSaid({ undo }: { readonly undo: Turn["undo"] }): ReactNode {
  if (undo === undefined) return null;
  const stopped =
    undo.stopped === 0
      ? ""
      : ` ${undo.stopped === 1 ? "One write" : `${undo.stopped} writes`} still waiting ${
          undo.stopped === 1 ? "was" : "were"
        } stopped before ${undo.stopped === 1 ? "it was" : "they were"} sent.`;
  if (undo.state === "undoing") {
    return (
      <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-undo-said" role="status">
        Undoing…{stopped}
      </p>
    );
  }
  if (undo.state === "refused") {
    return (
      <p className={`text-xs ${dangerText}`} data-testid="going-undo-said" role="alert">
        Could not undo: {undo.message}
        {stopped}
      </p>
    );
  }
  return (
    <p className={`text-xs ${textMutedOnSurface}`} data-testid="going-undo-said" role="status">
      {undo.message ?? "Undone."}
      {stopped}
    </p>
  );
}
