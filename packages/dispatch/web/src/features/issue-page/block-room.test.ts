import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { Awareness } from "y-protocols/awareness";
import * as Y from "yjs";

import { settle } from "../../__tests__/fake-clock";
import type { ConnectionCallbacks } from "../doc/connection";
import { createBlockRoom } from "./block-room";
import { KEY_REPORTS } from "./page-structure";

const spec = { id: "spec-id", fragment: "spec" };
const check = { id: "verification-id", fragment: "verification" };
const importSteps = ["transport", "editor", "stylesheet"];

function setup(converted = true) {
  const queries = new QueryClient({
    defaultOptions: { queries: { gcTime: Infinity, retry: false } },
  });
  const queryKeys: unknown[] = [];
  const unsubscribe = queries
    .getQueryCache()
    .subscribe(({ query }) => queryKeys.push(query.queryKey));
  const tasks: (() => void)[] = [];
  const opened: {
    id: string;
    callbacks: ConnectionCallbacks;
    doc: Y.Doc;
    awareness: Awareness;
    destroy(): void;
  }[] = [];
  const marks: string[] = [];
  const measured: { name: string; task: number }[] = [];
  const effects: { name: string; task: number }[] = [];
  const refused: string[] = [];
  const bound: string[] = [];
  const reports: string[] = [];
  const destroyed: string[] = [];
  const host = document.body.appendChild(document.createElement("div"));
  let task = 0;
  let failed: string | undefined;
  let buildGate: Promise<void> | undefined;
  function effect(name: string) {
    effects.push({ name, task });
    if (failed === name) throw new Error(`${name} failed`);
  }
  const room = createBlockRoom({
    spec,
    converted,
    queryClient: queries,
    host,
    loadTransport: async () => {
      effect("transport");
      return (id: string, callbacks: ConnectionCallbacks) => {
        effect("connect");
        const doc = new Y.Doc();
        const awareness = new Awareness(doc);
        const connection = {
          id,
          callbacks,
          doc,
          awareness,
          destroy: () => {
            awareness.destroy();
            doc.destroy();
            destroyed.push(id);
          },
        };
        opened.push(connection);
        return connection;
      };
    },
    loadEditor: async () => {
      effect("editor");
    },
    loadStyles: async () => {
      effect("stylesheet");
    },
    buildEditor: async (element: Y.XmlElement) => {
      effect("build");
      expect(element.nodeName).toBe("paragraph");
      expect(opened.every(({ doc }) => doc !== element.doc)).toBe(true);
      await buildGate;
      return {
        view: { dom: document.createElement("div") },
        destroy: () => {
          destroyed.push("editor");
        },
      };
    },
    schedule: (run: () => void) => {
      tasks.push(run);
    },
    mark: (name: string) => {
      marks.push(name);
    },
    measure: (name: string) => {
      measured.push({ name, task });
    },
    report: (words: string) => {
      reports.push(words);
    },
  });
  const drain = async () => {
    await settle();
    while (tasks.length > 0) {
      task += 1;
      tasks.shift()?.();
      await settle();
      if (task > 100) throw new Error("room did not stop scheduling work");
    }
  };
  const admission = async (readOnly = false, at = 0) => {
    const connection = opened[at];
    if (!connection) throw new Error("expected room to connect");
    connection.callbacks.onAdmission(readOnly);
    connection.callbacks.onSynced();
    await drain();
  };
  return {
    room,
    opened,
    effects,
    measured,
    marks,
    refused,
    bound,
    reports,
    destroyed,
    drain,
    admission,
    queryKeys,
    fail(name: string | undefined) {
      failed = name;
    },
    holdBuild(gate: Promise<void>) {
      buildGate = gate;
    },
    bind(id: string, artifact = spec) {
      room.requestBind(artifact, {
        onBind: () => bound.push(id),
        onBindRefused: () => refused.push(id),
      });
    },
    cleanup() {
      room.destroy();
      unsubscribe();
      queries.clear();
      host.remove();
    },
  };
}

function caseOf(
  name: string,
  run: (f: ReturnType<typeof setup>) => Promise<void>,
  converted = true
) {
  test(name, async () => {
    const f = setup(converted);
    try {
      await run(f);
    } finally {
      f.cleanup();
    }
  });
}

caseOf("each step of the work runs in a task of its own and records its measure", async (f) => {
  f.room.start();
  await f.drain();
  await f.admission();
  expect(f.effects.map(({ name }) => name)).toEqual([...importSteps, "connect", "build"]);
  expect(new Set(f.effects.map(({ task }) => task)).size).toBe(f.effects.length);
  expect(f.measured.map(({ name }) => name)).toEqual(f.effects.map(() => "block-editor-task"));
  expect(f.measured.map(({ task }) => task)).toEqual(f.effects.map(({ task }) => task));
});

caseOf("an import refusal releases the waiting act and another Enter retries", async (f) => {
  f.fail("editor");
  f.bind("first");
  await f.drain();
  expect(f.refused).toEqual(["first"]);
  expect(f.opened).toHaveLength(0);
  f.fail(undefined);
  f.bind("retry");
  await f.drain();
  await f.admission();
  expect(f.bound).toEqual(["retry"]);
});

caseOf("a socket outside the schema reports refusal and the next act retries", async (f) => {
  f.bind("first");
  await f.drain();
  f.opened[0].callbacks.onOutsideSchema();
  await f.drain();
  expect(f.refused).toEqual(["first"]);
  expect(f.reports).toEqual([KEY_REPORTS.editor_closed]);
  f.bind("retry");
  await f.drain();
  expect(f.opened).toHaveLength(2);
  await f.admission(false, 1);
  expect(f.bound).toEqual(["retry"]);
});

caseOf("acts while parts apply wait for the bind, in order", async (f) => {
  f.room.start();
  await f.drain();
  f.bind("one");
  f.bind("two");
  await f.drain();
  expect(f.bound).toEqual([]);
  await f.admission();
  expect(f.bound).toEqual(["one", "two"]);
});

caseOf("each mark is set once in a page's life", async (f) => {
  f.room.start();
  await f.drain();
  expect(f.marks).toEqual(["block-editor-start"]);
  await f.admission();
  f.room.start();
  await f.admission();
  expect(f.marks).toEqual(["block-editor-start", "block-editor-ready"]);
  expect(document.querySelector('[inert][aria-hidden="true"]')).not.toBeNull();
});

caseOf("Spec and Conversation on the same issue keep one connection", async (f) => {
  f.bind("conversation");
  await f.drain();
  await f.admission();
  f.bind("spec");
  f.bind("conversation-again");
  await f.drain();
  expect(f.bound).toEqual(["conversation", "spec", "conversation-again"]);
  expect(f.opened.map(({ id }) => id)).toEqual([spec.id]);
  expect(f.destroyed).toEqual([]);
});

caseOf("leaving the issue destroys the connection and the kept editor", async (f) => {
  f.room.start();
  await f.drain();
  await f.admission();
  f.room.destroy();
  expect(f.destroyed.sort()).toEqual(["editor", spec.id]);
});

caseOf("a read-only admission refuses a waiting edit", async (f) => {
  f.bind("waiting");
  await f.drain();
  await f.admission(true);
  expect(f.bound).toEqual([]);
  expect(f.refused).toEqual(["waiting"]);
});

caseOf("the room makes no document query while opening or binding", async (f) => {
  f.bind("waiting");
  await f.drain();
  await f.admission();
  expect(f.bound).toEqual(["waiting"]);
  expect(f.queryKeys).toEqual([]);
});

caseOf(
  "an unconverted issue opens its other document only on its first act",
  async (f) => {
    f.room.start();
    await f.drain();
    await f.admission();
    expect(f.opened.map(({ id }) => id)).toEqual([spec.id]);
    f.bind("verification", check);
    await f.drain();
    expect(f.opened.map(({ id }) => id)).toEqual([spec.id, check.id]);
    await f.admission(false, 1);
    f.bind("another-check", check);
    await f.drain();
    expect(f.bound).toEqual(["verification", "another-check"]);
    expect(f.opened.map(({ id }) => id)).toEqual([spec.id, check.id]);
  },
  false
);

caseOf(
  "the kept editor is built on a scratch document, writes nothing to the room and sets no awareness state before its first bind",
  async (f) => {
    const hold = Promise.withResolvers<void>();
    f.holdBuild(hold.promise);
    f.room.start();
    await f.drain();
    const updates: Uint8Array[] = [];
    f.opened[0].doc.on("update", (update) => updates.push(update));
    const presence = f.opened[0].awareness;
    const before = presence.getLocalState();
    const changed: unknown[] = [];
    presence.on("change", () => changed.push(presence.getLocalState()));
    await f.admission();
    expect(f.effects.at(-1)?.name).toBe("build");
    expect(f.marks).not.toContain("block-editor-ready");
    hold.resolve();
    await f.drain();
    expect(updates).toEqual([]);
    expect(presence.getLocalState()).toEqual(before);
    expect(changed).toEqual([]);
    expect(f.marks).toContain("block-editor-ready");
    expect(f.bound).toEqual([]);
  }
);
