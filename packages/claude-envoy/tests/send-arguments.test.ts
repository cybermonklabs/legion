import { expect, test } from "bun:test"
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { resendCommand } from "../src/send-arguments"

test("parses a target session followed by the message words", async () => {
  const { parseSendArguments } = await import("../src/send-arguments")
  expect(parseSendArguments(["ses_target", "ready", "for", "QA"])).toEqual({
    targetSession: "ses_target",
    message: "ready for QA",
  })
})

test("rejects a missing target or an empty message", async () => {
  const { parseSendArguments } = await import("../src/send-arguments")
  expect(() => parseSendArguments(["ses_target"])).toThrow()
  expect(() => parseSendArguments(["", "hello"])).toThrow()
})

test("takes the key an unconfirmed send named, ahead of the target", async () => {
  const { parseSendArguments } = await import("../src/send-arguments")
  expect(
    parseSendArguments(["--idempotency-key", "k-1", "ses_receiver", "ready", "for", "QA"]),
  ).toEqual({ targetSession: "ses_receiver", message: "ready for QA", idempotencyKey: "k-1" })
  // A message whose words begin with the flag's name, after the target, is the message.
  expect(parseSendArguments(["ses_receiver", "--idempotency-key", "x"])).toEqual({
    targetSession: "ses_receiver",
    message: "--idempotency-key x",
  })
  expect(() => parseSendArguments(["--idempotency-key"])).toThrow()
})

// h2 (the review of 2fa6fbf7cf): a resend carries ENVOY_URL as it was set, and the scrub of held
// secrets takes out only a held value of 10 characters or more, so a URL whose user info is
// shorter printed whole. A URL setting prints only where it is a plain http(s) URL with no place
// a credential can sit (`isCleanUrl`), as every client names a configured URL; otherwise no
// command is printed, and no part of the URL.
test("a resend whose URL setting carries user info of any length prints no command", () => {
  const resend = {
    script: "envoy-send.ts",
    cwd: "/work",
    idempotencyKey: "k-1",
    targetSession: "ses_receiver",
    message: "hello",
  }
  for (const url of [
    "http://u:p@127.0.0.1:8080",
    "http://u@127.0.0.1:8080",
    "http://:p@127.0.0.1:8080",
    "http://user:p@ssw0rd",
    "http://127.0.0.1:8080/?token=x",
  ]) {
    const printed = resendCommand({ ...resend, settings: [["ENVOY_URL", url]] })
    expect(printed, url).toEqual({ unprinted: "ENVOY_URL" })
  }
  expect(resendCommand({ ...resend, settings: [["ENVOY_URL", "http://127.0.0.1:8080"]] })).toEqual({
    command:
      "cd /work && ENVOY_URL=http://127.0.0.1:8080 bun envoy-send.ts --idempotency-key k-1 ses_receiver hello",
  })
})

/** The zsh this row runs the resend command under: `LEGION_TEST_ZSH`, else the one on PATH.
 *  GitHub Actions' claude-envoy job installs one (`ci-job-claude-envoy.yaml`), so there a missing
 *  zsh fails the row rather than skipping it. */
const zsh = process.env["LEGION_TEST_ZSH"] ?? Bun.which("zsh")

/** A stand-in `bun` that writes the ENVOY_CONFIG_FILE it was given and then its arguments, each
 *  ended by a NUL, so the row reads back exactly what the shell handed it. */
function standInBun(): string {
  const bin = mkdtempSync(join(tmpdir(), "resend-bun-"))
  writeFileSync(join(bin, "bun"), `#!/bin/sh\nprintf '%s\\0' "$ENVOY_CONFIG_FILE" "$@"\n`)
  chmodSync(join(bin, "bun"), 0o755)
  return bin
}

/** Words each shell would expand or split if they stood bare: zsh's `=word`, quotes, command
 *  substitution, history, a leading dash, a tilde, globs, a line break, an empty message. */
const AWKWARD = [
  "=ls",
  "=",
  "a=b",
  "plain",
  "it's here; done",
  "$(id)",
  "`id`",
  "!x",
  "-n",
  "~",
  "*",
  "two\nlines",
  "",
]

/** Runs the resend command each awkward word makes through `argv`'s shell, and checks the stand-in
 *  `bun` got exactly the words the send had. */
async function sameWordsUnder(shell: string, argv: (command: string) => string[]): Promise<void> {
  const bin = standInBun()
  for (const word of AWKWARD) {
    const printed = resendCommand({
      script: "=envoy-send.ts",
      cwd: bin,
      settings: [["ENVOY_CONFIG_FILE", `=${word}`]],
      idempotencyKey: `=${word}`,
      targetSession: word || "ses_receiver",
      message: word,
    })
    if (!("command" in printed)) throw new Error(`no command was built for ${JSON.stringify(word)}`)
    const run = Bun.spawnSync(argv(printed.command), {
      env: { PATH: `${bin}:${process.env["PATH"] ?? ""}` },
      stdout: "pipe",
      stderr: "pipe",
    })
    expect(run.exitCode, `${shell} exited for ${JSON.stringify(word)}`).toBe(0)
    expect(run.stdout.toString().split("\0").slice(0, -1)).toEqual([
      `=${word}`,
      "=envoy-send.ts",
      "--idempotency-key",
      `=${word}`,
      word || "ses_receiver",
      word,
    ])
  }
}

// F7 (the review of 215510405b): the printed command must give the resend exactly the words the
// send had, under bash and under zsh alike. A bare leading `=` is read by zsh's EQUALS option as a
// command's path (`=ls` becomes `/usr/bin/ls`), so it is quoted.
test("the resend command gives the same words under bash", async () => {
  await sameWordsUnder("bash", (command) => ["bash", "-c", command])
})

// Skipped, and reported as skipped, where no zsh is found, except on GitHub Actions
// (`GITHUB_ACTIONS`), whose job installs one, so there a missing zsh fails the row. `CI` is no
// such sign: an agent's tool shell sets `CI=true` on a dev box with no zsh.
test.skipIf(zsh === null && process.env["GITHUB_ACTIONS"] !== "true")(
  "the resend command gives the same words under zsh",
  async () => {
    await sameWordsUnder("zsh", (command) => [zsh ?? "zsh", "-f", "-c", command])
  },
)
