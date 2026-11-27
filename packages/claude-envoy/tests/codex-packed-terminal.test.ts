import { afterAll, beforeAll, expect, test } from "bun:test"
import { mkdir, mkdtemp, readdir, rm, stat, symlink } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { STAND_IN_USER_CODE, stubDeviceLoginBroker } from "@legion/cipher-client/test/device-logins"

// The packed Codex plugin's `dispatch` command run in a person's terminal for the one thing it is
// shipped for: `dispatch login --device`, which waits for a person's approval longer than the
// plugin's `dispatch` tool holds a command. The package is built and packed as release.yaml's
// claude_envoy_build does it (scripts/build.ts --codex-out, then `bun pm pack`), unpacked where
// npm installs a package, and started three ways against a stand-in broker: through the link npm
// makes for the package bin, as the installed plugin's bin/dispatch, and as the install text's
// `bun <plugin>/dist/dispatch.js`. Each must start the device login and keep it, the same way.
const packageRoot = resolve(import.meta.dir, "..")

type Run = { readonly exitCode: number; readonly stdout: string; readonly stderr: string }

async function run(
  command: string[],
  cwd: string,
  env: Record<string, string | undefined> = process.env,
): Promise<Run> {
  const child = Bun.spawn(command, { cwd, env, stdout: "pipe", stderr: "pipe" })
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

let scratch = ""
/** The packed package unpacked where npm installs it. */
let installed = ""
/** The link npm makes for the package's `dispatch` bin. */
let npmLink = ""

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "codex-envoy-terminal-"))
  const out = join(scratch, "codex-envoy")
  const build = await run(
    [process.execPath, join(packageRoot, "scripts", "build.ts"), "--codex-out", out],
    scratch,
  )
  expect({ exitCode: build.exitCode, stderr: build.stderr }).toEqual({ exitCode: 0, stderr: "" })
  const packed = join(scratch, "packed")
  await mkdir(packed)
  const pack = await run([process.execPath, "pm", "pack", "--destination", packed], out)
  expect(pack.exitCode).toBe(0)
  const tarballs = (await readdir(packed)).filter((entry) => entry.endsWith(".tgz"))
  expect(tarballs).toHaveLength(1)
  const modules = join(scratch, "prefix", "node_modules")
  installed = join(modules, "@sjawhar", "codex-envoy")
  await mkdir(installed, { recursive: true })
  const untar = await run(
    ["tar", "-xzpf", join(packed, tarballs[0] as string), "-C", installed, "--strip-components=1"],
    scratch,
    { PATH: process.env["PATH"] ?? "" },
  )
  expect(untar.exitCode).toBe(0)
  await mkdir(join(modules, ".bin"))
  npmLink = join(modules, ".bin", "dispatch")
  await symlink(join("..", "@sjawhar", "codex-envoy", "bin", "dispatch"), npmLink)
}, 180_000)

afterAll(async () => {
  await rm(scratch, { recursive: true, force: true })
}, 60_000)

/** `command login --device --person ada@example.com` in a home of its own against a fresh
 *  stand-in broker, and what a person and the broker see of it: the broker's URL and every time
 *  written as placeholders, so two runs compare. */
async function deviceLogin(name: string, command: string[]) {
  const home = join(scratch, `home-${name}`)
  await mkdir(home)
  const broker = stubDeviceLoginBroker({ polls: ["pending", "approve"] })
  try {
    const result = await run(
      [...command, "login", "--device", "--person", "ada@example.com"],
      home,
      {
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        AGENT_SECRETS_URL: broker.url,
      },
    )
    const file = join(home, ".config", "legion", "device-login.json")
    return {
      exitCode: result.exitCode,
      stderr: result.stderr,
      stdout: result.stdout
        .replaceAll(broker.url, "<broker>")
        .replace(/\d{4}-\d\d-\d\dT[\d:.]+Z/g, "<time>"),
      authorizations: broker.authorizations.map((one) => one.form.get("scope")),
      polls: broker.polls.length,
      loginFileMode: await stat(file).then(
        (entry) => (entry.mode & 0o777).toString(8),
        () => "none",
      ),
    }
  } finally {
    broker.stop()
  }
}

test("the packed dispatch starts and keeps a device login, as the install text's bun dist/dispatch.js does", async () => {
  const viaNpmLink = await deviceLogin("npm-link", [npmLink])
  const viaPluginBin = await deviceLogin("plugin-bin", [join(installed, "bin", "dispatch")])
  const viaDistJs = await deviceLogin("dist-js", [
    process.execPath,
    join(installed, "dist", "dispatch.js"),
  ])
  expect({
    exitCode: viaNpmLink.exitCode,
    stderr: viaNpmLink.stderr,
    printsTheCode: viaNpmLink.stdout.includes(`and enter the code ${STAND_IN_USER_CODE}`),
    printsThePage: viaNpmLink.stdout.includes("open <broker>/credentials/machine"),
    approved: viaNpmLink.stdout.includes("dispatch login: approved."),
    authorizations: viaNpmLink.authorizations,
    polls: viaNpmLink.polls,
    loginFileMode: viaNpmLink.loginFileMode,
  }).toEqual({
    exitCode: 0,
    stderr: "",
    printsTheCode: true,
    printsThePage: true,
    approved: true,
    authorizations: ["machine"],
    polls: 2,
    loginFileMode: "600",
  })
  expect(viaPluginBin).toEqual(viaNpmLink)
  expect(viaDistJs).toEqual(viaNpmLink)
}, 60_000)

test("bin/dispatch started by a relative path from the package's directory runs under an exported CDPATH", async () => {
  // A shell profile may export CDPATH=.:<dirs>; a `cd` whose operand CDPATH resolves prints the
  // directory it entered, which a `$(cd … && pwd)` then holds as well.
  const result = await run(
    ["sh", "-c", 'cd "$1" && bin/dispatch --help', "sh", installed],
    scratch,
    {
      PATH: process.env["PATH"] ?? "",
      HOME: scratch,
      CDPATH: ".",
    },
  )
  expect({
    exitCode: result.exitCode,
    stderr: result.stderr,
    usage: result.stdout.startsWith("Usage: dispatch "),
  }).toEqual({ exitCode: 0, stderr: "", usage: true })
}, 60_000)
