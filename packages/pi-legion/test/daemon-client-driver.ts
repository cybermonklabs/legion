// The plugin side of k31's live proof (dispatch://INFRA-4730, decision 22): one call of a
// checkout's own daemon client (`createLegionDaemonClient`, which every operation of a deployed
// plugin goes through) against a running daemon, and what came of it. The release-before driver
// (packages/daemon/internal/daemon/tree_release_before_live_test.go, which scripts/e2e/tree-cutover.sh
// builds with `-tags e2e`) runs this file under bun in the root of the oldest plugin contract in the
// fleet (scripts/e2e/lib/old-plugin-root.sh copies it there), with its input on stdin, and reads the
// answer this file prints. So that contract's own client parses each request and each answer, and an
// answer it cannot read is this call's failure.
//
// The file runs against that contract's client and this checkout's, so it builds the client from
// what both have: this checkout's takes its options (the plugin's contract and the fetch) as an
// object, and an older one takes the fetch alone, so it is handed a fetch carrying those options as
// its own fields, which each version reads its own way, as gate-holder-driver.ts does.
//
// Input, one JSON object on stdin: `daemonUrl`, `method` (the client's method: `register`, `ready`,
// `activity`, `grant`, `handoffComplete` or `exit`) and `input` (the method's request; `register`
// is given the plugin's own contract from this root's manifest). Output, one JSON object on stdout:
// `ok` and the method's `answer`, or `ok` false with the daemon's `status`, `code` and `error`, or the
// contract `error` the client found in an answer it could not read.
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  createLegionDaemonClient,
  LegionDaemonApiError,
  type LegionDaemonClient,
  LegionDaemonContractError,
} from "@legion/pi-shared/daemon-client";
import { z } from "zod";
import pkg from "../package.json";

const Input = z.object({
  daemonUrl: z.string(),
  method: z.enum(["register", "ready", "activity", "grant", "handoffComplete", "exit"]),
  input: z.record(z.string(), z.unknown()),
});

const input = Input.parse(JSON.parse(await Bun.stdin.text()));
const daemonFetch = ((request: string | URL | Request, init?: RequestInit) =>
  fetch(request, init)) as typeof fetch;
const pluginContract = daemonContract(pkg, import.meta.dir);
const daemonOptions = Object.assign(daemonFetch, {
  pluginContract,
  fetch: daemonFetch,
});
const daemon = (createLegionDaemonClient as (url: string, options: unknown) => LegionDaemonClient)(
  input.daemonUrl,
  daemonOptions
);
const request = input.method === "register" ? { ...input.input, pluginContract } : input.input;
const call = daemon[input.method] as (request: unknown) => Promise<unknown>;

let answer: object;
try {
  answer = { ok: true, answer: (await call(request)) ?? null };
} catch (error) {
  if (error instanceof LegionDaemonApiError) {
    answer = { ok: false, status: error.status, code: error.code ?? "", error: error.message };
  } else if (error instanceof LegionDaemonContractError) {
    answer = { ok: false, status: error.status, code: "", error: error.message };
  } else {
    throw error;
  }
}
process.stdout.write(`${JSON.stringify(answer)}\n`);
