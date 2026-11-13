import { readFileSync } from "node:fs";
import { request } from "node:https";
import { fileURLToPath } from "node:url";

import { test } from "@playwright/test";

import type { ChatScript, FakeChatCalls } from "./fake-chat-model";
import type { DecisionScript, FakeDecisionCalls } from "./fake-decision-model";
import { modelsDirVariable, modelsFilesIn, usesFakeModels } from "./harness-models";
import { type HarnessListenerKey, harnessPorts, stackValue } from "./harness-ports";

// What a row of the models lane uses to drive the fake writing model and the fake decision
// model (`e2e/fake-chat-model.ts`, `e2e/fake-decision-model.ts`) and to read what the server
// asked of them. Each call addresses the fakes of this worker's own stack, by the ports and the
// models directory that stack's variables carry (`e2e/harness-ports.ts`, `e2e/harness-models.ts`),
// and trusts that stack's certificate alone. Node's own fetch takes no certificate per request, so
// these go through `node:https`.

/** The record `packages/envoy/going/decision_models.json` holds for one measured model. */
export interface DecisionRecord {
  readonly model: string;
  readonly action_threshold: number;
  /** Null where the display did not fit. */
  readonly chart_threshold: number | null;
  readonly chosen: boolean;
}

const recordPath = fileURLToPath(
  new URL("../../envoy/going/decision_models.json", import.meta.url)
);

/** The entry of the committed decision record the record marks chosen, which is the model
 *  `e2e/run-server.sh` hands the server: a row reads its thresholds from here, so no row writes a
 *  threshold of its own and a new measurement moves every row with it. */
export function decisionRecord(): DecisionRecord {
  const entries = JSON.parse(readFileSync(recordPath, "utf8")) as DecisionRecord[];
  const chosen = entries.filter((entry) => entry.chosen === true);
  if (chosen.length !== 1) {
    throw new Error(
      `${recordPath} marks ${chosen.length} entries chosen; the models lane needs exactly one`
    );
  }
  return chosen[0];
}

/** Skips the row unless this run is the models lane (`DISPATCH_E2E_MODELS=fake`, `bun run
 *  e2e:models`), whose server answers and decides through the fakes. Every helper below that
 *  scripts a fake calls it, so a row that scripts one is skipped everywhere else. */
export function modelsLane(): void {
  test.skip(
    !usesFakeModels,
    "this run's server answers through no fake model (DISPATCH_E2E_MODELS=fake, bun run e2e:models)"
  );
}

type FakeModel = Extract<HarnessListenerKey, "fakeChatModel" | "fakeDecisionModel">;

const names: Record<FakeModel, string> = {
  fakeChatModel: "fake chat model",
  fakeDecisionModel: "fake decision model",
};

/** The stack's certificate, read once per directory: every fixture call trusts it, and a row
 *  reads the counters between keystrokes. */
let trusted: { readonly path: string; readonly bytes: Buffer } | undefined;

function certificate(path: string): Buffer {
  if (trusted?.path !== path) trusted = { bytes: readFileSync(path), path };
  return trusted.bytes;
}

/** One fixture call to `model`, answered with its body, refused naming the route on a status
 *  other than 200. */
function fixture(
  model: FakeModel,
  method: "GET" | "PUT",
  path: string,
  body?: unknown
): Promise<string> {
  const files = modelsFilesIn(stackValue(process.env, modelsDirVariable), `the ${names[model]}`);
  const payload = body === undefined ? undefined : JSON.stringify(body);
  // The executor form rather than Promise.withResolvers, which Playwright's runner may run on a
  // Node that predates.
  return new Promise<string>((resolve, reject) => {
    const call = request(
      {
        ca: certificate(files.certificate),
        headers: payload === undefined ? {} : { "Content-Type": "application/json" },
        host: "127.0.0.1",
        method,
        path,
        port: harnessPorts()[model].port,
      },
      (response) => {
        const chunks: Buffer[] = [];
        response.on("data", (chunk: Buffer) => chunks.push(chunk));
        response.on("error", reject);
        response.on("end", () => {
          const text = Buffer.concat(chunks).toString("utf8");
          if (response.statusCode === 200) resolve(text);
          else reject(new Error(`${names[model]} ${path}: ${response.statusCode} ${text}`));
        });
      }
    );
    call.on("error", reject);
    call.end(payload);
  });
}

/** Replaces what the fake writing model answers: `script.replies`, one per conversation in
 *  order, then `script.default` (`e2e/fake-chat-model.ts`). */
export async function scriptChat(script: ChatScript): Promise<void> {
  modelsLane();
  await fixture("fakeChatModel", "PUT", "/__fixture/script", script);
}

/** Replaces what the fake decision model answers: `script.replies`, one per call in order, then
 *  `script.default` (`e2e/fake-decision-model.ts`). */
export async function scriptDecision(script: DecisionScript): Promise<void> {
  modelsLane();
  await fixture("fakeDecisionModel", "PUT", "/__fixture/script", script);
}

/** What the server has asked of the fake writing model since the last reset. */
export async function chatCalls(): Promise<FakeChatCalls> {
  return JSON.parse(await fixture("fakeChatModel", "GET", "/__fixture/calls")) as FakeChatCalls;
}

/** What the server has asked of the fake decision model since the last reset. */
export async function decisionCalls(): Promise<FakeDecisionCalls> {
  return JSON.parse(
    await fixture("fakeDecisionModel", "GET", "/__fixture/calls")
  ) as FakeDecisionCalls;
}

/** Clears both fakes between rows (`e2e/seed.ts`), so one row's script and counts reach none of
 *  the next. A run outside the models lane started neither, and has nothing to clear. */
export async function resetFakeModels(): Promise<void> {
  if (!usesFakeModels) return;
  await Promise.all([
    fixture("fakeChatModel", "PUT", "/__fixture/reset", {}),
    fixture("fakeDecisionModel", "PUT", "/__fixture/reset", {}),
  ]);
}
