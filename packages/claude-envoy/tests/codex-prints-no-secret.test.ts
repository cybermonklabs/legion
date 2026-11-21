// The Codex plugin's MCP server prints no secret it holds: a broker at the wrong URL that echoes
// the subject token it was sent in its refusal, whole or as a run of 10 or more of its characters,
// reaches the server's stderr as `<withheld>` (`@legion/contracts/held-secrets`). The server runs
// in a process of its own as the `codex` process starts it (tests/codex-server-process.ts), told
// `NATS_AUTH=broker`, so it trades its pod's identity for the session's token before it dials.
import { afterEach, describe, expect, test } from "bun:test"
import { writeFileSync } from "node:fs"
import { join } from "node:path"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { codexHomes, recordFor, startCodexServer, until } from "./codex-server-process"

const stops: (() => Promise<void> | void)[] = []
// Registered before the directories' removal below, which bun runs after it: a server still alive
// in its directory slows that removal past the removal hook's 5 s. Twenty seconds here, since a
// server's stop alone may take 5 s, SIGTERM to its SIGKILL (`CodexServer.stop`).
afterEach(async () => {
  for (const stop of stops.splice(0).reverse()) await stop()
}, 20_000)
const makeDirectory = temporaryDirectories("codex-prints-no-secret")

/** The refusal the broker's own library writes for a subject token minted for another audience,
 *  one of the refusals a client prints as written (`KNOWN_BROKER_REFUSALS`), naming `shown`. */
const refusal = (shown: string): string =>
  `The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed. The subject token was refused: the subject token was not minted for ${shown}, which is the audience this broker accepts from https://kubernetes.default.svc.`

describe("the Codex server's stderr", () => {
  test.each([
    "whole",
    "a run of it",
  ] as const)("shows a broker's refusal echoing the subject token (%s) with the token withheld", async (form) => {
    const root = makeDirectory()
    const subject = fixtureSecret("pod")
    const shown = form === "whole" ? subject : subject.slice(4, 18)
    const broker = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: () =>
        Response.json(
          { error: "invalid_request", error_description: refusal(shown) },
          { status: 400 },
        ),
    })
    stops.push(() => broker.stop(true))
    const homes = codexHomes(root, {
      enabled: true,
      url: "http://127.0.0.1:9",
      // The trade fails before any dial, so no hub answers here.
      natsUrls: ["nats://127.0.0.1:9"],
    })
    const tokenFile = join(root, "pod-token")
    writeFileSync(tokenFile, `${subject}\n`)
    await recordFor(homes.pluginData, crypto.randomUUID(), homes)
    const server = startCodexServer(
      {
        HOME: homes.home,
        PLUGIN_DATA: homes.pluginData,
        AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
        AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
        AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
        NATS_AUTH: "broker",
      },
      root,
    )
    stops.push(server.stop)
    await until(
      () => server.stderr().includes("the subject token was not minted for"),
      "the server to print the broker's refusal",
      30_000,
      server.stderr,
    )
    const printed = server.stderr()
    expect(printed).toContain("the subject token was not minted for <withheld>")
    expect(printed).not.toContain(shown)
  }, 60_000)
})
