import { expect, test } from "bun:test";
import { join } from "node:path";
import { rowLimit, runBounded } from "../../../packages/dispatch/e2e/__tests__/bounded-subprocess";

// Two drivers of scripts/e2e take Dispatch's URL as their first argument and the URL of a fake
// server of their own as their third, and either may carry a user and a password. One that is no
// http or https URL with a host is refused naming its length and the cause, here that it does not
// parse, with how to write a user part since it holds an `@`, before anything parses it: the URL
// parser's error for it, which the runtime prints on its `input:` or `base:` line, holds the value
// whole. The password is made for this run.
const drivers = [
  {
    script: "dispatch-claimant-delivery.ts",
    fake: "<fake-envoy-url>",
    env: { CLAIMANT_E2E_AGENT_TOKEN: "made-up-agent-token" },
  },
  {
    script: "verification-results.ts",
    fake: "<fake-github-url>",
    env: {
      VERIFICATION_RESULTS_E2E_AGENT_TOKEN: "made-up-agent-token",
      VERIFICATION_RESULTS_E2E_DATABASE_URL: "postgres://127.0.0.1:1/never-opened",
    },
  },
] as const;

/** What each driver prints and how it exits, run with `dispatchUrl` and `fakeUrl`. */
async function runDrivers(dispatchUrl: string, fakeUrl: string) {
  const said: { readonly code: number; readonly output: string }[] = [];
  for (const { script, env } of drivers) {
    const { code, stdout, stderr } = await runBounded({
      argv: [
        process.execPath,
        join(import.meta.dir, "..", script),
        dispatchUrl,
        "nats://127.0.0.1:1",
        fakeUrl,
      ],
      bound: "short",
      env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", ...env },
      from: import.meta.url,
    });
    said.push({ code, output: stdout + stderr });
  }
  return said;
}

/** The words a driver refuses `value`, given as `argument`, with. */
function refusal(argument: string, value: string): string {
  return `${argument} must be an http or https URL: its value is ${value.length} characters long; it does not parse as a URL; if it carries a user part, write each @, /, ?, # and \\ in that part percent-encoded (@ as %40, / as %2F), or remove the user part\n`;
}

test(
  "a driver whose Dispatch URL is no http or https URL with a host names its length and cause",
  async () => {
    const password = crypto.randomUUID();
    const dispatchUrl = `http://made-up-user:${password}/x@[bad`;
    expect(await runDrivers(dispatchUrl, "http://127.0.0.1:1")).toEqual(
      drivers.map(() => ({ code: 2, output: refusal("<dispatch-url>", dispatchUrl) }))
    );
  },
  // One driver process for each script, one after the other.
  rowLimit("short", "short")
);

test(
  "a driver whose fake server's URL is no http or https URL with a host names its length and cause",
  async () => {
    const password = crypto.randomUUID();
    const fakeUrl = `http://made-up-user:${password}/x@[bad`;
    expect(await runDrivers("http://127.0.0.1:1", fakeUrl)).toEqual(
      drivers.map(({ fake }) => ({ code: 2, output: refusal(fake, fakeUrl) }))
    );
  },
  // One driver process for each script, one after the other.
  rowLimit("short", "short")
);
