import { expect, test } from "bun:test";
import { join } from "node:path";
import { unshownSettingUrl } from "@legion/contracts/setting-url";
import { rowLimit, runBounded } from "./bounded-subprocess";

// e2e/acceptance/ci-check.sh hands socket-scope.ts its DISPATCH_URL as `<server>`, and prints its
// stderr when it fails. That URL may carry a user and a password: a value that is no http or
// https URL with a host is refused naming its length and the cause the one function found (here,
// a value that does not parse and one with no http scheme), before the URL parser's error, which
// holds the value whole, can be printed. The password is made for this run.
test(
  "a <server> that is no http or https URL with a host is refused naming its length and cause",
  async () => {
    const password = crypto.randomUUID();
    const said: { readonly code: number; readonly output: string }[] = [];
    const values = [
      `http://made-up-user:${password}@[bad`,
      `made-up-user:${password}@dispatch.example`,
    ];
    for (const server of values) {
      const { code, stdout, stderr } = await runBounded({
        argv: [
          process.execPath,
          join(import.meta.dir, "..", "acceptance", "socket-scope.ts"),
          server,
          "ACME-1",
          "4",
          join(import.meta.dir, "no-such-cookie-file"),
        ],
        bound: "short",
        from: import.meta.url,
      });
      said.push({ code, output: stdout + stderr });
    }
    expect(said).toEqual(
      values.map((server) => ({
        code: 2,
        output: `<server> must be an http or https URL: ${unshownSettingUrl(server)}\n`,
      }))
    );
  },
  // One socket-scope.ts process for each value, one after the other.
  rowLimit("short", "short")
);
