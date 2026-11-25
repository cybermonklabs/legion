/*
 * The steps of a command-bar run (design v2, states 3 and 4), ported from the tool section of
 * slopus/happy-desktop at commit 533c9a1: `packages/happy-desktop-ui/src/AgentToolSection.tsx`, a
 * run of tool calls drawn as grey lines, live while it runs and summed up once it is over by what
 * it was made of and how long it took; `conversationToolSections.ts`, where the calls between two
 * pieces of the agent's text are one section; and `elapsedTimeFormat.ts`, an elapsed time in its
 * smallest useful units. The port keeps those rules in Dispatch's words and its Tailwind classes
 * (`theme/classes`), not Happy's CSS variables. It draws each call as a line of its own, as the
 * bar's design asks, and leaves out what the bar does not use: the letter-by-letter arrival
 * (`ArrivingText`), the tool icons and the detail view a call opens. `package.json` and `LICENSE`
 * beside this file list the code in the build's THIRD_PARTY_NOTICES.txt.
 *
 * The code it is ported from is under happy-desktop's MIT license (its `LICENSE`):
 *
 * MIT License
 *
 * Copyright (c) 2026 Happy (2) Contributors
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

import { type ReactNode, useLayoutEffect, useRef, useState } from "react";

import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../../theme/classes";
import { TimedWords } from "../../refs/dispatch-time";
import { buildReferencePath } from "../../refs/routes";
import type { RunStep } from "../run-frames";

/** How many finished steps show as lines while the run works; every older one folds into one
 *  line (design v3, state 3: "at most 3 finished lines show"). */
export const FINISHED_LINES = 3;
/** How many calls that run at once show as lines; the rest fold into "and 2 more…". */
export const RUNNING_LINES = 3;

/** The shimmer of a line that runs. It stops where the person asked for less motion. */
const SHIMMER = "motion-safe:animate-pulse";

/** An issue key, as the server names the items of a step (`going/agent/labels.go`). */
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$/;

/** The first word of a header the model wrote beside its calls, where it is a verb in "-ing"
 *  ("Reading the telescope's issues…"), and the rest of the header after it. */
const LEADING_PARTICIPLE = /^([A-Z][a-z]*(?:-[a-z]+)*ing)\b(.*)$/su;

/** Words in "-ing" that start a header without being a verb in its present tense, which keep
 *  their form however the calls under them end. */
const NOT_PARTICIPLES: Readonly<Record<string, true>> = {
  anything: true,
  being: true,
  bring: true,
  ceiling: true,
  during: true,
  evening: true,
  everything: true,
  king: true,
  morning: true,
  nothing: true,
  ring: true,
  sing: true,
  something: true,
  spring: true,
  sting: true,
  string: true,
  swing: true,
  thing: true,
  wing: true,
};

/** Verbs whose past form is not the "-ing" form's stem and "-ed". */
const IRREGULAR_PAST: Readonly<Record<string, string>> = {
  beginning: "began",
  bringing: "brought",
  building: "built",
  buying: "bought",
  choosing: "chose",
  digging: "dug",
  doing: "did",
  drawing: "drew",
  finding: "found",
  getting: "got",
  giving: "gave",
  going: "went",
  having: "had",
  holding: "held",
  keeping: "kept",
  leaving: "left",
  making: "made",
  putting: "put",
  reading: "read",
  running: "ran",
  seeing: "saw",
  seeking: "sought",
  sending: "sent",
  setting: "set",
  splitting: "split",
  taking: "took",
  telling: "told",
  thinking: "thought",
  writing: "wrote",
};

/** The past form of a verb in "-ing", in lower case: an irregular verb's own ("reading" →
 *  "read"), else its stem and "-ed", which already has a doubled consonant ("planning" →
 *  "planned") or the stem a silent "e" left ("comparing" → "compared"), with a "y" after a
 *  consonant turned to "ied" ("verifying" → "verified") and "ee" taking "d" ("agreeing" →
 *  "agreed"). */
function pastForm(participle: string): string {
  const irregular = IRREGULAR_PAST[participle];
  if (irregular !== undefined) return irregular;
  const stem = participle.slice(0, -3);
  if (/[^aeiou]y$/u.test(stem)) return `${stem.slice(0, -1)}ied`;
  return stem.endsWith("ee") ? `${stem}d` : `${stem}ed`;
}

/**
 * A header the model wrote beside calls that have all ended, in the past tense (design v3, rule
 * 2): its "…" dropped, so it does not read as work still going on, and a first word that is a
 * verb in "-ing" put in its past form, "Reading the telescope's issues…" → "Read the telescope's
 * issues". A header that starts any other way keeps its words.
 */
export function pastHeader(header: string): string {
  const words = header.replace(/\s*(?:…|\.\.\.)$/u, "");
  const leading = LEADING_PARTICIPLE.exec(words);
  const first = leading?.[1]?.toLowerCase();
  if (
    leading === null ||
    first === undefined ||
    first.length < 5 ||
    NOT_PARTICIPLES[first] === true
  ) {
    return words;
  }
  const past = pastForm(first);
  return `${past.charAt(0).toUpperCase()}${past.slice(1)}${leading[2] ?? ""}`;
}

/**
 * An elapsed time in its smallest useful units: "48 s", "1 min 48 s", "2 h 5 min". It is Happy's
 * `elapsedTimeFormat`, in the words the bar writes a time in.
 */
export function elapsedTimeWords(elapsedMs: number): string {
  const totalSeconds = Math.max(0, Math.floor(elapsedMs / 1_000));
  if (totalSeconds < 60) return `${totalSeconds} s`;
  const totalMinutes = Math.floor(totalSeconds / 60);
  if (totalMinutes < 60) return `${totalMinutes} min ${totalSeconds % 60} s`;
  return `${Math.floor(totalMinutes / 60)} h ${totalMinutes % 60} min`;
}

/** How often something was done, in words: "once", "twice", "3 times". */
function times(count: number): string {
  if (count === 1) return "once";
  return count === 2 ? "twice" : `${count} times`;
}

/** `count` of `noun`, counted: "1 item", "14 items". */
function counted(count: number, noun: string): string {
  return `${count} ${count === 1 ? noun : `${noun}s`}`;
}

/** What each kind of step adds to a fold's words, in the order a fold names them. A read that
 *  answered a list counts its results as the items it read, and any other read the one thing it
 *  read. A step that drew a block says what it drew - a chart, a table or one number - as its kind
 *  does once its block came (`run-frames.ts`). */
const KIND_WORDS: readonly (readonly [string, (steps: readonly RunStep[]) => string])[] = [
  [
    "read",
    (steps) =>
      `read ${counted(
        steps.reduce((sum, step) => sum + Math.max(step.count, 1), 0),
        "item"
      )}`,
  ],
  ["search", (steps) => `searched ${times(steps.length)}`],
  ["look", (steps) => `looked for a way ${times(steps.length)}`],
  ["chart", (steps) => (steps.length === 1 ? "drew a chart" : `drew ${steps.length} charts`)],
  ["table", (steps) => (steps.length === 1 ? "drew a table" : `drew ${steps.length} tables`)],
  [
    "metric",
    (steps) => (steps.length === 1 ? "showed a number" : `showed ${steps.length} numbers`),
  ],
  ["plan", (steps) => (steps.length === 1 ? "proposed a plan" : `proposed ${steps.length} plans`)],
  [
    "review",
    (steps) =>
      steps.length === 1 ? "reviewed the plan" : `reviewed the plan ${times(steps.length)}`,
  ],
];

/** The kinds `KIND_WORDS` names. */
const NAMED_KINDS: ReadonlySet<string> = new Set(KIND_WORDS.map(([kind]) => kind));

/**
 * What `steps` were made of, in words joined with ", ": "read 14 items, searched twice, drew a
 * chart". It is Happy's `sectionKinds`, which counts each kind of step, with four changes: each
 * kind has its own words; the kinds come in one fixed order, not the most frequent first; a
 * failed step counts as "1 step failed", not with its kind, since it did not do what its kind
 * says; and a slip counts nowhere, since it did nothing and its person has nothing to do about
 * it. A step of a kind this page does not know counts among "other steps". Steps that were all
 * slips are said in no words at all: "". Once the run's plan ran, `plan` says what that did
 * ("applied 3 changes") in place of "proposed a plan", which is no longer the last thing the run
 * did.
 */
export function foldWords(steps: readonly RunStep[], plan?: string): string {
  const counts = steps.filter((step) => !step.slip);
  const done = counts.filter((step) => !step.refused);
  const parts: string[] = [];
  for (const [kind, words] of KIND_WORDS) {
    const ofKind = done.filter((step) => step.kind === kind);
    if (ofKind.length > 0) parts.push(kind === "plan" && plan !== undefined ? plan : words(ofKind));
  }
  const other = done.filter((step) => !NAMED_KINDS.has(step.kind)).length;
  if (other > 0) parts.push(counted(other, "other step"));
  const failed = counts.length - done.length;
  if (failed > 0) parts.push(`${counted(failed, "step")} failed`);
  return parts.join(", ");
}

/** How long a run worked, for the line its steps fold into: "1 min 48 s"; undefined, as in
 *  Happy, for a run that worked for less than a second, and for one whose time the page does not
 *  know. */
function workedTime(workedMs: number | undefined): string | undefined {
  return workedMs === undefined || workedMs < 1_000 ? undefined : elapsedTimeWords(workedMs);
}

/** What the line steps fold into says after how long the run worked: what the steps were made
 *  of (`foldWords`, with what the run's plan did), with its first letter a capital where no time
 *  comes before it, and "Steps" for steps that were all slips and no time to say. */
function workedRest(
  steps: readonly RunStep[],
  time: string | undefined,
  plan: string | undefined
): string {
  const words = foldWords(steps, plan);
  if (time !== undefined) return words;
  return words === "" ? "Steps" : words.charAt(0).toUpperCase() + words.slice(1);
}

/**
 * The one line steps fold into once the run's answer or its plan came, or the run ended (design
 * v2, state 4): every step's, with how long the run worked, "Worked for 1 min 48 s · read 14
 * items, searched twice". A run whose time `workedTime` does not say says what the steps were
 * made of alone: "Read 6 items, searched twice". Steps that were all slips say only the time,
 * "Worked for 12 s", or with no time to say, "Steps". `plan` is what the run's plan did once it
 * ran (`foldWords`). The time stands in an element of its own (`going-steps-fold-time`), so that
 * a screenshot can mask the one part that moves with the run's pace.
 */
function WorkedLine({
  plan,
  steps,
  workedMs,
}: {
  readonly plan: string | undefined;
  readonly steps: readonly RunStep[];
  readonly workedMs: number | undefined;
}): ReactNode {
  const time = workedTime(workedMs);
  const rest = workedRest(steps, time, plan);
  if (time === undefined) return rest;
  return (
    <>
      Worked for <span data-testid="going-steps-fold-time">{time}</span>
      {rest === "" ? null : ` · ${rest}`}
    </>
  );
}

/** Which steps draw as lines while the run works (design v2, state 3). */
export interface StepLines {
  /** The finished steps that fold into one line above the rest: all but the newest
   *  `FINISHED_LINES`, where they are two or more. */
  readonly older: readonly RunStep[];
  /** The steps drawn as lines, in their order: the newest finished ones and the first
   *  `RUNNING_LINES` of those that run. */
  readonly shown: readonly RunStep[];
  /** How many running steps past `RUNNING_LINES` fold into "and N more…". */
  readonly more: number;
}

/** Which of `steps` draw as lines while the run works, and which fold. The finished steps past
 *  the newest `FINISHED_LINES` fold into one line where they are two or more (review a7); one
 *  such step shows as its own line, since a line folded into one line saves nothing (review N2). */
export function stepLines(steps: readonly RunStep[]): StepLines {
  const finished = steps.filter((step) => !step.running);
  const running = steps.filter((step) => step.running);
  const past = finished.length - FINISHED_LINES;
  const older = past >= 2 ? finished.slice(0, past) : [];
  const folded = new Set([...older, ...running.slice(RUNNING_LINES)]);
  return {
    more: Math.max(0, running.length - RUNNING_LINES),
    older,
    shown: steps.filter((step) => !folded.has(step)),
  };
}

/**
 * The time since `since`, in this page's clock, counted each second (`elapsedTimeWords`). It
 * writes the time into the page itself, so a tick draws nothing else again, and it is hidden from
 * a screen reader, which would hear every second.
 */
export function RunTimer({
  className = "",
  since,
  testId,
}: {
  readonly className?: string;
  readonly since: number;
  readonly testId?: string;
}): ReactNode {
  const shown = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    const write = () => {
      if (shown.current !== null) {
        shown.current.textContent = elapsedTimeWords(Date.now() - since);
      }
    };
    write();
    const timer = window.setInterval(write, 1000);
    return () => window.clearInterval(timer);
  }, [since]);
  return (
    <span
      aria-hidden="true"
      className={`shrink-0 tabular-nums ${className}`}
      data-testid={testId}
      ref={shown}
    />
  );
}

/** A step's words, each issue key among the items it named a plain link to its issue. */
function LabelWords({
  items,
  label,
}: {
  readonly items: readonly string[];
  readonly label: string;
}): ReactNode {
  const keys = items.filter((item) => ISSUE_KEY.test(item));
  if (keys.length === 0) return label;
  const parts: ReactNode[] = [];
  let at = 0;
  for (const match of label.matchAll(new RegExp(`\\b(?:${keys.join("|")})\\b`, "g"))) {
    parts.push(label.slice(at, match.index));
    parts.push(
      <a
        className={`${linkText} ${linkHoverText}`}
        href={buildReferencePath({ key: match[0], kind: "issue" })}
        key={match.index}
      >
        {match[0]}
      </a>
    );
    at = match.index + match[0].length;
  }
  parts.push(label.slice(at));
  return <>{parts}</>;
}

/** One step's line: its words in the present tense with a shimmer while it runs, in the past
 *  tense with its result once it ended, or the sentence of a call that failed, in red; a slip's
 *  line is a lighter grey than an ended call's, with what the model tried, since it did nothing.
 *  The newest line of a run that works carries the run's timer right beside its words, where the
 *  eye reads it with them, counted from `since`, where one is given. */
function StepLine({
  since,
  step,
}: {
  readonly since: number | undefined;
  readonly step: RunStep;
}): ReactNode {
  const ink = step.refused ? dangerText : step.slip ? textMutedOnSurface : textSecondaryOnSurface;
  return (
    <li
      className={`flex min-w-0 items-baseline gap-2 text-[13px] ${ink}`}
      data-kind={step.kind}
      data-running={String(step.running)}
      data-testid="going-step"
      data-tool={step.tool}
    >
      <span
        className={`min-w-0 truncate ${step.running ? SHIMMER : ""}`}
        data-testid="going-step-label"
      >
        <LabelWords items={step.items} label={step.label} />
        {step.running ? "…" : null}
      </span>
      {since === undefined ? null : (
        <RunTimer className={textMutedOnSurface} since={since} testId="going-step-timer" />
      )}
    </li>
  );
}

/** A run of steps one model call made, under the prose it wrote beside them. */
interface HeadedSteps {
  readonly header: string;
  readonly first: number;
  readonly steps: RunStep[];
}

/** `steps` in runs that share a header, in their order: the calls between two pieces of the
 *  model's text are one run, as Happy's `conversationToolSections` groups them. */
function headed(steps: readonly RunStep[]): readonly HeadedSteps[] {
  const groups: HeadedSteps[] = [];
  for (const step of steps) {
    const last = groups.at(-1);
    if (last !== undefined && last.header === step.header) {
      last.steps.push(step);
    } else {
      groups.push({ first: step.id, header: step.header, steps: [step] });
    }
  }
  return groups;
}

/** The lines of `steps`: each run of calls under its grey header line where the model wrote one,
 *  then "and N more…" for the running calls past `RUNNING_LINES`. The newest line carries the
 *  run's timer, counted from `since`, where one is given and a call runs; a run that works with
 *  no call running - the model is writing or deciding - ends with "Thinking…" and the timer
 *  instead, so a working run never reads as idle. */
function StepList({
  more,
  since,
  steps,
}: {
  readonly more: number;
  readonly since: number | undefined;
  readonly steps: readonly RunStep[];
}): ReactNode {
  const callRuns = more > 0 || steps.some((step) => step.running);
  const newest = more > 0 || !callRuns ? undefined : steps.at(-1)?.id;
  return (
    <div className="space-y-0.5" data-testid="going-steps">
      {headed(steps).map((group) => (
        <div className="space-y-0.5" key={group.first}>
          {group.header === "" ? null : (
            // Once every call of the run ended, the header is in the past tense (`pastHeader`),
            // so it does not read as work still going on.
            <p
              className={`truncate text-[13px] ${textMutedOnSurface}`}
              data-testid="going-step-words"
            >
              <TimedWords
                text={
                  group.steps.some((step) => step.running) ? group.header : pastHeader(group.header)
                }
              />
            </p>
          )}
          <ol aria-label="Steps" className={`space-y-0.5 ${group.header === "" ? "" : "pl-3"}`}>
            {group.steps.map((step) => (
              <StepLine key={step.id} since={step.id === newest ? since : undefined} step={step} />
            ))}
          </ol>
        </div>
      ))}
      {more === 0 ? null : (
        <p
          className={`flex items-baseline gap-2 text-[13px] ${textSecondaryOnSurface}`}
          data-testid="going-steps-more"
        >
          <span className={SHIMMER}>and {more} more…</span>
          {since === undefined ? null : (
            <RunTimer className={textMutedOnSurface} since={since} testId="going-step-timer" />
          )}
        </p>
      )}
      {since === undefined || callRuns ? null : (
        <p
          className={`flex items-baseline gap-2 text-[13px] ${textSecondaryOnSurface}`}
          data-testid="going-steps-thinking"
        >
          <span className={SHIMMER}>Thinking…</span>
          <RunTimer className={textMutedOnSurface} since={since} testId="going-thinking-timer" />
        </p>
      )}
    </div>
  );
}

/** A grey line that stands for steps folded under it, and opens them on a click. */
function FoldLine({
  onToggle,
  open,
  testId,
  words,
}: {
  readonly onToggle: () => void;
  readonly open: boolean;
  readonly testId: string;
  readonly words: ReactNode;
}): ReactNode {
  return (
    <button
      aria-expanded={open}
      className={`flex min-h-6 max-w-full items-center gap-1 text-left text-xs hover:underline ${textMutedOnSurface}`}
      data-testid={testId}
      onClick={onToggle}
      type="button"
    >
      <span className="min-w-0 truncate">{words}</span>
      <span aria-hidden="true" className={`shrink-0 ${open ? "rotate-90" : ""}`}>
        ›
      </span>
    </button>
  );
}

/**
 * The steps of one run, as the bar over the page and the panel at the side both draw them.
 *
 * While the run works (design v2, state 3), each call has a line, as Happy's section is live:
 * the newest finished ones, at most `FINISHED_LINES`, under one grey line that folds the older
 * ones where they are two or more and says how many ("2 earlier steps ›", which opens them; one
 * older step shows as its own line, review N2), then the
 * calls that run, at most `RUNNING_LINES` of them and "and 2 more…" for the rest. The prose the
 * model wrote beside its calls heads them as a grey line and folds with them. The newest line
 * carries the run's timer, counted from `since`.
 *
 * Once the run no longer works - its answer or its plan came, or it stopped or failed (`folded`,
 * design v2, states 4 and 9) - and while the rows of an approved plan run (state 7), every step
 * folds into one grey line, "Worked for 1 min 48 s · read 14 items, searched twice ›", as Happy's
 * section is summed up once it is over; a click opens them, and its mark turns to say so. The
 * line counts what the end of a stopped run counts ("read 14 items so far"), and says what the
 * run's plan did once it ran (`planWords`, "applied 3 changes"). The steps are the work, never
 * the result, so the line starts closed whatever the run's end (design v3, rule 3).
 */
export function StepSection({
  folded,
  live,
  planWords,
  since,
  steps,
  workedMs,
}: {
  /** The steps that draw a line: each call with words, without the changes of an approved plan,
   *  which the plan's card shows. */
  readonly steps: readonly RunStep[];
  /** Whether the run works, so that its newest line carries its timer. */
  readonly live: boolean;
  /** What the run's plan did once its rows ran, said in place of "proposed a plan". */
  readonly planWords?: string;
  /** When the person asked for the run, in this page's clock: what its timer counts from. */
  readonly since: number;
  /** Whether the run no longer works, or its approved plan's rows run, so that every step folds
   *  into one line. */
  readonly folded: boolean;
  /** How long the run worked, for its folded line; undefined where the page does not know. */
  readonly workedMs: number | undefined;
}): ReactNode {
  const [olderOpen, setOlderOpen] = useState(false);
  const [workedOpen, setWorkedOpen] = useState(false);
  if (steps.length === 0) {
    return null;
  }
  if (folded) {
    return (
      <div className="space-y-1" data-testid="going-steps-section">
        <FoldLine
          onToggle={() => setWorkedOpen((open) => !open)}
          open={workedOpen}
          testId="going-steps-fold"
          words={<WorkedLine plan={planWords} steps={steps} workedMs={workedMs} />}
        />
        {workedOpen ? <StepList more={0} since={undefined} steps={steps} /> : null}
      </div>
    );
  }
  const lines = stepLines(steps);
  const liveSince = live ? since : undefined;
  return (
    <div className="space-y-0.5" data-testid="going-steps-section">
      {lines.older.length === 0 ? null : (
        <FoldLine
          onToggle={() => setOlderOpen((open) => !open)}
          open={olderOpen}
          testId="going-steps-older"
          words={`${lines.older.length} earlier steps`}
        />
      )}
      {olderOpen ? (
        <StepList more={0} since={liveSince} steps={steps} />
      ) : (
        <StepList more={lines.more} since={liveSince} steps={lines.shown} />
      )}
    </div>
  );
}
