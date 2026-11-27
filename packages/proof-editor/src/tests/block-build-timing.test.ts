import {
  CommandsReady,
  ConfigReady,
  EditorStateReady,
  EditorViewReady,
  InitReady,
  KeymapReady,
  ParserReady,
  PasteRulesReady,
  SchemaReady,
  SerializerReady,
} from "@milkdown/core";
import type { TimerType } from "@milkdown/ctx";
import type { Window } from "happy-dom";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";

import { serverBlockSchema } from "../../tests/block-schema-fixture.js";
import { withDomWindow } from "../../tests/dom-window.js";
import {
  type CreateProofEditorOptions,
  createProofEditor,
  type ProofEditorHandle,
} from "../lib.js";
import { createHeadlessProof } from "../lib-headless.js";
import { test } from "./harness.js";

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message);
}

const TASK_MEASURE = "block-editor-task";

/** The phases of a block build, each measured once, in the order the build runs them. */
const PHASES = ["init", "schema", "editor state", "editor view", "view setup", "collaboration"];

/** Each of Milkdown's dependency timer lists the cut gates: the phase that releases it, the
 *  timers Milkdown puts in it, and the timer that its work resolves. */
const GATED = [
  { phase: "init", waits: [ConfigReady], ready: InitReady },
  { phase: "schema", waits: [InitReady], ready: SchemaReady },
  {
    phase: "editor state",
    waits: [ParserReady, SerializerReady, CommandsReady, KeymapReady],
    ready: EditorStateReady,
  },
  { phase: "editor view", waits: [EditorStateReady, PasteRulesReady], ready: EditorViewReady },
];

const WATCHED: readonly TimerType[] = [
  ConfigReady,
  InitReady,
  SchemaReady,
  ParserReady,
  SerializerReady,
  CommandsReady,
  KeymapReady,
  PasteRulesReady,
  EditorStateReady,
  EditorViewReady,
];

const SPEC_BODY = [
  ":::requirement{#r1}",
  "First requirement body.",
  ":::",
  "",
  ":::requirement{#r2}",
  "Second requirement body.",
  ":::",
  "",
].join("\n");

async function seededRoom(): Promise<Y.Doc> {
  const { parseMarkdown } = await createHeadlessProof({ blockSchema: serverBlockSchema });
  const room = new Y.Doc();
  prosemirrorToYXmlFragment(parseMarkdown(SPEC_BODY), room.getXmlFragment("prosemirror"));
  return room;
}

function findBlock(fragment: Y.XmlFragment, blockId: string): Y.XmlElement {
  for (const child of fragment.toArray()) {
    if (child instanceof Y.XmlElement && child.getAttribute("blockId") === blockId) return child;
  }
  throw new Error(`expected block ${JSON.stringify(blockId)} in fragment`);
}

interface Measured {
  readonly phase: unknown;
  readonly start: number;
  readonly end: number;
  /** The task the measure was recorded in. */
  readonly task: number;
}

interface Observed {
  readonly handle: ProofEditorHandle;
  /** The task the build answered in. */
  readonly answeredIn: number;
  /** When each task began, by its number. */
  readonly began: readonly number[];
  /** The task each of Milkdown's watched timers was resolved in, by the timer's name. */
  readonly resolved: ReadonlyMap<string, number>;
  readonly measures: readonly Measured[];
}

/**
 * Builds an editor and records the tasks the build runs in. The task that starts the build is
 * task 0, and each timer callback starts the next one. Milkdown resolves a timer by dispatching
 * an event with the timer's name on the global scope, so a listener added before the build sees
 * each resolution first and records its task. Each `block-editor-task` measure is recorded with
 * its task too.
 */
async function observeBuild(
  window: Window,
  options: Partial<CreateProofEditorOptions>,
  room: Y.Doc
): Promise<Observed> {
  const root = window.document.body.appendChild(window.document.createElement("div"));
  const setTimer = globalThis.setTimeout;
  const measure = performance.measure;
  let task = 0;
  const began = [performance.now()];
  const resolved = new Map<string, number>();
  const measures: Measured[] = [];
  const removals = WATCHED.map((timer) => {
    const listener = () => {
      if (!resolved.has(timer.name)) resolved.set(timer.name, task);
    };
    addEventListener(timer.name, listener);
    return () => removeEventListener(timer.name, listener);
  });
  globalThis.setTimeout = ((
    run: (...args: unknown[]) => void,
    delay?: number,
    ...args: unknown[]
  ) =>
    setTimer(() => {
      task += 1;
      began.push(performance.now());
      run(...args);
    }, delay)) as unknown as typeof setTimeout;
  performance.measure = ((name: string, measureOptions?: PerformanceMeasureOptions) => {
    if (name === TASK_MEASURE) {
      const detail: unknown = measureOptions?.detail;
      measures.push({
        phase:
          typeof detail === "object" && detail !== null ? Reflect.get(detail, "phase") : undefined,
        start: Number(measureOptions?.start),
        end: Number(measureOptions?.end),
        task,
      });
    }
    return measure.call(performance, name, measureOptions);
  }) as typeof performance.measure;
  try {
    const handle = await createProofEditor(root as unknown as HTMLElement, {
      blockSchema: serverBlockSchema,
      heatMapMode: "hidden",
      onMarkClick: () => {},
      onMarkHover: () => {},
      user: { color: "#0369a1", name: "alice" },
      ydoc: room,
      ...options,
    });
    return { handle, answeredIn: task, began, resolved, measures };
  } finally {
    globalThis.setTimeout = setTimer;
    performance.measure = measure;
    for (const remove of removals) remove();
  }
}

function taskOf(observed: Observed, timer: TimerType): number {
  const task = observed.resolved.get(timer.name);
  assert(task !== undefined, `expected Milkdown to resolve ${timer.name} during the build`);
  return task;
}

function assertBoundToR1(handle: ProofEditorHandle): void {
  const { doc } = handle.view.state;
  assert(
    doc.type.name === "requirement" && doc.attrs.blockId === "r1",
    `expected the view's root to be requirement r1, got ${doc.type.name} ${JSON.stringify(doc.attrs.blockId)}`
  );
  assert(
    doc.textContent === "First requirement body.",
    `expected the view to hold r1 alone, got ${JSON.stringify(doc.textContent)}`
  );
}

function assertPhasesMeasured(observed: Observed): void {
  const phases = observed.measures.map((measured) => measured.phase);
  assert(
    JSON.stringify(phases) === JSON.stringify(PHASES),
    `expected one ${TASK_MEASURE} measure for each phase, ${JSON.stringify(PHASES)}, got ${JSON.stringify(phases)}`
  );
  for (const measured of observed.measures) {
    assert(
      measured.end >= measured.start,
      `expected the ${String(measured.phase)} measure to end after it starts, got ${measured.start} to ${measured.end}`
    );
  }
}

console.log("\n=== createProofEditor block build timing ===");

await test("a build with element releases each dependency timer in a task of its own and records its measure", async () => {
  const room = await seededRoom();
  const r1 = findBlock(room.getXmlFragment("prosemirror"), "r1");
  await withDomWindow(async (window) => {
    const observed = await observeBuild(window, { element: r1 }, room);
    try {
      assertBoundToR1(observed.handle);
      assertPhasesMeasured(observed);
      for (const { phase, waits, ready } of GATED) {
        const readyTask = taskOf(observed, ready);
        for (const wait of waits) {
          assert(
            readyTask > taskOf(observed, wait),
            `expected the ${phase} work (${ready.name}) in a later task than ${wait.name}, got tasks ${readyTask} and ${taskOf(observed, wait)}`
          );
        }
        const measured = observed.measures.find((entry) => entry.phase === phase);
        assert(
          measured?.task === readyTask,
          `expected the ${phase} measure in the task that ran its work (${readyTask}), got ${measured?.task}`
        );
      }
      observed.measures.forEach((measured, index) => {
        const previous = observed.measures[index - 1];
        assert(
          measured.task > (previous?.task ?? 0),
          `expected the ${String(measured.phase)} phase in a task of its own after the one before it, got task ${measured.task} after ${previous?.task ?? 0}`
        );
        const began = observed.began[measured.task];
        assert(
          began !== undefined && measured.start >= began,
          `expected the ${String(measured.phase)} measure to start when its task began (${began}), got ${measured.start}`
        );
      });
      const last = observed.measures.at(-1);
      assert(
        last?.task === observed.answeredIn,
        `expected the last phase's measure in the task the build answers in (${observed.answeredIn}), got ${last?.task}`
      );
    } finally {
      await observed.handle.destroy();
    }
  });
});

await test("a build without element runs as main does", async () => {
  const room = await seededRoom();
  await withDomWindow(async (window) => {
    const observed = await observeBuild(window, {}, room);
    try {
      const { doc } = observed.handle.view.state;
      assert(
        doc.type.name === "doc" && doc.childCount === 2,
        `expected the whole document with both blocks, got ${doc.type.name} with ${doc.childCount}`
      );
      assert(
        observed.measures.length === 0,
        `expected no ${TASK_MEASURE} measure, got ${JSON.stringify(observed.measures)}`
      );
      assert(
        observed.answeredIn === 0,
        `expected the build to answer in the task that called it, got task ${observed.answeredIn}`
      );
      for (const timer of WATCHED) {
        assert(
          taskOf(observed, timer) === 0,
          `expected ${timer.name} in the task that called the build, got task ${taskOf(observed, timer)}`
        );
      }
    } finally {
      await observed.handle.destroy();
    }
  });
});

await test("a build with element on a hidden page releases each phase at once and still records its measure", async () => {
  const room = await seededRoom();
  const r1 = findBlock(room.getXmlFragment("prosemirror"), "r1");
  await withDomWindow(async (window) => {
    Object.defineProperty(window.document, "visibilityState", {
      configurable: true,
      get: () => "hidden",
    });
    const observed = await observeBuild(window, { element: r1 }, room);
    try {
      assertBoundToR1(observed.handle);
      assertPhasesMeasured(observed);
      assert(
        observed.answeredIn === 0,
        `expected a hidden page's build to wait for no timer, got task ${observed.answeredIn}`
      );
      for (const timer of WATCHED) {
        assert(
          taskOf(observed, timer) === 0,
          `expected ${timer.name} in the task that called the build, got task ${taskOf(observed, timer)}`
        );
      }
    } finally {
      await observed.handle.destroy();
    }
  });
});
