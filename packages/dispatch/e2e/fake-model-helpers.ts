import { test } from "@playwright/test";

import type { FakeModelRequest, FakeModelRequests, FakeModelRule } from "./fake-model";
import { harnessPorts } from "./harness-ports";

/** Whether this run started the stand-in model (`e2e/fake-model.ts`): every run that starts its
 *  own server does, and a deployed run (`PLAYWRIGHT_BASE_URL`) starts neither. */
const usesFakeModel = !process.env.PLAYWRIGHT_BASE_URL;

function fixtureURL(path: string): string {
  return `http://127.0.0.1:${harnessPorts().fakeModel.port}${path}`;
}

async function fixtureWrite(path: string, body: unknown): Promise<void> {
  const response = await fetch(fixtureURL(path), {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`fake model ${path}: ${response.status} ${await response.text()}`);
  }
}

/** Replaces the stand-in model's rules wholesale: each request the server's model groups make is
 *  answered by the first rule that matches it, and a request none matches is refused, which the
 *  server reads as a model that failed (`e2e/fake-model.ts`). The row that scripts it is skipped
 *  where this run started no stand-in. */
export async function setModelRules(rules: readonly FakeModelRule[]): Promise<void> {
  test.skip(!usesFakeModel, "a deployed run starts no stand-in model");
  await fixtureWrite("/__fixture/rules", rules);
}

/** Every request the server has made to the stand-in model since the last reset, oldest first;
 *  each names its model group's deployment as its model. */
export async function modelRequests(): Promise<readonly FakeModelRequest[]> {
  const response = await fetch(fixtureURL("/__fixture/requests"));
  if (!response.ok) {
    throw new Error(`fake model /__fixture/requests: ${response.status}`);
  }
  return ((await response.json()) as FakeModelRequests).requests;
}

/** Forgets the requests counted so far, keeping the rules a row scripted, so a row that made the
 *  server call a model while it was setting its fixture up counts from zero. */
export async function forgetModelRequests(): Promise<void> {
  await fixtureWrite("/__fixture/requests", {});
}

/** Clears the stand-in model between rows, so the requests one row made are never read by the
 *  next and the rules it scripted answer none of them. A run that started no stand-in has nothing
 *  to clear. */
export async function resetFakeModel(): Promise<void> {
  if (!usesFakeModel) return;
  await fixtureWrite("/__fixture/reset", {});
}
