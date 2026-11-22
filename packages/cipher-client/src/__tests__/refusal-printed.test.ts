import { expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { holdSecret } from "@legion/contracts/held-secrets";
import { BrokerRefusedError } from "../token-errors";

// The policy's own refusal prints as the broker wrote it, and a broker can echo the subject token
// in it. A host prints the error itself, not only its reason line: pi-envoy's console.warn of the
// error, and Bun's print of an uncaught one. The error's own text is scrubbed where it is made, so
// neither print carries the token (#4140, S1 at cb4949936a).
const LIBRARY =
  "The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed.";
// The broker's own audience refusal, its slot filled with the subject token it was sent.
const refusalEchoing = (subject: string) =>
  `${LIBRARY} The subject token was refused: the subject token was not minted for ${subject}, which is the audience this broker accepts from https://kubernetes.default.svc.`;

test("a refusal echoing the subject token prints none of it through console.warn", () => {
  const subject = holdSecret(`eyJ${randomBytes(48).toString("base64url")}`);
  const printed: string[] = [];
  const warn = console.warn;
  console.warn = (...args: unknown[]) => {
    printed.push(args.map((arg) => Bun.inspect(arg)).join(" "));
  };
  try {
    console.warn(
      "envoy: the session's token was not traded",
      new BrokerRefusedError(400, "invalid_request", refusalEchoing(subject), "probe")
    );
  } finally {
    console.warn = warn;
  }
  expect(printed.join("\n")).toContain("the subject token was not minted for <withheld>");
  expect(printed.join("\n")).not.toContain(subject.slice(0, 20));
});

test("a refusal echoing the subject token, thrown and uncaught, prints none of it", async () => {
  const subject = `eyJ${randomBytes(48).toString("base64url")}`;
  const dir = await mkdtemp(join(tmpdir(), "refusal-printed-"));
  const script = join(dir, "throw.ts");
  // The token reaches the process by its environment, as a secret does, never in its source: Bun
  // prints the source lines around an uncaught throw.
  await writeFile(
    script,
    [
      `import { holdSecret } from ${JSON.stringify(join(import.meta.dir, "../../../contracts/src/held-secrets.ts"))};`,
      `import { BrokerRefusedError } from ${JSON.stringify(join(import.meta.dir, "../token-errors.ts"))};`,
      'const subject = holdSecret(process.env.REFUSAL_SUBJECT ?? "");',
      `throw new BrokerRefusedError(400, "invalid_request", ${JSON.stringify(refusalEchoing("SUBJECT"))}.replace("SUBJECT", subject), "probe");`,
    ].join("\n")
  );
  const run = Bun.spawnSync(["bun", script], {
    stdout: "pipe",
    stderr: "pipe",
    env: { ...process.env, REFUSAL_SUBJECT: subject },
  });
  const printed = `${run.stdout.toString()}${run.stderr.toString()}`;
  expect(run.exitCode).not.toBe(0);
  expect(printed).toContain("BrokerRefusedError");
  expect(printed).not.toContain(subject.slice(0, 20));
}, 20_000);
