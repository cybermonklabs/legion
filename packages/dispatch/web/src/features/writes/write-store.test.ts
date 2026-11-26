import { afterEach, beforeEach, expect, jest, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { QueryClient, QueryObserver } from "@tanstack/react-query";

import { onFakeClock, settle } from "../../__tests__/fake-clock";
import {
  type KeptTab,
  keptTab,
  keyMinted,
  type MemoryFiles,
  memoryFiles,
  UnloadTarget,
} from "../../__tests__/kept-sends-harness";
import { answerSends, type SendAnswers } from "../../__tests__/send-answers";
import {
  ApiError,
  api,
  NetworkError,
  RequestTimeoutError,
  type WriteBounds,
  type WriteRequest,
} from "../../api/client";
import { reachable, routeOf, unreachable } from "../../api/reachability";
import {
  KEYED_SEND_ATTEMPT_MS,
  OneRequestPerWriteError,
  SEND_DEADLINE_MS,
  SendExpiredError,
  SendInterruptedError,
  SendStoppedError,
  WriteDiscardedError,
} from "./keyed-write";
import { KEPT_SEND_RESUME_MS, type KeptState } from "./write-storage";
import { defineWriteKind, plainWrite, type Write, Writes } from "./write-store";

/** A page's query client, signed in: the store stamps every write with this login at its press
 *  and resumes only this login's, as the app's does. */
function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

/** Moves the clock `ms` on, letting each timer's continuation run as it fires. */
async function tick(ms: number): Promise<void> {
  for (let left = ms; left > 0; left -= 100) {
    jest.advanceTimersByTime(Math.min(100, left));
    await settle();
  }
}

/** A fetch that failed: what the client makes of a browser's `TypeError`. */
const dropped = (path = "/api/v1/issues/CORE-1") =>
  new NetworkError(path, new TypeError("Failed to fetch"));

/** One attempt that tells `sent` the request as `api.send` does on the real path, then answers
 *  with `answer()`. */
function attemptOf(
  request: WriteRequest,
  answer: () => Promise<unknown>
): (bounds: WriteBounds) => Promise<unknown> {
  return (bounds) => {
    bounds.sent?.(request);
    return answer();
  };
}

const patch: WriteRequest = {
  body: { status: "done" },
  method: "PATCH",
  path: "/api/v1/issues/CORE-1",
};

/** A pin of CORE-1: a `PUT` sends no `Idempotency-Key`, since making it again is the same
 *  change. */
const put: WriteRequest = {
  body: { dismissed: [], seq: 2 },
  method: "PUT",
  path: "/api/v1/me/state/CORE-1",
};

/** The answers of this file's writes made from their request alone: a test's own spy on
 *  `api.send` takes over from it for that test. */
let sends: SendAnswers;
beforeEach(() => {
  sends = answerSends();
});
afterEach(() => sends.restore());

/** A status pick on CORE-1, as the indicator shows it, its attempts answered by `answer`. One on a
 *  line is made from its request alone, as every plain write on a line is. */
function statusWrite(
  writes: Writes,
  answer: () => Promise<unknown>,
  fields: { target?: string; label?: string } = {}
): Promise<unknown> {
  const press = {
    detail: null,
    kind: plainWrite,
    label: fields.label ?? "CORE-1 set to done",
    presentation: "indicator",
  } as const;
  return fields.target === undefined
    ? writes.run({ ...press, method: "PATCH", write: attemptOf(patch, answer) })
    : writes.run({ ...press, request: sends.answer({ ...patch }, answer), target: fields.target });
}

/** An upload of `file` to CORE-1, as the Artifacts tab presses one. */
function uploadOf(file: File): WriteRequest {
  const body = new FormData();
  body.set("name", file.name);
  body.set("file", file);
  return { body, method: "POST", path: "/api/v1/issues/CORE-1/artifacts" };
}

/** Presses an upload of `file` in `writes`, its attempts answered by `answer`. */
function uploadWrite(writes: Writes, file: File, answer: () => Promise<unknown>): Promise<unknown> {
  return writes.run({
    detail: null,
    file,
    kind: plainWrite,
    label: `Upload of ${file.name}`,
    method: "POST",
    presentation: "indicator",
    write: attemptOf(uploadOf(file), answer),
  });
}

/** A browser's kept files whose `put` answers only when the test releases it, with the answer
 *  the test chooses: the window in which the page holds a file it has not kept yet. */
function heldFiles(): MemoryFiles & {
  /** The keys whose put is still unanswered. */
  pending: () => string[];
  /** Answers the put under `key`: `kept` false is a browser that kept nothing of it. */
  release: (key: string, kept?: boolean) => Promise<void>;
} {
  const files = memoryFiles();
  const held = new Map<string, { file: File; login: string; answer: (kept: boolean) => void }>();
  return {
    ...files,
    pending: () => [...held.keys()],
    put: (key, login, file) => {
      const { promise, resolve } = Promise.withResolvers<boolean>();
      held.set(key, { answer: resolve, file, login });
      return promise;
    },
    release: async (key, kept = true) => {
      const pending = held.get(key);
      if (pending === undefined) return;
      held.delete(key);
      if (kept) await files.put(key, pending.login, pending.file);
      pending.answer(kept);
    },
  };
}

const spec = new File(["spec body"], "spec.md", { type: "text/markdown" });
const once = new File(["spec body"], "once.bin", { type: "application/octet-stream" });

const unavailable = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** A store of a page of `tab`, signed in as `login`. */
function storeOf(tab = keptTab(), login = "alice", files = memoryFiles()) {
  return {
    client: signedIn(login),
    files,
    tab,
    writes: new Writes(signedIn(login), tab.page(), files),
  };
}

/** The write `writes` holds now, at `index` in press order. */
const row = (writes: Writes, index = 0): Write | undefined => writes.rows()[index];

/** One write on CORE-1 kept in `tab` under `key` as it stood (`state`) when the page that pressed
 *  it went away, as the request it last went out as: a status pick unless a row names another, a
 *  plain write unless it names another kind (`of`). It is kept as the release before the issue
 *  was kept wrote it, with no `issue`. */
function keptEntry(
  tab: KeptTab,
  key: string,
  state: KeptState,
  request: WriteRequest = patch,
  of: { readonly kind: string; readonly detail: unknown } = { detail: null, kind: "write" }
): void {
  tab.storage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      ...of,
      key,
      label: "CORE-1 set to done",
      login: "alice",
      presentation: "indicator",
      pressedAt: Date.now(),
      request,
      sent: true,
      state,
      target: null,
      version: 1,
    })
  );
}

/** A page that has just resumed the one write kept in `tab` under `key` (`keptEntry`). */
function resuming(
  tab: KeptTab,
  key: string,
  state: KeptState,
  request: WriteRequest = patch,
  of: { readonly kind: string; readonly detail: unknown } = { detail: null, kind: "write" }
): Writes {
  keptEntry(tab, key, state, request, of);
  const writes = new Writes(signedIn(), tab.page(), memoryFiles());
  writes.resume("alice");
  return writes;
}

// One press is one key, and every attempt of that press carries it, so however many attempts
// reach Dispatch it is one write there. Only an attempt that ended short of Dispatch's verdict is
// made again: a connection that failed, an attempt gone silent, a 5xx, a 408, a 429, and the 409
// that says Dispatch is still running the first request under this key.
test("one key per press, reused by every attempt the store makes", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const answers: (() => Promise<unknown>)[] = [
      () => Promise.reject(dropped()),
      () => Promise.reject(new RequestTimeoutError(patch.path, KEYED_SEND_ATTEMPT_MS)),
      () => Promise.reject(unavailable),
      () => Promise.reject(new ApiError(408, { error: "request timeout" })),
      () => Promise.reject(new ApiError(429, { code: "RATE_LIMITED", error: "slow down" })),
      () => Promise.reject(new ApiError(409, { code: "KEY_IN_FLIGHT", error: "still running" })),
      async () => ({ key: "CORE-1" }),
    ];
    const keys: string[] = [];
    const saved = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      method: "PATCH",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        bounds.sent?.(patch);
        return (answers.shift() ?? answers[0])?.() ?? Promise.resolve();
      },
    });
    await tick(40_000);
    expect(await saved).toEqual({ key: "CORE-1" });
    expect(keys).toHaveLength(7);
    expect(new Set(keys).size).toBe(1);
    expect(writes.rows()).toEqual([]);
  });
});

// A write of an issue tells that issue's listeners where it stands, from going out to settling.
// Its issue is its press's, or the issue of its request's path: at its press for a write made
// from its request, at its first attempt for one made by an attempt. A write naming no issue, or
// another, tells this one nothing.
test("a write of an issue tells that issue's listeners as it goes out and settles", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const told: string[] = [];
    const stop = writes.onIssueWrite("CORE-1", (phase, write) => {
      told.push(`${write.label}: ${phase}`);
    });
    const elsewhere: string[] = [];
    const stopOther = writes.onIssueWrite("CORE-2", (phase) => elsewhere.push(phase));

    // Named by its press: out at once, before its attempt starts.
    const answer = Promise.withResolvers<unknown>();
    const keep = writes.run({
      detail: null,
      issue: "CORE-1",
      kind: plainWrite,
      label: "Keep",
      method: "POST",
      presentation: "indicator",
      write: () => answer.promise,
    });
    expect(told).toEqual(["Keep: out"]);
    expect(writes.writesOutOf("CORE-1").map((write) => write.label)).toEqual(["Keep"]);
    answer.resolve({ kept: true });
    expect(await keep).toEqual({ kept: true });
    expect(told).toEqual(["Keep: out", "Keep: landed"]);
    expect(writes.writesOutOf("CORE-1")).toEqual([]);

    // Named by its first attempt's request, refused, and out again on its Retry.
    told.length = 0;
    let refuse = true;
    const refused = statusWrite(writes, () =>
      refuse ? Promise.reject(verdict) : Promise.resolve({ key: "CORE-1" })
    ).catch((error: unknown) => error);
    expect(await refused).toBe(verdict);
    expect(told).toEqual(["CORE-1 set to done: out", "CORE-1 set to done: refused"]);
    expect(row(writes)?.issue).toBe("CORE-1");
    refuse = false;
    writes.retry(row(writes)?.id ?? 0);
    await tick(1_000);
    expect(told.slice(2)).toEqual(["CORE-1 set to done: out", "CORE-1 set to done: landed"]);

    // Made from its request on a line: named at its press. One waiting its turn behind it,
    // discarded before it was sent, settles as discarded.
    told.length = 0;
    const first = Promise.withResolvers<unknown>();
    const ahead = statusWrite(writes, () => first.promise, { label: "Ahead", target: "issue" });
    const behind = statusWrite(writes, async () => null, {
      label: "Behind",
      target: "issue",
    }).catch((error: unknown) => error);
    expect(told).toEqual(["Ahead: out", "Behind: out"]);
    const waiting = writes.rows().find((write) => write.label === "Behind");
    expect(writes.discard(waiting?.id ?? 0)).toBe(true);
    expect(told).toEqual(["Ahead: out", "Behind: out", "Behind: discarded"]);
    first.resolve({ key: "CORE-1" });
    await ahead;
    await behind;
    expect(told.at(-1)).toBe("Ahead: landed");

    // A pin's request names no issue in its path; a write of another issue tells that one.
    told.length = 0;
    await writes.run({
      detail: null,
      kind: plainWrite,
      label: "Pin",
      method: "PUT",
      presentation: "indicator",
      write: attemptOf(put, async () => ({ seq: 3 })),
    });
    await writes.run({
      detail: null,
      issue: "CORE-2",
      kind: plainWrite,
      label: "Other",
      method: "POST",
      presentation: "indicator",
      write: async () => ({ key: "CORE-2" }),
    });
    expect(told).toEqual([]);
    expect(elsewhere).toEqual(["out", "landed"]);
    stop();
    stopOther();
  });
});
// Dispatch's verdict ends the write: a 4xx it answered is not made again, and the write stays for
// the reader with that refusal until Discard drops it. An error of the page's own - a bug - is
// refused at once too, never retried for good.
test("a 4xx, or an error of the page's own, refuses at once and waits for the reader", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let attempts = 0;
    const refused = statusWrite(writes, () => {
      attempts += 1;
      return Promise.reject(verdict);
    }).catch((error: unknown) => error);
    await settle();
    expect(await refused).toBe(verdict);
    const write = row(writes);
    expect(write).toMatchObject({
      label: "CORE-1 set to done",
      refusal: verdict,
      status: "refused",
    });
    await tick(60_000);
    expect(attempts).toBe(1);
    expect(writes.discard(write?.id ?? 0)).toBe(true);
    expect(writes.rows()).toEqual([]);

    const bug = new Error("bug");
    let buggy = 0;
    const failed = statusWrite(writes, () => {
      buggy += 1;
      return Promise.reject(bug);
    }).catch((error: unknown) => error);
    await tick(60_000);
    expect(await failed).toBe(bug);
    expect(buggy).toBe(1);
  });
});

// Past its deadline a write is late and the reader may stop it: no attempt follows the one in
// flight, it ends with `SendStoppedError`, and Retry asks again under the same key. A Retry after
// Dispatch's own refusal goes under a new key, since Dispatch replays the refusal it stored.
test("Stop trying ends a late write, and Retry keeps its key until Dispatch has refused", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const keys: string[] = [];
    let answer: () => Promise<unknown> = () => Promise.reject(unavailable);
    const stopped = writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to done",
        method: "PATCH",
        presentation: "indicator",
        write: (bounds) => {
          keys.push(bounds.key);
          bounds.sent?.(patch);
          return answer();
        },
      })
      .catch((error: unknown) => error);
    await tick(SEND_DEADLINE_MS + 1_000);
    const late = row(writes);
    expect(late?.status).toBe("late");
    const before = keys.length;
    expect(writes.stop(late?.id ?? 0)).toBe(true);
    await tick(30 * 60_000);
    expect(await stopped).toBeInstanceOf(SendStoppedError);
    expect(keys).toHaveLength(before);
    expect(row(writes)?.status).toBe("refused");

    answer = () => Promise.reject(verdict);
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    expect(keys.at(-1)).toBe(keys[0]);

    answer = async () => ({ key: "CORE-1" });
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    expect(keys.at(-1)).not.toBe(keys[0]);
    expect(writes.rows()).toEqual([]);
  });
});

// A write is kept from its press, so a reload takes it up again under its key - which Dispatch
// answers with what the first did - and makes it once. One pressed longer ago than five minutes is
// not made again on its own: it comes back refused, for Retry under its key or Discard. A PUT and a
// DELETE are kept and resumed like any write (W29).
test("a write survives a reload and resumes once, PUT included, and an old one comes back refused", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    const put: WriteRequest = {
      body: { until: "2026-11-01T00:00:00Z" },
      method: "PUT",
      path: "/api/v1/asks/ask-1/snooze",
    };
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      const hang = () => Promise.withResolvers<never>().promise;
      void statusWrite(before, hang).catch(() => {});
      void before
        .run({
          detail: null,
          kind: plainWrite,
          label: "Ask snoozed",
          method: "PUT",
          presentation: "indicator",
          write: attemptOf(put, hang),
        })
        .catch(() => {});
      await settle();
      const [patchKey, putKey] = before.rows().map((write) => write.key);
      expect(tab.kept(patchKey ?? "")).toMatchObject({
        kind: "write",
        login: "alice",
        state: { status: "sending" },
      });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      const resent: unknown[] = send.mock.calls.map((call) => [
        call[0],
        (call[1] as WriteBounds).key,
      ]);
      expect(resent).toEqual([
        [patch, patchKey],
        [put, putKey],
      ]);
      expect(after.rows()).toEqual([]);
      expect(tab.kept(patchKey ?? "")).toBeUndefined();

      const old = new Writes(signedIn(), tab.page(), memoryFiles());
      void statusWrite(old, hang).catch(() => {});
      await settle();
      const oldKey = row(old)?.key ?? "";
      const entry = tab.kept(oldKey) as { pressedAt: number };
      tab.storage.setItem(
        `dispatch.write:${oldKey}`,
        JSON.stringify({ ...entry, pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1 })
      );
      const later = new Writes(signedIn(), tab.page(), memoryFiles());
      later.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(2);
      const resumed = row(later);
      expect(resumed).toMatchObject({ label: "CORE-1 set to done", status: "refused" });
      expect(resumed?.status === "refused" ? resumed.refusal : undefined).toBeInstanceOf(
        SendInterruptedError
      );
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// A write keeps the issue its caller named, so one resumed after a reload is a write of that
// issue again, though its request's path - a Keep's, under the document - names none. An entry
// kept before the issue was kept holds none, and resumes naming the issue of its request's path.
test("a resumed write names the issue it was kept with, or else its request's path", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ kept: true } as never);
    const keep: WriteRequest = {
      body: { text_sha256: "sha" },
      method: "POST",
      path: "/api/v1/artifacts/artifact-1/blocks/block-1/keep",
    };
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      void before
        .run({
          detail: null,
          issue: "CORE-1",
          kind: plainWrite,
          label: "Keep",
          method: "POST",
          presentation: "indicator",
          write: attemptOf(keep, () => Promise.withResolvers<never>().promise),
        })
        .catch(() => {});
      await settle();
      const key = row(before)?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ issue: "CORE-1" });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      const told: string[] = [];
      const stop = after.onIssueWrite("CORE-1", (phase, write) =>
        told.push(`${write.label}: ${phase}`)
      );
      after.resume("alice");
      await settle();
      stop();
      expect(told).toEqual(["Keep: out", "Keep: landed"]);

      keptEntry(tab, "unnamed", { status: "sending" });
      const older = new Writes(signedIn(), tab.page(), memoryFiles());
      const toldOlder: string[] = [];
      const stopOlder = older.onIssueWrite("CORE-1", (phase, write) =>
        toldOlder.push(`${write.label}: ${phase}`)
      );
      older.resume("alice");
      await settle();
      stopOlder();
      expect(toldOlder).toEqual(["CORE-1 set to done: out", "CORE-1 set to done: landed"]);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// A write pressed in this page before the app takes up what a reload kept is kept in the tab from
// its press, so the take-up finds it there too: it is this page's write, already in its line, and
// is not taken up a second time beside itself. The app's resume runs in the shell's effect, which
// React runs after every effect of the shell's children, so a child's write on mount comes first.
test("a write pressed before the take-up of a reload's writes is not taken up beside itself", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const writes = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      const hang = () => Promise.withResolvers<never>().promise;
      void statusWrite(writes, hang).catch(() => {});
      await settle();
      const key = row(writes)?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ login: "alice", state: { status: "sending" } });

      writes.resume("alice");
      await settle();
      expect(writes.rows().map((write) => write.key)).toEqual([key]);
      expect(tab.kept(key)).toMatchObject({ login: "alice" });
    } finally {
      tab.close();
    }
  });
});

// W24. An upload's file is kept apart from its request, under the write's key, so a reload makes
// the upload again with the same file; it goes once the upload lands.
test("an upload's file survives a reload and the upload resumes with it", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = memoryFiles();
    const send = spyOn(api, "send").mockResolvedValue({ artifact: {}, version: {} } as never);
    const before = new Writes(signedIn(), tab.page(), files);
    try {
      void uploadWrite(before, spec, () => Promise.withResolvers<never>().promise).catch(() => {});
      await settle();
      const key = row(before)?.key ?? "";
      expect(files.rows.get(key)).toBe(spec);
      expect(tab.kept(key)).toMatchObject({
        request: {
          form: [
            { name: "name", value: "spec.md" },
            { file: true, name: "file" },
          ],
          method: "POST",
        },
      });

      const after = new Writes(signedIn(), tab.page(), files);
      after.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      const [resent, bounds] = send.mock.calls[0] ?? [];
      expect(bounds?.key).toBe(key);
      const body = resent?.body as FormData;
      expect(body.get("name")).toBe("spec.md");
      expect((body.get("file") as File).name).toBe("spec.md");
      expect(after.rows()).toEqual([]);
      expect(files.rows.has(key)).toBe(false);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// W24. A Retry Dispatch's refusal sends under a new key takes the upload's file with it: the file
// is kept under the key its attempt carries, so one row is left, under the new key, and a reload
// makes the upload again from it.
test("a retried upload's file moves to its new key, and a reload resumes it there", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = memoryFiles();
    const send = spyOn(api, "send").mockResolvedValue({ artifact: {}, version: {} } as never);
    const before = new Writes(signedIn(), tab.page(), files);
    try {
      let answer: () => Promise<unknown> = () => Promise.reject(verdict);
      void uploadWrite(before, spec, () => answer()).catch(() => {});
      await settle();
      const refused = row(before);
      expect(refused?.status).toBe("refused");

      answer = () => Promise.withResolvers<never>().promise;
      before.retry(refused?.id ?? 0);
      await settle();
      const key = row(before)?.key ?? "";
      expect(key).not.toBe(refused?.key);
      expect([...files.rows.keys()]).toEqual([key]);

      // With no file under the new key the resumed upload has no request to make (`requestFrom`)
      // and no attempt: that this one goes out under that key is the file found there.
      const after = new Writes(signedIn(), tab.page(), files);
      after.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect((send.mock.calls[0]?.[1] as WriteBounds).key).toBe(key);
      expect(after.rows()).toEqual([]);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// W25. The page never keeps, and never sends, anything that points at an upload's file before
// that file is kept: a record naming a key IndexedDB holds no file under is a write a reload
// drops with nothing shown and nothing sent. The press still joins at once - the indicator
// reads `Saving 1 change…` from it, and a sign-out still reaches it - and its deadline starts
// with its attempt, so a slow keep never makes it late before it has been sent.
test("an upload is neither kept nor sent until its file is kept", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = heldFiles();
    const writes = new Writes(signedIn(), tab.page(), files);
    try {
      let attempts = 0;
      void uploadWrite(writes, once, () => {
        attempts += 1;
        return Promise.withResolvers<never>().promise;
      }).catch(() => {});
      await settle();
      const key = row(writes)?.key ?? "";
      expect(row(writes)?.status).toBe("sending");
      await tick(SEND_DEADLINE_MS + 1_000);
      expect(attempts).toBe(0);
      expect(row(writes)?.status).toBe("sending");
      expect(tab.kept(key)).toBeUndefined();

      await files.release(key);
      await settle();
      expect(attempts).toBe(1);
      expect(files.rows.get(key)).toBe(once);
      expect(tab.kept(key)).toMatchObject({ key, sent: true, state: { status: "sending" } });
    } finally {
      tab.close();
    }
  });
});

// W25. A Retry Dispatch's verdict sends under a new key waits the same way: the file is put
// under the new key first, and until it is there the write stands refused under its old key
// with its old file, so a reload inside that window brings it back for the reader to retry
// again. Nothing names the new key before the file is under it, and the old row goes only
// once it does.
test("a retried upload keeps its old key and file until the new key's file is kept", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = heldFiles();
    const writes = new Writes(signedIn(), tab.page(), files);
    try {
      let attempts = 0;
      void uploadWrite(writes, once, () => {
        attempts += 1;
        return Promise.reject(verdict);
      }).catch(() => {});
      await settle();
      const old = row(writes)?.key ?? "";
      await files.release(old);
      await settle();
      const refused = row(writes);
      expect(refused?.status).toBe("refused");
      expect(attempts).toBe(1);

      writes.retry(refused?.id ?? 0);
      await settle();
      expect(row(writes)?.status).toBe("refused");
      expect(row(writes)?.key).toBe(old);
      expect(tab.kept(old)).toMatchObject({ state: { status: "refused" } });
      expect([...files.rows.keys()]).toEqual([old]);
      expect(attempts).toBe(1);

      // Nothing but the put itself knows the new key while the write stands under its old one.
      const next = files.pending()[0] ?? "";
      expect(next).not.toBe(old);
      expect(tab.kept(next)).toBeUndefined();

      // The key moves, the old record and the old file go, and the attempt follows, all in the
      // tick the put answers in.
      await files.release(next);
      await settle();
      expect(row(writes)?.key).toBe(next);
      expect(attempts).toBe(2);
      expect(tab.kept(old)).toBeUndefined();
      expect(tab.kept(next)).toMatchObject({ key: next });
      expect([...files.rows.keys()]).toEqual([next]);
    } finally {
      tab.close();
    }
  });
});

// W25. A reload inside that window loses nothing: the page the reader comes back to reads the
// refused write under its old key, with its file, and offers Retry again. Before this ordering
// the record named the new key while no file was under it yet, so the page after the reload had
// nothing to make the upload from and dropped it with nothing shown and nothing sent.
test("a reload inside a Retry's keep brings the refused upload back with its file", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = heldFiles();
    const before = new Writes(signedIn(), tab.page(), files);
    try {
      void uploadWrite(before, once, () => Promise.reject(verdict)).catch(() => {});
      await settle();
      const old = row(before)?.key ?? "";
      await files.release(old);
      await settle();
      before.retry(row(before)?.id ?? 0);
      await settle();

      const after = new Writes(signedIn(), tab.page(), files);
      after.resume("alice");
      await settle();
      const back = row(after);
      expect(back?.status).toBe("refused");
      expect(back?.key).toBe(old);
      expect(files.rows.get(old)).toBe(once);
    } finally {
      tab.close();
    }
  });
});

// W25. A browser that keeps nothing - one with no IndexedDB, or one that refuses to open it for
// a quota or a storage policy - says so, and the page then keeps no record of the upload at all
// rather than one naming a file that is not there: the upload goes out and lands as it always
// did, leaving is still confirmed while it is out, and a reload finds nothing to resume. That
// last is the one loss this leaves, and it is the loss a browser with no IndexedDB always had.
test("an upload whose file is not kept is sent, kept nowhere, and still confirmed on leaving", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = heldFiles();
    const unload = new UnloadTarget();
    const writes = new Writes(signedIn(), tab.page(unload), files);
    try {
      let attempts = 0;
      void uploadWrite(writes, once, () => {
        attempts += 1;
        return Promise.withResolvers<never>().promise;
      }).catch(() => {});
      await settle();
      const key = row(writes)?.key ?? "";

      await files.release(key, false);
      await settle();
      expect(attempts).toBe(1);
      expect(row(writes)?.status).toBe("sending");
      expect(tab.kept(key)).toBeUndefined();
      expect(files.rows.has(key)).toBe(false);
      expect(unload.asks()).toBe(true);

      const after = new Writes(signedIn(), tab.page(), files);
      after.resume("alice");
      await settle();
      expect(after.rows()).toEqual([]);
    } finally {
      tab.close();
    }
  });
});

// W25. The same answer at a Retry: the key moves and the old record and old file go, as they do
// when the file is kept, and no record is written under the new key, so nothing of the page ever
// names a file this browser did not keep.
test("a retried upload whose file is not kept moves its key and is kept nowhere", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const files = heldFiles();
    const writes = new Writes(signedIn(), tab.page(), files);
    try {
      let attempts = 0;
      void uploadWrite(writes, once, () => {
        attempts += 1;
        return Promise.reject(verdict);
      }).catch(() => {});
      await settle();
      const old = row(writes)?.key ?? "";
      await files.release(old);
      await settle();
      writes.retry(row(writes)?.id ?? 0);
      await settle();

      const next = files.pending()[0] ?? "";
      await files.release(next, false);
      await settle();
      expect(row(writes)?.key).toBe(next);
      expect(attempts).toBe(2);
      expect(tab.kept(old)).toBeUndefined();
      expect(tab.kept(next)).toBeUndefined();
      expect(files.rows.has(old)).toBe(false);
      expect(files.rows.has(next)).toBe(false);
    } finally {
      tab.close();
    }
  });
});

// W26. A write belongs to the person who pressed it: another login's kept write is dropped unmade
// on load, and once someone else is signed in, every write of another login stops and goes - in
// memory and in storage - and its press is refused.
test("a write of another login is dropped, kept or in flight", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({} as never);
    const alice = new Writes(signedIn("alice"), tab.page(), memoryFiles());
    try {
      let attempts = 0;
      const pressed = statusWrite(alice, () => {
        attempts += 1;
        return Promise.reject(dropped());
      }).catch((error: unknown) => error);
      await settle();
      const key = row(alice)?.key ?? "";
      expect(row(alice)?.login).toBe("alice");

      const bob = new Writes(signedIn("bob"), tab.page(), memoryFiles());
      bob.resume("bob");
      await settle();
      expect(send).not.toHaveBeenCalled();
      expect(tab.kept(key)).toBeUndefined();

      alice.signedIn("bob");
      expect(await pressed).toBeInstanceOf(SendStoppedError);
      expect(alice.rows()).toEqual([]);
      const after = attempts;
      await tick(60_000);
      expect(attempts).toBe(after);
      expect(tab.kept(key)).toBeUndefined();
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

/** A composer's refused send, as far as the store is concerned: a write whose record is the
 *  reader's draft, kept with it. */
const draftWrite = defineWriteKind<{ readonly draft: string }>({
  kept: (value): value is { readonly draft: string } =>
    typeof value === "object" && value !== null && typeof Reflect.get(value, "draft") === "string",
  name: "test-draft",
});

/** Alice's writes when her session lapses: one being tried (an upload, its file kept), and one
 *  Dispatch refused, holding her draft. */
async function lapsedWrites() {
  const tab = keptTab();
  const files = memoryFiles();
  const writes = new Writes(signedIn("alice"), tab.page(), files);
  const file = new File(["spec body"], "spec.md", { type: "text/markdown" });
  let up = false;
  let attempts = 0;
  const out = writes
    .run({
      detail: null,
      file,
      kind: plainWrite,
      label: "Upload of spec.md",
      method: "PATCH",
      presentation: "indicator",
      write: attemptOf(patch, () => {
        attempts += 1;
        return up ? Promise.resolve({ ok: true }) : Promise.reject(dropped());
      }),
    })
    .catch((error: unknown) => error);
  void writes
    .run({
      detail: { draft: "My only copy of this" },
      kind: draftWrite,
      label: "Message",
      method: "PATCH",
      presentation: "composer",
      write: attemptOf(patch, () => Promise.reject(verdict)),
    })
    .catch(() => {});
  await settle();
  const [upload, draft] = writes.rows();
  writes.signedIn(undefined);
  await settle();
  return {
    attempts: () => attempts,
    draftKey: draft?.key ?? "",
    files,
    out,
    tab,
    up: () => {
      up = true;
    },
    uploadKey: upload?.key ?? "",
    writes,
  };
}

// W32. A session that lapses - whoami answers 401 - keeps the person's own writes: every one out
// stops trying and nothing more is sent, but each stays, in memory and in storage, a refused
// one's draft and an upload's file with it. Once the same person signs in again they go on: the
// one out is made again under its key, and the refusal is still there for Retry or Discard.
test("a lapsed session keeps its writes, and the same login's sign-in resumes them", async () => {
  await onFakeClock(async () => {
    const lapsed = await lapsedWrites();
    const { files, tab, writes } = lapsed;
    try {
      expect(writes.rows().map((write) => write.status)).toEqual(["queued", "refused"]);
      expect(tab.kept(lapsed.uploadKey)).toBeDefined();
      expect(tab.kept(lapsed.draftKey)).toMatchObject({
        detail: { draft: "My only copy of this" },
      });
      expect(files.rows.has(lapsed.uploadKey)).toBe(true);
      const stopped = lapsed.attempts();
      await tick(60_000);
      expect(lapsed.attempts()).toBe(stopped);

      lapsed.up();
      writes.signedIn("alice");
      await settle();
      expect(await lapsed.out).toEqual({ ok: true });
      expect(lapsed.attempts()).toBe(stopped + 1);
      expect(writes.rows()).toMatchObject([
        { detail: { draft: "My only copy of this" }, status: "refused" },
      ]);
      expect(tab.kept(lapsed.draftKey)).toMatchObject({
        detail: { draft: "My only copy of this" },
      });
      expect(files.rows.has(lapsed.uploadKey)).toBe(false);
    } finally {
      tab.close();
    }
  });
});

// W32. Only someone else's sign-in after the lapse drops them: then they go, in memory and in
// storage, files included, and the press is refused.
test("a lapsed session's writes go once another login signs in", async () => {
  await onFakeClock(async () => {
    const lapsed = await lapsedWrites();
    const { files, tab, writes } = lapsed;
    try {
      writes.signedIn("bob");
      await settle();
      expect(await lapsed.out).toBeInstanceOf(SendStoppedError);
      expect(writes.rows()).toEqual([]);
      expect(tab.kept(lapsed.uploadKey)).toBeUndefined();
      expect(tab.kept(lapsed.draftKey)).toBeUndefined();
      expect(files.rows.has(lapsed.uploadKey)).toBe(false);
    } finally {
      tab.close();
    }
  });
});

// W32. Dispatch answers a write whose session lapsed `401`, and nothing ran (the key is untouched):
// the write is not refused - it is asked again under its key - and the page reads who is signed in
// again, so a lapse it learns of this way holds every write until the sign-in that follows.
test("a write answered 401 is asked again under its key, and whoami is read again", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    let answers = 0;
    const keys: string[] = [];
    const landed = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to done",
      method: "PATCH",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        answers += 1;
        return answers === 1
          ? Promise.reject(new ApiError(401, { error: "unauthorized" }))
          : Promise.resolve({ key: "CORE-1" });
      },
    });
    await settle();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["whoami"] });
    expect(row(writes)?.status).toBe("sending");
    await tick(1_000);
    expect(await landed).toEqual({ key: "CORE-1" });
    expect(new Set(keys).size).toBe(1);
  });
});

// A write Dispatch keeps answering 401 while the page's sign-in check, read again each time, still
// names the person who pressed it is no lapse: it is Dispatch's refusal, and the surface is told
// it, rather than waiting on a sign-in that already holds. A 401 is asked again once after the
// check says the person is signed in (a session that rotated between the two), and the second is
// the verdict.
test("a write answered 401 again while its person is still signed in is refused", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const whoami = new QueryObserver(client, {
      queryFn: async () => ({ event_head: "0", kind: "user" as const, login: "alice" }),
      queryKey: ["whoami"],
    });
    const stop = whoami.subscribe(() => {});
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    try {
      let attempts = 0;
      const refused = statusWrite(writes, () => {
        attempts += 1;
        return Promise.reject(
          new ApiError(401, { code: "UNAUTHENTICATED", error: "sign in again" })
        );
      }).catch((error: unknown) => error);
      await tick(5_000);
      expect(await refused).toMatchObject({ status: 401 });
      expect(attempts).toBe(2);
      expect(row(writes)).toMatchObject({ status: "refused" });
      await tick(30_000);
      expect(attempts).toBe(2);
    } finally {
      stop();
    }
  });
});

// Two writes answered 401 at once, while the sign-in check the first one asked for is still out:
// the second's ask cancels that check and starts its own (`askSignInAgain`). The first waits for
// that newer check's answer rather than reading the cancelled check as no answer, so each write is
// Dispatch's refusal at its second 401, as a write alone is.
test("two writes answered 401 at once while the sign-in check is out are each refused at their second 401", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const whoami = new QueryObserver(client, {
      // The check answers a while after it is asked, on the held clock, so the second write's ask
      // comes while the first write's check is still out.
      queryFn: () => {
        const check = Promise.withResolvers<{ event_head: string; kind: "user"; login: string }>();
        setTimeout(() => check.resolve({ event_head: "0", kind: "user", login: "alice" }), 200);
        return check.promise;
      },
      queryKey: ["whoami"],
      staleTime: Number.POSITIVE_INFINITY,
    });
    const stop = whoami.subscribe(() => {});
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    try {
      const attempts = { first: 0, second: 0 };
      const refusedWrite = (which: "first" | "second") =>
        statusWrite(
          writes,
          () => {
            attempts[which] += 1;
            return Promise.reject(
              new ApiError(401, { code: "UNAUTHENTICATED", error: "sign in again" })
            );
          },
          { label: `The ${which} write` }
        ).catch((error: unknown) => error);
      const first = refusedWrite("first");
      const second = refusedWrite("second");
      await tick(10_000);
      expect(attempts).toEqual({ first: 2, second: 2 });
      expect(await first).toMatchObject({ status: 401 });
      expect(await second).toMatchObject({ status: 401 });
    } finally {
      stop();
    }
  });
});

// W32, W50. A sign-out still out when the session lapses has done what it was for: it lands, with
// no answer, rather than waiting to sign the person out again the moment they sign back in, or
// being refused and leaving its surface to keep what the person chose to discard.
test("a sign-out still out lands when the session lapses", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let attempts = 0;
    const signOut = writes
      .run({
        detail: null,
        endsWithSession: true,
        inline: true,
        kind: plainWrite,
        label: "Sign out",
        method: "POST",
        presentation: "indicator",
        write: () => {
          attempts += 1;
          return Promise.reject(dropped("/auth/logout"));
        },
      })
      .catch((error: unknown) => error);
    await settle();
    writes.signedIn(undefined);
    expect(await signOut).toBeNull();
    expect(writes.rows()).toEqual([]);
    writes.signedIn("alice");
    await tick(5_000);
    expect(attempts).toBe(1);
  });
});

// Dispatch refuses a write whose caller is not the person who pressed it (`Dispatch-Login`): the
// store drops every write of that login, and has the page read who is signed in again.
test("LOGIN_CHANGED drops every write of its login and reads whoami again", async () => {
  await onFakeClock(async () => {
    const client = signedIn("alice");
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    let sentLogin: string | undefined;
    void statusWrite(writes, () => Promise.withResolvers<never>().promise, {
      target: "issue:CORE-2",
    }).catch(() => {});
    const refused = writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to done",
        method: "PATCH",
        presentation: "indicator",
        write: (bounds) => {
          sentLogin = bounds.login;
          return Promise.reject(
            new ApiError(409, { code: "LOGIN_CHANGED", error: "signed in as bob" })
          );
        },
      })
      .catch((error: unknown) => error);
    await settle();
    expect(sentLogin).toBe("alice");
    expect(await refused).toBeInstanceOf(ApiError);
    expect(writes.rows()).toEqual([]);
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["whoami"] });
  });
});

// W42. Dropping writes never starts another: a sign-out (`clear`), a sign-out in another tab
// (`dropLogin`), someone else's sign-in (`signedIn`) and Dispatch's `409 LOGIN_CHANGED` each take
// every write they drop out of the store before any line moves on. So a write waiting its turn
// behind one of them - behind one out, or one a lapse holds - goes with it and is never sent.
test("dropping a line of writes starts none of the writes it drops", async () => {
  await onFakeClock(async () => {
    const loginChanged = new ApiError(409, { code: "LOGIN_CHANGED", error: "signed in as bob" });
    const paths: Record<string, (writes: Writes, refuse: (error: Error) => void) => void> = {
      clear: (writes) => writes.clear(),
      dropLogin: (writes) => writes.dropLogin("alice"),
      loginChanged: (_writes, refuse) => refuse(loginChanged),
      signedIn: (writes) => writes.signedIn("bob"),
    };
    const started: string[] = [];
    for (const held of [false, true]) {
      for (const [name, path] of Object.entries(paths)) {
        const { writes } = storeOf();
        const first = Promise.withResolvers<never>();
        void statusWrite(writes, () => first.promise, { target: "issue:CORE-1" }).catch(() => {});
        const second = statusWrite(
          writes,
          async () => {
            started.push(`${name}${held ? " (held)" : ""}`);
            return {};
          },
          { label: "second", target: "issue:CORE-1" }
        ).catch((error: unknown) => error);
        await settle();
        expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"]);
        if (held) writes.signedIn(undefined);
        path(writes, first.reject);
        await tick(60_000);
        expect(await second).toBeInstanceOf(SendStoppedError);
        expect(writes.rows()).toEqual([]);
      }
    }
    expect(started).toEqual([]);
  });
});

/** A kind that remembers something of the person signed in, and what the store tells it while a
 *  row listens (`told`), in order: each store made, each write it is to undo, each moment it is
 *  to let go. */
let told: unknown[][] | undefined;
const rememberingWrite = defineWriteKind<null>({
  kept: (value): value is null => value === null,
  name: "test-remembering",
  opened: (client, writes) => {
    told?.push(["opened", client, writes]);
  },
  reset: () => {
    told?.push(["reset"]);
  },
  undone: () => {
    told?.push(["undone"]);
  },
});

// What a kind keeps of a client for the client's whole life begins with the client's store, not
// with the first write of the kind: every kind is told once of each store as it is made.
test("a store tells each kind of itself and its client once, as it is made", () => {
  const tab = keptTab();
  told = [];
  try {
    const client = signedIn();
    const writes = new Writes(client, tab.page(), memoryFiles());
    expect(told).toHaveLength(1);
    const [what, of, store] = told[0] ?? [];
    expect(what).toBe("opened");
    expect(of).toBe(client);
    expect(store).toBe(writes);
  } finally {
    told = undefined;
    tab.close();
  }
});

// A kind undoes a dropped write from what it remembers of its person's answers, so the store
// drops the writes a change of who is signed in takes with it before it has the kinds let go of
// that: another person's sign-in, and a lapse that is the person's sign-out in another tab
// landing (`dropLogin`).
test("a change of who is signed in undoes the writes it drops before the kinds let go of what they remembered", async () => {
  await onFakeClock(async () => {
    const changes: Record<string, (writes: Writes) => void> = {
      "another person's sign-in": (writes) => writes.signedIn("bob"),
      "a lapse that lands a sign-out in another tab": (writes) => {
        writes.holdFor("alice");
        writes.signedIn(undefined);
      },
    };
    try {
      for (const [name, change] of Object.entries(changes)) {
        const { writes } = storeOf();
        writes.signedIn("alice");
        void writes
          .run({
            detail: null,
            kind: rememberingWrite,
            label: "CORE-1 remembered",
            presentation: "indicator",
            method: "POST",
            write: () => Promise.withResolvers<never>().promise,
          })
          .catch(() => {});
        await settle();
        told = [];
        change(writes);
        expect([name, ...told.flat()]).toEqual([name, "undone", "reset"]);
        expect(writes.rows()).toEqual([]);
      }
    } finally {
      told = undefined;
    }
  });
});

// A sign-out that lands is told, with the person it signed out, once their writes are gone,
// whichever way it lands: this page's (`signedOut`), another tab's (`dropLogin`), and a lapse that
// is another tab's sign-out landing - one still holding the writes, or one whose tab went just
// before it (`releaseUnsettled`). The page forgets the drafts the person typed on that word
// (`app.tsx`). A plain lapse, another person's sign-in and the preload's `clear` land none, and
// tell no one.
test("a landed sign-out tells whom it signed out, and a lapse or another person's sign-in tells no one", async () => {
  await onFakeClock(async () => {
    const ways: Record<string, (writes: Writes) => void> = {
      "this page's sign-out": (writes) => writes.signedOut("alice"),
      "another tab's sign-out": (writes) => writes.dropLogin("alice"),
      "a lapse under another tab's sign-out": (writes) => {
        writes.holdFor("alice");
        writes.signedIn(undefined);
      },
      "a lapse just after a gone tab's sign-out": (writes) => {
        writes.holdFor("alice");
        writes.releaseUnsettled("alice");
        writes.signedIn(undefined);
      },
      "a lapse": (writes) => writes.signedIn(undefined),
      "another person's sign-in": (writes) => writes.signedIn("bob"),
      "the preload's clear": (writes) => writes.clear(),
    };
    const heard: string[] = [];
    for (const [name, way] of Object.entries(ways)) {
      const { writes } = storeOf();
      writes.signedIn("alice");
      void statusWrite(writes, () => Promise.withResolvers<never>().promise).catch(() => {});
      await settle();
      const stop = writes.onSignedOut((login) => {
        heard.push(`${name}: ${login}, ${writes.rows().length} writes left`);
      });
      way(writes);
      stop();
    }
    expect(heard).toEqual([
      "this page's sign-out: alice, 0 writes left",
      "another tab's sign-out: alice, 0 writes left",
      "a lapse under another tab's sign-out: alice, 0 writes left",
      "a lapse just after a gone tab's sign-out: alice, 0 writes left",
    ]);
  });
});

// W48. A sign-out discards nothing until Dispatch answers it, and sends nothing of its person's
// meanwhile: while it is out every other write of theirs stops trying after the attempt in flight
// and none starts. Refused, it lets them go on, each where it stood; landed, they stay held until
// the page lets them go (`clear`). Another tab's sign-out holds them the same way (`holdFor`) until
// that tab says how it ended: the person still signed in (`release`), or signed out (`dropLogin`).
test("a sign-out out holds its person's writes until Dispatch answers it", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let attempts = 0;
    const failing = () => {
      attempts += 1;
      return Promise.reject(dropped());
    };
    void statusWrite(writes, failing).catch(() => {});
    await settle();
    const signOut = (answer: Promise<unknown>) =>
      writes
        .run({
          detail: null,
          endsWithSession: true,
          inline: true,
          kind: plainWrite,
          label: "Sign out",
          method: "POST",
          presentation: "indicator",
          write: () => answer,
        })
        .catch((error: unknown) => error);

    const refused = Promise.withResolvers<unknown>();
    const first = signOut(refused.promise);
    await settle();
    const held = attempts;
    await tick(10_000);
    expect(attempts).toBe(held);
    expect(writes.rows().map((write) => write.label)).toEqual(["CORE-1 set to done", "Sign out"]);
    refused.reject(new ApiError(403, { code: "FORBIDDEN", error: "not now" }));
    expect(await first).toBeInstanceOf(ApiError);
    await tick(2_000);
    expect(attempts).toBeGreaterThan(held);

    writes.holdFor("alice");
    await tick(1_000);
    const elsewhere = attempts;
    await tick(10_000);
    expect(attempts).toBe(elsewhere);
    writes.release("alice");
    await tick(2_000);
    expect(attempts).toBeGreaterThan(elsewhere);

    const landed = Promise.withResolvers<unknown>();
    const last = signOut(landed.promise);
    await tick(1_000);
    const signedOut = attempts;
    landed.resolve({ ok: true });
    expect(await last).toEqual({ ok: true });
    await tick(10_000);
    expect(attempts).toBe(signedOut);
    expect(writes.rows().map((write) => write.label)).toEqual(["CORE-1 set to done"]);
    writes.clear();
    expect(writes.rows()).toEqual([]);
  });
});
// A hold let go because its tab went, while Dispatch still named the person
// (`releaseUnsettled`), is not the end of that sign-out: the gone tab's own request may still
// reach Dispatch after this page's read did. A lapse within `KEYED_SEND_ATTEMPT_MS` of that
// release is that sign-out landing late, and the writes it would discard go; a lapse after it is
// a lapse, and keeps them, as does one after a hold its tab said the end of (`release`).
test("a lapse just after a gone tab's hold was let go discards; a later one keeps", async () => {
  await onFakeClock(async () => {
    const labels = (writes: Writes) => writes.rows().map((write) => write.label);
    const held = async (letGo: (writes: Writes) => void, wait: number) => {
      const { writes } = storeOf();
      writes.signedIn("alice");
      void statusWrite(writes, () => Promise.reject(dropped())).catch(() => {});
      await settle();
      writes.holdFor("alice");
      letGo(writes);
      await tick(wait);
      writes.signedIn(undefined);
      await settle();
      return labels(writes);
    };

    expect(await held((writes) => writes.releaseUnsettled("alice"), 1_000)).toEqual([]);
    expect(
      await held((writes) => writes.releaseUnsettled("alice"), KEYED_SEND_ATTEMPT_MS + 1_000)
    ).toEqual(["CORE-1 set to done"]);
    expect(await held((writes) => writes.release("alice"), 1_000)).toEqual(["CORE-1 set to done"]);
  });
});

// W25. Writes to one target go out in press order: a later one waits while an earlier one is still
// being tried, a refused one no longer holds the line, and its Retry joins at the back.
test("writes to one target go in press order, and a refused one does not hold the line", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const made: string[] = [];
    const first = Promise.withResolvers<unknown>();
    const press = (label: string, answer: () => Promise<unknown>) =>
      statusWrite(
        writes,
        () => {
          made.push(label);
          return answer();
        },
        { label, target: "session:planner" }
      ).catch(() => {});
    void press("first", () => first.promise);
    void press("second", async () => ({}));
    await settle();
    expect(made).toEqual(["first"]);
    expect(writes.rows().map((write) => write.status)).toEqual(["sending", "queued"]);

    first.reject(verdict);
    await settle();
    expect(made).toEqual(["first", "second"]);
    expect(writes.rows().map((write) => [write.label, write.status])).toEqual([
      ["first", "refused"],
    ]);

    void press("third", () => Promise.withResolvers<never>().promise);
    await settle();
    writes.retry(row(writes)?.id ?? 0);
    await settle();
    // The retry joined behind the third, still out.
    expect(made).toEqual(["first", "second", "third"]);
    expect(writes.rows().map((write) => [write.label, write.status])).toEqual([
      ["first", "queued"],
      ["third", "sending"],
    ]);
  });
});

// A write the reader never sees - a read mark - is tried and kept like any other, its refusal is
// never shown, and it is let go once five minutes pass without Dispatch confirming it.
test("a quiet write is never shown refused and is let go after five minutes", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    const quiet = (answer: () => Promise<unknown>) =>
      writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "Read mark",
          method: "PATCH",
          presentation: "quiet",
          write: attemptOf(patch, answer),
        })
        .catch((error: unknown) => error);
    const refused = quiet(() => Promise.reject(verdict));
    await settle();
    expect(await refused).toBe(verdict);
    expect(writes.rows()).toEqual([]);

    const unconfirmed = quiet(() => Promise.reject(dropped()));
    await settle();
    const key = row(writes)?.key ?? "";
    expect(tab.kept(key)).toMatchObject({ presentation: "quiet" });
    await tick(KEPT_SEND_RESUME_MS + 1_000);
    expect(await unconfirmed).toBeInstanceOf(SendStoppedError);
    expect(writes.rows()).toEqual([]);
    expect(tab.kept(key)).toBeUndefined();
  });
});

// W31. A write waiting out its backoff is asked again as soon as the network comes back: after a
// 40 s outage it lands within 2 s of the browser's `online`, rather than after the rest of a 30 s
// pause. A `409 KEY_IN_FLIGHT` waits the `Retry-After` Dispatch named.
test("a write lands within 2 s of the network returning, and KEY_IN_FLIGHT honours Retry-After", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let up = false;
    const landed = statusWrite(writes, () =>
      up ? Promise.resolve({ key: "CORE-1" }) : Promise.reject(dropped())
    );
    let done = false;
    void landed.then(() => {
      done = true;
    });
    await tick(40_000);
    expect(done).toBe(false);
    up = true;
    window.dispatchEvent(new Event("online"));
    await tick(2_000);
    expect(done).toBe(true);

    const times: number[] = [];
    const start = Date.now();
    let answers = 0;
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      answers += 1;
      return answers === 1
        ? Promise.reject(new ApiError(409, { code: "KEY_IN_FLIGHT", error: "running" }, 3_000))
        : Promise.resolve({});
    });
    await tick(2_500);
    // An answer elsewhere does not cut Dispatch's own Retry-After short.
    window.dispatchEvent(new Event("online"));
    await tick(1_000);
    expect(times).toHaveLength(2);
    expect(times[1]).toBeGreaterThanOrEqual(3_000);
    expect(times[1]).toBeLessThan(3_200);
  });
});

// A write Dispatch never answers is asked again after 1 s, 2 s, 4 s, 8 s and 16 s, and never more
// than 30 s apart after that, however long the outage lasts.
test("the backoff doubles from 1 s and is capped at 30 s", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const times: number[] = [];
    const start = Date.now();
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      return Promise.reject(dropped());
    }).catch(() => {});
    await tick(1_000 + 2_000 + 4_000 + 8_000 + 16_000 + 30_000 + 30_000 + 500);
    const gaps = times.slice(1).map((at, index) => at - (times[index] ?? 0));
    // Each gap is read at the 100 ms the clock moves by.
    expect(gaps.map((gap) => Math.round(gap / 1_000))).toEqual([1, 2, 4, 8, 16, 30, 30]);
  });
});

// A DELETE whose answer was lost and that Dispatch answers 404 on the next attempt was made: the
// thing it removes is gone either way, so it lands rather than being refused.
test("a resent DELETE answered 404 lands", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const remove: WriteRequest = {
      method: "DELETE",
      path: "/api/v1/projects/CORE/architecture-sources/legion/arch",
    };
    let attempts = 0;
    const landed = writes.run({
      detail: null,
      kind: plainWrite,
      label: "Source removed",
      method: "DELETE",
      presentation: "indicator",
      write: attemptOf(remove, () => {
        attempts += 1;
        return Promise.reject(
          attempts === 1 ? dropped() : new ApiError(404, { code: "SOURCE_NOT_FOUND" })
        );
      }),
    });
    await tick(1_500);
    expect(await landed).toBeNull();
    expect(writes.rows()).toEqual([]);
  });
});

// The write landed and its answer was lost: a repeat under the key is `409 KEY_COMMITTED`. That is
// a landing with no answer: the press resolves `null`, nothing stays or is kept, and every read is
// refreshed.
test("409 KEY_COMMITTED is a landing: null, nothing kept, and every read refreshed", async () => {
  await onFakeClock(async () => {
    const { tab } = storeOf();
    const client = signedIn();
    const invalidate = spyOn(client, "invalidateQueries");
    const writes = new Writes(client, tab.page(), memoryFiles());
    let attempts = 0;
    const landed = statusWrite(writes, () => {
      attempts += 1;
      return Promise.reject(
        attempts === 1 ? unavailable : new ApiError(409, { code: "KEY_COMMITTED", error: "made" })
      );
    });
    await settle();
    const key = row(writes)?.key ?? "";
    await tick(1_000);
    expect(await landed).toBeNull();
    expect(writes.rows()).toEqual([]);
    expect(tab.kept(key)).toBeUndefined();
    expect(invalidate).toHaveBeenCalledWith();
    await tick(60_000);
    expect(attempts).toBe(2);
  });
});

// A landing is told before the store reads anything again: a listener that orders what Dispatch
// answered by when each read went out (`issue-field-writes.ts`) counts every read the landing
// starts as sent after it.
test("a landing with no answer tells its outcome before it reads every query again", async () => {
  await onFakeClock(async () => {
    const { tab } = storeOf();
    const client = signedIn();
    const order: string[] = [];
    spyOn(client, "invalidateQueries").mockImplementation(async () => {
      order.push("read again");
    });
    const writes = new Writes(client, tab.page(), memoryFiles());
    writes.onOutcome(({ kind }) => order.push(kind));
    const landed = statusWrite(writes, () =>
      Promise.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "made" }))
    );
    await settle();
    expect(await landed).toBeNull();
    expect(order).toEqual(["landed", "read again"]);
  });
});

// The reader was stopping a write when the page reloaded, so its last attempt went with the page.
// The reload asks once more under its key and makes no attempt after that one.
test("a write the reader was stopping resumes with exactly one attempt", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockRejectedValue(unavailable);
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      void statusWrite(before, () => Promise.reject(unavailable)).catch(() => {});
      await tick(SEND_DEADLINE_MS + 1_000);
      expect(before.stop(row(before)?.id ?? 0)).toBe(true);
      const key = row(before)?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ state: { status: "late", stopping: true } });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await tick(30 * 60_000);
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0]?.[1]?.key).toBe(key);
      const resumed = row(after);
      expect(resumed?.status).toBe("refused");
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendStoppedError);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// (a) Every write is kept from its press, one waiting its turn behind another on its line too: it
// is kept with the request its press names and its place in the line, so a reload during an outage
// makes both again, each once, in press order.
test("two writes on one line survive a reload and land once each, in press order", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    const landed: unknown[] = [];
    const failing = new WeakSet<WriteRequest>();
    const send = spyOn(api, "send").mockImplementation(async (request) => {
      if (failing.has(request)) throw dropped();
      landed.push(request.body);
      return {} as never;
    });
    try {
      for (const status of ["backlog", "done"]) {
        const request: WriteRequest = { ...patch, body: { status } };
        failing.add(request);
        void before
          .run({
            detail: null,
            kind: plainWrite,
            label: `CORE-1 set to ${status}`,
            presentation: "indicator",
            request,
            target: "issue:CORE-1",
          })
          .catch(() => {});
      }
      await settle();
      expect(before.rows().map((write) => write.status)).toEqual(["sending", "queued"]);
      expect(tab.kept(row(before, 1)?.key ?? "")).toMatchObject({
        request: { body: { status: "done" }, method: "PATCH" },
        sent: false,
        state: { status: "queued" },
      });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      expect(landed).toEqual([{ status: "backlog" }, { status: "done" }]);
      expect(after.rows()).toEqual([]);
    } finally {
      // The page the reload replaced is gone with it: its writes stop rather than go on trying
      // through the real client once this test's answers are taken back.
      before.clear();
      send.mockRestore();
      tab.close();
    }
  });
});

// W37. A write waiting its turn that was never sent is the reader's to discard - its press is
// refused, and nothing of it goes out - while one that is out is not.
test("a write waiting its turn and never sent can be discarded; one out cannot", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    let secondMade = 0;
    void statusWrite(writes, () => Promise.withResolvers<never>().promise, {
      target: "issue:CORE-1",
    }).catch(() => {});
    const waiting = statusWrite(
      writes,
      async () => {
        secondMade += 1;
        return {};
      },
      { label: "second", target: "issue:CORE-1" }
    ).catch((error: unknown) => error);
    await settle();
    expect(row(writes, 1)).toMatchObject({ sent: false, status: "queued" });
    expect(writes.discard(row(writes, 0)?.id ?? 0)).toBe(false);
    expect(writes.discard(row(writes, 1)?.id ?? 0)).toBe(true);
    expect(await waiting).toBeInstanceOf(WriteDiscardedError);
    expect(writes.rows().map((write) => write.status)).toEqual(["sending"]);
    expect(secondMade).toBe(0);
  });
});

// INFRA-592. Dispatch answers `503 DATABASE_BUSY` and `503 REQUEST_TIMEOUT` with a `Retry-After`
// it means, as it does a `429`: a write so answered is made again under its key once that wait
// is out, and its backoff stays where it was. Any other `503` names no wait the page can trust,
// so it keeps the 1 s, 2 s, 4 s backoff whatever `Retry-After` it carries.
test("a write answered 503 DATABASE_BUSY or REQUEST_TIMEOUT waits out its Retry-After, any other 503 backs off", async () => {
  await onFakeClock(async () => {
    const gapsAfter = async (code: string, ms: number) => {
      const { writes } = storeOf();
      const times: number[] = [];
      const start = Date.now();
      void statusWrite(writes, () => {
        times.push(Date.now() - start);
        return Promise.reject(new ApiError(503, { code, error: "not now" }, 7_000));
      }).catch(() => {});
      await tick(ms);
      writes.clear();
      await settle();
      return times.slice(1).map((at, index) => Math.round((at - (times[index] ?? 0)) / 1_000));
    };
    expect(await gapsAfter("DATABASE_BUSY", 21_500)).toEqual([7, 7, 7]);
    expect(await gapsAfter("REQUEST_TIMEOUT", 21_500)).toEqual([7, 7, 7]);
    expect(await gapsAfter("UNAVAILABLE", 7_500)).toEqual([1, 2, 4]);
  });
});

// W39. Only Dispatch becoming reachable again resets a failing write's backoff. A route that keeps
// answering 503 while every other request the page makes succeeds backs off 1 s, 2 s, 4 s … to its
// 30 s cap; once a request on its route has failed to reach Dispatch, the next answer there starts
// it over.
test("a route that keeps answering 503 backs off to its cap while other requests succeed", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const times: number[] = [];
    const start = Date.now();
    void statusWrite(writes, () => {
      times.push(Date.now() - start);
      return Promise.reject(unavailable);
    }).catch(() => {});
    for (let elapsed = 0; elapsed < 91_500; elapsed += 500) {
      // Another route's 2xx, while Dispatch was reachable all along.
      reachable("answer", routeOf("GET", "/api/v1/projects"));
      await tick(500);
    }
    const gaps = times.slice(1).map((at, index) => at - (times[index] ?? 0));
    expect(gaps.map((gap) => Math.round(gap / 1_000))).toEqual([1, 2, 4, 8, 16, 30, 30]);

    unreachable(routeOf(patch.method, patch.path));
    reachable("answer", routeOf(patch.method, patch.path));
    const made = times.length;
    await tick(2_200);
    expect(times.length).toBe(made + 2);
  });
});

// W47. A failing write's backoff starts over only once the browser is online again, the live
// socket reopens, or Dispatch answers on the write's own route. A route whose requests keep failing
// at the connection - a proxy dropping one request shape - backs off 1 s, 2 s, 4 s … to its 30 s
// cap however often the page's other routes answer meanwhile, even a route that came back after
// failing too; an answer on its own route, after it failed there, starts it over. W52: a route is
// its method and its path, so a read of the issue the write changes - which the page makes on
// every live event - says nothing of the write's own route.
test("a route that keeps failing at the connection backs off to its cap while other routes answer", async () => {
  await onFakeClock(async () => {
    const { writes } = storeOf();
    const times: number[] = [];
    const start = Date.now();
    let projectsUp = false;
    const fetched = spyOn(globalThis, "fetch").mockImplementation((async (
      input: RequestInfo | URL,
      init?: RequestInit
    ) => {
      const path = String(input);
      const key = new Headers(init?.headers).get("Idempotency-Key");
      if (path === patch.path && init?.method === "PATCH" && key !== "another press") {
        times.push(Date.now() - start);
        throw new TypeError("Failed to fetch");
      }
      if (path === "/api/v1/projects" && !projectsUp) throw new TypeError("Failed to fetch");
      return new Response("{}", { headers: { "Content-Type": "application/json" }, status: 200 });
    }) as typeof fetch);
    try {
      void writes
        .run({
          detail: null,
          kind: plainWrite,
          label: "CORE-1 set to done",
          method: "PATCH",
          presentation: "indicator",
          write: (bounds) => api.send(patch, bounds),
        })
        .catch(() => {});
      for (let elapsed = 0; elapsed < 91_500; elapsed += 500) {
        // Another route, failing at the connection and answering by turns, after the write's own
        // attempt failed at the connection.
        projectsUp = !projectsUp;
        void api.listProjects().catch(() => {});
        await tick(500);
      }
      const gaps = times.slice(1).map((at, index) => at - (times[index] ?? 0));
      expect(gaps.map((gap) => Math.round(gap / 1_000))).toEqual([1, 2, 4, 8, 16, 30, 30]);

      // A read of the issue answering, on the write's own path, starts nothing over: it is
      // another route.
      const capped = times.length;
      void api.getIssue("CORE-1").catch(() => {});
      await tick(2_200);
      expect(times.length).toBe(capped);

      // Its own route answering - another press's PATCH of the issue, after another route
      // answered last - starts it over.
      projectsUp = false;
      void api.listProjects().catch(() => {});
      await settle();
      projectsUp = true;
      void api.listProjects().catch(() => {});
      await settle();
      const made = times.length;
      void api
        .send(patch, { deadlineMs: KEYED_SEND_ATTEMPT_MS, key: "another press" })
        .catch(() => {});
      await tick(2_200);
      expect(times.length).toBe(made + 2);

      // So does the live socket reopening after it was lost, once the backoff has grown again.
      await tick(20_000);
      const grown = times.length;
      unreachable();
      reachable("socket");
      await tick(2_200);
      expect(times.length).toBe(grown + 2);
    } finally {
      fetched.mockRestore();
    }
  });
});

// W49, W54. A plain write on a line is made from the request its press names and from nothing
// else: every attempt of it is `api.send` of that very request, which is what it is kept as while
// it waits its turn, so the request a reload makes again and the one that goes out cannot differ,
// and nothing of it runs before its turn. A plain write on a line that names no request, only an
// attempt, is a page bug, refused at once with nothing of it run.
test("a plain write on a line goes out as the very request it is kept as", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    const first = Promise.withResolvers<unknown>();
    void statusWrite(writes, () => first.promise, { target: "issue:CORE-1" }).catch(() => {});
    const request: WriteRequest = { ...patch, body: { status: "backlog" } };
    const second = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-1 set to backlog",
      presentation: "indicator",
      request: sends.answer(request, async () => ({ status: "backlog" })),
      target: "issue:CORE-1",
    });
    await settle();
    expect(row(writes, 1)).toMatchObject({ sent: false, status: "queued" });
    expect(tab.kept(row(writes, 1)?.key ?? "")).toMatchObject({
      request: { body: { status: "backlog" }, method: "PATCH", path: patch.path },
      state: { status: "queued" },
    });
    expect(sends.calls).toHaveLength(1);
    const effects: string[] = [];
    const unplanned = await writes
      .run({
        detail: null,
        kind: plainWrite,
        label: "CORE-1 set to todo",
        method: "PATCH",
        presentation: "indicator",
        target: "issue:CORE-1",
        write: async () => {
          effects.push("unplanned");
          return {};
        },
      })
      .catch((error: unknown) => error);
    expect(unplanned).toBeInstanceOf(TypeError);
    expect(writes.rows()).toHaveLength(2);

    first.resolve({});
    expect(await second).toEqual({ status: "backlog" });
    expect(sends.calls[1]).toBe(request);
    expect(sends.calls[1]).toEqual({
      body: { status: "backlog" },
      method: "PATCH",
      path: patch.path,
    });
    expect(effects).toEqual([]);

    // A write made by an attempt of its own makes one request on every attempt: one that makes
    // another is a page bug, refused at once rather than kept as a request a reload would make.
    let attempts = 0;
    const other = writes.run({
      detail: null,
      kind: plainWrite,
      label: "CORE-2 set to done",
      method: "PATCH",
      presentation: "indicator",
      write: (bounds) => {
        attempts += 1;
        if (attempts === 1) return attemptOf(patch, () => Promise.reject(dropped()))(bounds);
        return attemptOf({ ...patch, path: "/api/v1/issues/CORE-2" }, async () => ({}))(bounds);
      },
    });
    await tick(1_500);
    expect(await other.catch((error: unknown) => error)).toBeInstanceOf(OneRequestPerWriteError);
  });
});

// A tab that slept longer than Dispatch keeps a key wakes holding a write under a key Dispatch
// has forgotten: making it again would write the person's comment, message or upload a second
// time, so nothing goes out and it comes back for the reader to Retry as a new write.
test("a resumed write whose key Dispatch has forgotten makes no attempt", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = resuming(tab, spent, { status: "sending" });
      await tick(60_000);
      expect(send).not.toHaveBeenCalled();
      const resumed = row(writes);
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendExpiredError);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

/** A tab holding one write of alice's that never left the page: it is kept with the request it
 *  was planned as, as every write waiting its turn is, and `sent` says nothing has gone out
 *  under its key. */
function neverSent(tab: KeptTab, key: string, pressedAt: number): Writes {
  tab.storage.setItem(
    `dispatch.write:${key}`,
    JSON.stringify({
      detail: null,
      key,
      kind: "write",
      label: "CORE-1 set to done",
      login: "alice",
      presentation: "indicator",
      pressedAt,
      request: patch,
      sent: false,
      state: { status: "queued" },
      target: null,
      version: 1,
    })
  );
  const writes = new Writes(signedIn(), tab.page(), memoryFiles());
  writes.resume("alice");
  return writes;
}

// What a key has carried is what `sent` says, not what the kept request says: a write kept while
// it waited its turn holds the request it would go out as, and nothing of it has left the page.
// So a tab that slept past the key's lifetime holding one makes it, under that same key, as the
// same write in a page that never reloaded would be made.
test("a resumed write that never left the page goes out under a key older than the lifetime", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = neverSent(tab, spent, Date.now());
      await tick(60_000);
      expect(send).toHaveBeenCalledTimes(1);
      expect((send.mock.calls[0]?.[1] as WriteBounds).key).toBe(spent);
      expect(writes.rows()).toEqual([]);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// The same write, pressed longer ago than a page makes one again on its own: it comes back
// interrupted rather than expired, and its Retry goes out under that same key, since the key has
// carried nothing for its age to spend.
test("a resumed write that never left the page comes back interrupted, not expired", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = neverSent(tab, spent, Date.now() - KEPT_SEND_RESUME_MS - 1);
      await settle();
      const resumed = row(writes);
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendInterruptedError);
      expect(send).not.toHaveBeenCalled();

      writes.retry(resumed?.id ?? 0);
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect((send.mock.calls[0]?.[1] as WriteBounds).key).toBe(spent);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

/** A write that builds its own request at each attempt, as a composer's send does: a reload
 *  takes it up whether or not a request ever went out under its key. */
let composedAttempts = 0;
const composedWrite = defineWriteKind<null>({
  kept: (value): value is null => value === null,
  name: "test-composed",
  resume: () => (bounds) => {
    composedAttempts += 1;
    bounds.sent?.(patch);
    return Promise.resolve({ key: "CORE-1" });
  },
});

// The key stops only a write something has already gone out under. One kept with no request sent
// nothing - a write's request is kept as its first attempt goes out - so Dispatch has never seen
// its key, however old the key is. Pressed longer ago than a page makes a write again on its own,
// it comes back interrupted, and its first Retry makes the attempt, under that same key: it is
// not refused as expired first, for the reader to press Retry a second time under a new key.
test("a resumed write that sent nothing makes its attempt on the first Retry", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      composedAttempts = 0;
      tab.storage.setItem(
        `dispatch.write:${spent}`,
        JSON.stringify({
          detail: null,
          key: spent,
          kind: composedWrite.name,
          label: "A message",
          login: "alice",
          presentation: "composer",
          pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1,
          request: null,
          sent: false,
          state: { status: "sending" },
          target: null,
          version: 1,
        })
      );
      const writes = new Writes(signedIn(), tab.page(), memoryFiles());
      writes.resume("alice");
      await settle();
      const resumed = row(writes);
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendInterruptedError);
      expect(composedAttempts).toBe(0);

      writes.retry(resumed?.id ?? 0);
      await settle();
      expect(composedAttempts).toBe(1);
      expect(row(writes)).toBeUndefined();
    } finally {
      tab.close();
    }
  });
});

// A `PUT` sends no key: Dispatch keeps nothing under one for it, and making it again is the same
// change. So a tab that slept past the key's lifetime makes that write again rather than ending
// it and telling the reader a Retry may save it a second time.
test("a resumed write that sends no key goes out under a key older than the lifetime", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ seq: 2 } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = resuming(tab, spent, { status: "sending" }, put);
      await tick(60_000);
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0]?.[0]).toEqual(put);
      expect((send.mock.calls[0]?.[1] as WriteBounds).key).toBe(spent);
      expect(writes.rows()).toEqual([]);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// A lapsed sign-in puts a write that was out back in its line, to go again under its key once its
// person signs in. One whose key Dispatch has forgotten has nothing to go back for: it is refused
// there and then, rather than waiting in the line under a key no attempt may use.
test("a held write whose key is spent is refused rather than put back in its line", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = resuming(tab, spent, { status: "sending" });
      // The session lapses while the resumed write is out, which holds it.
      writes.signedIn(undefined);
      await tick(60_000);
      const held = row(writes);
      expect(held?.status === "refused" && held.refusal).toBeInstanceOf(SendExpiredError);
      expect(send).not.toHaveBeenCalled();
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// Retry of a write whose key is spent is a new write: it goes out under a new key, which Dispatch
// knows, and the page stops keeping the write under the old one.
test("a Retry of a write whose key is spent goes out under a new key", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      const writes = resuming(tab, spent, { status: "sending" });
      await settle();
      writes.retry(row(writes)?.id ?? 0);
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect((send.mock.calls[0]?.[1] as WriteBounds).key).not.toBe(spent);
      expect(writes.rows()).toEqual([]);
      expect(tab.kept(spent)).toBeUndefined();
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// The deadline a write stops at is its key's own, so a Retry under that key is not another day of
// trying: one retried two seconds before Dispatch forgets its key is tried for those two seconds,
// under that same key, and then ends expired.
test("a Retry under the write's own key stops when that key runs out", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockRejectedValue(unavailable);
    try {
      const nearlySpent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS + 2_000);
      const writes = resuming(tab, nearlySpent, {
        refusal: { kind: "stopped" },
        status: "refused",
      });
      await settle();
      writes.retry(row(writes)?.id ?? 0);
      await tick(10_000);
      const ended = row(writes);
      expect(ended?.key).toBe(nearlySpent);
      expect(ended?.status === "refused" && ended.refusal).toBeInstanceOf(SendExpiredError);
      expect(send).toHaveBeenCalledTimes(2);
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

// The release before this one kept no record of what a key had carried (`KeptWrite.carried`), so
// an entry it wrote holds none. The format's version stands and the field is the only one left
// out, so that entry still loads - none of the person's unresumed writes is dropped - and it is
// read as the release that wrote it read it: `sent` says whether a request went out under the
// key, and the request it holds says what that was. A day-old key a `PATCH` went out under comes
// back expired, and the entry this page keeps in its place holds the record from now on.
test("an entry kept with no carried loads, and reads as what it sent", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
      // `resuming` writes the entry as that release wrote it, `carried` and all its kin absent.
      const writes = resuming(tab, spent, { refusal: { kind: "stopped" }, status: "refused" });
      await settle();
      const resumed = row(writes);
      expect(resumed?.status === "refused" && resumed.refusal).toBeInstanceOf(SendExpiredError);
      expect(send).not.toHaveBeenCalled();
      expect(tab.kept(spent)).toMatchObject({ carried: "PATCH", sent: true, version: 1 });
    } finally {
      send.mockRestore();
      tab.close();
    }
  });
});

/** Whether the surface of `claimedWrite` takes a conflict to settle with the reader now, and how
 *  often its kind undid a write. */
const surface = { claims: false, undone: 0 };

/** A write whose surface may take a conflict that refused it (`WriteKind.claims`): a priority's
 *  title, as far as the store is concerned, its record the reader's text. A refused one that went
 *  out takes the place of an unsent claim of the same text (`WriteKind.supersedes`). */
const claimedWrite = defineWriteKind<{ readonly text: string }>({
  claims: () => surface.claims,
  draft: (write) => write.detail.text,
  kept: (value): value is { readonly text: string } =>
    typeof value === "object" && value !== null && typeof Reflect.get(value, "text") === "string",
  name: "test-claimed",
  supersedes: (refused, unsent) => refused.detail.text === unsent.detail.text,
  undone: () => {
    surface.undone += 1;
  },
});

/** Dispatch's refusal of a write over a text another person has since replaced: the same body is
 *  refused the same way every time it is sent. */
const changed = new ApiError(409, {
  code: "PRIORITY_CHANGED",
  error: "the title was written meanwhile",
});

/** Presses a write of `claimedWrite` in `writes`, each attempt answered by `answer`; `keys` hears
 *  each attempt's key. */
function claimedPress(writes: Writes, answer: () => Promise<unknown>, keys: string[] = []) {
  return writes
    .run({
      detail: { text: "Mine" },
      kind: claimedWrite,
      label: "Rename the priority",
      method: "PATCH",
      presentation: "indicator",
      write: (bounds) => {
        keys.push(bounds.key);
        bounds.sent?.(patch);
        return answer();
      },
    })
    .catch((error: unknown) => error);
}

// A conflict only the reader can settle that a surface takes stays in the store, kept for a
// reload as any refusal is, and claimed: the shell's line leaves it to that surface. Its kind
// still undoes it, and what became of it is still told, for the surface that pressed it. The
// surface going hands it back (`unclaim`); the reader settling it there lets it go (`settle`),
// with nothing kept of it and no outcome told.
test("a conflict a surface claims stays in the store and kept, claimed, until the reader settles it", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    surface.claims = true;
    surface.undone = 0;
    const outcomes: string[] = [];
    const stop = writes.onOutcome((outcome) => outcomes.push(outcome.kind));
    try {
      const keys: string[] = [];
      const refused = claimedPress(writes, () => Promise.reject(changed), keys);
      await settle();
      expect(await refused).toBe(changed);
      const claimed = row(writes);
      expect(claimed).toMatchObject({ claimed: true, refusal: changed, status: "refused" });
      expect(tab.kept(keys[0] ?? "")).toMatchObject({
        detail: { text: "Mine" },
        state: { refusal: { code: "PRIORITY_CHANGED" }, status: "refused" },
      });
      expect(outcomes).toEqual(["refused"]);
      expect(surface.undone).toBe(1);
      const id = claimed?.id ?? 0;
      // Claimed already: no second surface takes it.
      expect(writes.claim(id)).toBe(false);

      writes.unclaim(id);
      expect(row(writes)).toMatchObject({ claimed: false, status: "refused" });
      // Only a surface that holds it settles it.
      expect(writes.settle(id)).toBe(false);
      expect(writes.claim(id)).toBe(true);
      expect(writes.settle(id)).toBe(true);
      expect(writes.rows()).toEqual([]);
      expect(tab.kept(keys[0] ?? "")).toBeUndefined();
      expect(outcomes).toEqual(["refused"]);
    } finally {
      stop();
      surface.claims = false;
      tab.close();
    }
  });
});

// One no surface takes stays refused for the reader to discard, and no Retry sends its body
// again: Dispatch would refuse it the same way.
test("a conflict no surface claims stays refused, and its Retry sends nothing", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    try {
      const keys: string[] = [];
      void claimedPress(writes, () => Promise.reject(changed), keys);
      await settle();
      const refused = row(writes);
      expect(refused).toMatchObject({ refusal: changed, status: "refused" });
      writes.retry(refused?.id ?? 0);
      await tick(60_000);
      expect(keys).toHaveLength(1);
      expect(row(writes)).toMatchObject({ refusal: changed, status: "refused" });
      expect(writes.discard(refused?.id ?? 0)).toBe(true);
      expect(writes.rows()).toEqual([]);
    } finally {
      tab.close();
    }
  });
});

// Any other refusal of the same kind is none of the surface's: it stays on the line, and its
// Retry sends it again.
test("a refusal that is no conflict stays refused, and its Retry sends it again", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    surface.claims = true;
    try {
      const keys: string[] = [];
      let answer: () => Promise<unknown> = () => Promise.reject(verdict);
      void claimedPress(writes, () => answer(), keys);
      await settle();
      expect(row(writes)).toMatchObject({ refusal: verdict, status: "refused" });
      answer = async () => ({ key: "CORE-1" });
      writes.retry(row(writes)?.id ?? 0);
      await settle();
      expect(keys).toHaveLength(2);
      expect(writes.rows()).toEqual([]);
    } finally {
      surface.claims = false;
      tab.close();
    }
  });
});

/** How a reload keeps a write Dispatch refused as `changed`. */
const keptConflict: KeptState = {
  refusal: { code: "PRIORITY_CHANGED", kind: "api", message: changed.message, status: 409 },
  status: "refused",
};

/** The record `claimedWrite` keeps, as a reload reads it. */
const keptClaimed = { detail: { text: "Mine" }, kind: claimedWrite.name };

// A reload's kept conflict comes back as it stood. One a surface takes comes back claimed, and
// so is one a surface drawn later takes after the fact (`claim`); each stays kept until the
// reader settles it. Until a surface takes it, it stands refused, and no Retry of it sends its
// body again.
test("a resumed conflict a surface claims comes back claimed, and one claimed later stays kept until settled", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    try {
      surface.claims = true;
      const claimed = resuming(tab, "claimed-key", keptConflict, patch, keptClaimed);
      await settle();
      const taken = row(claimed);
      expect(taken).toMatchObject({ claimed: true, kind: claimedWrite.name, status: "refused" });
      expect(tab.kept("claimed-key")).toMatchObject({ state: { status: "refused" } });
      expect(claimed.settle(taken?.id ?? 0)).toBe(true);
      expect(tab.kept("claimed-key")).toBeUndefined();

      surface.claims = false;
      const shown = resuming(tab, "shown-key", keptConflict, patch, keptClaimed);
      await settle();
      const refused = row(shown);
      expect(refused).toMatchObject({ claimed: false, kind: claimedWrite.name, status: "refused" });
      expect(refused?.status === "refused" && refused.refusal).toMatchObject({
        code: "PRIORITY_CHANGED",
        status: 409,
      });
      shown.retry(refused?.id ?? 0);
      await settle();
      expect(send).not.toHaveBeenCalled();
      expect(shown.claim(refused?.id ?? 0)).toBe(true);
      expect(row(shown)).toMatchObject({ claimed: true, status: "refused" });
      expect(tab.kept("shown-key")).toMatchObject({ state: { status: "refused" } });
      expect(shown.settle(refused?.id ?? 0)).toBe(true);
      expect(shown.rows()).toEqual([]);
      expect(tab.kept("shown-key")).toBeUndefined();
      expect(send).not.toHaveBeenCalled();
    } finally {
      surface.claims = false;
      send.mockRestore();
      tab.close();
    }
  });
});

// A text of the reader's a surface kept on its own, with no write of it refused, is claimed as a
// conflict that surface holds: kept for a reload as any refusal is, never sent, no Retry of it
// sends anything, and no outcome is told, since nothing was pressed. From there it is any
// claimed conflict: handed back to the line, taken up refused by a reload, taken again, and let
// go once the reader settles it. A refusal that is no conflict is no such claim at all.
test("a text a surface claims unsent is a claimed conflict nothing sends, kept for a reload until the reader settles it", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    const outcomes: string[] = [];
    const stop = writes.onOutcome((outcome) => outcomes.push(outcome.kind));
    const spec = {
      detail: { text: "Mine" },
      kind: claimedWrite,
      label: "Rename the priority",
      refusal: changed,
      request: patch,
    };
    try {
      expect(() => writes.claimUnsent({ ...spec, refusal: verdict })).toThrow(TypeError);
      expect(writes.rows()).toEqual([]);

      const id = writes.claimUnsent(spec);
      expect(row(writes)).toMatchObject({
        claimed: true,
        id,
        refusal: changed,
        sent: false,
        status: "refused",
      });
      const key = row(writes)?.key ?? "";
      expect(tab.kept(key)).toMatchObject({
        detail: { text: "Mine" },
        sent: false,
        state: keptConflict,
      });
      writes.retry(id);
      await tick(60_000);
      expect(row(writes)).toMatchObject({ claimed: true, status: "refused" });
      writes.unclaim(id);
      expect(row(writes)).toMatchObject({ claimed: false, status: "refused" });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      const resumed = row(after);
      expect(resumed).toMatchObject({ claimed: false, kind: claimedWrite.name, status: "refused" });
      expect(resumed?.status === "refused" && resumed.refusal).toMatchObject({
        code: "PRIORITY_CHANGED",
        status: 409,
      });
      after.retry(resumed?.id ?? 0);
      await settle();
      expect(after.claim(resumed?.id ?? 0)).toBe(true);
      expect(after.settle(resumed?.id ?? 0)).toBe(true);
      expect(after.rows()).toEqual([]);
      expect(tab.kept(key)).toBeUndefined();
      expect(send).not.toHaveBeenCalled();
      expect(outcomes).toEqual([]);
    } finally {
      stop();
      send.mockRestore();
      tab.close();
    }
  });
});

// A write that went out and that a conflict refused, carrying the text of an unsent claim, takes
// its place whether a surface holds that claim or the line does: the claim goes, from the store
// and from what the tab keeps, so the line names the text once. A claim of another text stays.
// One a surface claims after the refusal stays that surface's while it holds it, and goes once it
// is handed back to the line. A reload that takes up both of one text keeps the refused write
// alone, in whichever order the two come back.
test("a refused write that went out takes the place of the unsent claim of the same text", async () => {
  await onFakeClock(async () => {
    const { tab, writes } = storeOf();
    const claimUnsent = (text: string) =>
      writes.claimUnsent({
        detail: { text },
        kind: claimedWrite,
        label: "Rename the priority",
        refusal: changed,
        request: patch,
      });
    try {
      claimUnsent("Mine");
      writes.unclaim(claimUnsent("Mine"));
      claimUnsent("Other");
      const claimKeys = writes.rows().map((one) => one.key);
      const pressed: string[] = [];
      void claimedPress(writes, () => Promise.reject(changed), pressed);
      await settle();
      expect(writes.rows()).toMatchObject([
        { detail: { text: "Other" }, sent: false, status: "refused" },
        { detail: { text: "Mine" }, sent: true, status: "refused" },
      ]);
      expect(claimKeys.map((key) => tab.kept(key) !== undefined)).toEqual([false, false, true]);
      expect(tab.kept(pressed[0] ?? "")).toMatchObject({ sent: true, state: keptConflict });

      const later = claimUnsent("Mine");
      expect(writes.rows().map((one) => one.id)).toContain(later);
      writes.unclaim(later);
      expect(writes.rows()).toMatchObject([
        { detail: { text: "Other" }, sent: false, status: "refused" },
        { detail: { text: "Mine" }, sent: true, status: "refused" },
      ]);
    } finally {
      tab.close();
    }

    for (const sentFirst of [true, false]) {
      const reload = keptTab();
      try {
        for (const [index, sent] of [sentFirst, !sentFirst].entries()) {
          reload.storage.setItem(
            `dispatch.write:key-${index}`,
            JSON.stringify({
              ...keptClaimed,
              key: `key-${index}`,
              label: "Rename the priority",
              login: "alice",
              presentation: "indicator",
              pressedAt: Date.now() + index,
              request: patch,
              sent,
              state: keptConflict,
              target: null,
              version: 1,
            })
          );
        }
        const after = new Writes(signedIn(), reload.page(), memoryFiles());
        after.resume("alice");
        await settle();
        expect(after.rows()).toMatchObject([
          { detail: { text: "Mine" }, sent: true, status: "refused" },
        ]);
        expect(reload.kept(sentFirst ? "key-1" : "key-0")).toBeUndefined();
      } finally {
        reload.close();
      }
    }
  });
});

// Whether a write goes under its key is its press's to say (`WriteSpec.method`), and a reload
// reads it back from what was kept (`KeptWrite.keyed`), not from a request: a write its kind
// makes again from its own record, kept before any attempt of it went out, holds none. So the
// indicator says of a `PUT` that came back interrupted that its Retry sends the same change
// again, and of a `POST` that it asks again under the same key.
test("a reload reads back whether each write goes under its key, with no request to read it from", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    try {
      const before = new Writes(signedIn(), tab.page(), memoryFiles());
      const hang = () => Promise.withResolvers<never>().promise;
      for (const method of ["PUT", "POST"] as const) {
        void before
          .run({
            detail: null,
            kind: composedWrite,
            label: method,
            method,
            presentation: "indicator",
            write: hang,
          })
          .catch(() => {});
      }
      await settle();
      for (const write of before.rows()) {
        const entry = tab.kept(write.key) as { pressedAt: number; request: unknown };
        expect(entry.request).toBeNull();
        tab.storage.setItem(
          `dispatch.write:${write.key}`,
          JSON.stringify({ ...entry, pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1 })
        );
      }
      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      expect(Object.fromEntries(after.rows().map((write) => [write.label, write.keyed]))).toEqual({
        POST: true,
        PUT: false,
      });
    } finally {
      tab.close();
    }
  });
});
