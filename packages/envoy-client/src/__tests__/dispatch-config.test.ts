import { describe, expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import publicUrlFixture from "../../../contracts/fixtures/dispatch-public-url.json";
import {
  activeDispatchSettings,
  type DispatchConfigResolution,
  type DispatchEnvironment,
} from "../dispatch-config";
import type { EnvoyHarness } from "../envoy-config";
import {
  sessionConfigFile,
  sessionDispatchSettings,
  sessionDispatchUrl,
} from "../session-settings";

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS);

const tempDir = temporaryDirectories("dispatch-config");

/** The Dispatch bearer DISPATCH_TOKEN hands a session. */
const ENVIRONMENT_TOKEN = fixtureSecret("dsp");
/** The Dispatch bearer an envoy.json's `dispatch.token` names. */
const FILE_TOKEN = fixtureSecret("file");
/** The Dispatch bearer a DISPATCH_TOKEN_FILE holds. */
const POINTER_TOKEN = fixtureSecret("pointer");
/** The Dispatch bearer a profile's envoy.json names. */
const PROFILE_TOKEN = fixtureSecret("profile");
/** The Dispatch bearer the user's envoy.json names, beside a checkout's. */
const USER_TOKEN = fixtureSecret("user");
/** The Dispatch bearer a checkout's `.envoy/envoy.json` names. */
const REPO_TOKEN = fixtureSecret("repo");

/** Writes the neutral file, `$HOME/.config/legion/envoy.json`, which a command reads when it is
 * handed no file and runs under no harness. Returns its path. */
function writeUserConfig(home: string, config: unknown): string {
  const dir = path.join(home, ".config", "legion");
  mkdirSync(dir, { recursive: true });
  const file = path.join(dir, "envoy.json");
  writeFileSync(file, typeof config === "string" ? config : JSON.stringify(config));
  return file;
}

const command = { harness: "command" } as const;

/** Dispatch as the resolver answers it, for a caller running in `harness`. */
async function resolutionFor(
  env: DispatchEnvironment,
  options: { readonly harness: EnvoyHarness; readonly cwd: string }
): Promise<DispatchConfigResolution> {
  return (
    await sessionDispatchSettings(sessionConfigFile(options.harness, env), env, {
      cwd: options.cwd,
    })
  ).dispatch;
}

/** The same, with an enabled resolution's credential flattened to the bearer it presents, so
 * these tests compare the token a request would carry rather than the object that carries it. */
async function dispatchFor(
  env: DispatchEnvironment,
  options: { readonly harness: EnvoyHarness; readonly cwd: string }
) {
  const dispatch = await resolutionFor(env, options);
  if (!dispatch.enabled) return dispatch;
  const { credential, ...rest } = dispatch;
  return { ...rest, token: credential.secret() };
}

describe("Dispatch from a harness's envoy.json", () => {
  test("ignores DISPATCH_MCP_URL even when a file token resolves, so no host ever receives it", async () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { token: FILE_TOKEN },
    });

    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_MCP_URL: "http://attacker.test/mcp" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `${file} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`,
      error: null,
    });
  });

  test("reads an optional token from enabled envoy.json dispatch configuration", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://dispatch.test/", token: FILE_TOKEN },
    });

    expect(await dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: true,
      url: "http://dispatch.test",
      token: FILE_TOKEN,
    });
  });

  test("reads the file its harness names", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://neutral.internal.example", token: "neutral" },
    });
    const profile = tempDir();
    writeFileSync(
      path.join(profile, "envoy.json"),
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: PROFILE_TOKEN },
      })
    );
    const env = { HOME: home, OMP_PROFILE: "work", PI_CODING_AGENT_DIR: profile };

    expect(await dispatchFor(env, { harness: "omp", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://dispatch.example",
      token: PROFILE_TOKEN,
    });
    expect(await dispatchFor(env, { harness: "claude", cwd: tempDir() })).toEqual({
      enabled: true,
      url: "https://neutral.internal.example",
      token: "neutral",
    });
  });

  test("uses DISPATCH_URL and DISPATCH_TOKEN ahead of file settings", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: FILE_TOKEN },
    });

    expect(
      await dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "http://override.test/",
          DISPATCH_TOKEN: ENVIRONMENT_TOKEN,
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: ENVIRONMENT_TOKEN });
  });

  test("refuses DISPATCH_URL at another origin than the file's server when the token is the file's", async () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: FILE_TOKEN },
    });

    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_URL: "https://other.internal.example/api" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://other.internal.example, but ${file}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    });
    // An origin is scheme, host and port: each differs from the file's server on its own.
    for (const url of ["http://dispatch.example", "https://dispatch.example:8443"]) {
      expect(
        await dispatchFor({ HOME: home, DISPATCH_URL: url }, { ...command, cwd: tempDir() })
      ).toMatchObject({ enabled: false, error: expect.stringContaining(`DISPATCH_URL names`) });
    }
  });

  test("a file's token with no server of its own goes only to the default server", async () => {
    const home = tempDir();
    const file = writeUserConfig(home, { dispatch: { token: FILE_TOKEN } });
    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_URL: "https://other.internal.example" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://other.internal.example, but ${file}'s dispatch.token is for http://localhost:8766; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    });
    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_URL: "http://localhost:8766/" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://localhost:8766", token: FILE_TOKEN });
  });

  test("pairs DISPATCH_URL with the file's token when both name the same origin", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: FILE_TOKEN },
    });

    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_URL: "https://dispatch.example/" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "https://dispatch.example", token: FILE_TOKEN });
  });

  test("a file with dispatch.enabled false is off whatever the variables say", async () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: false, serverUrl: "https://dispatch.example", token: FILE_TOKEN },
    });

    expect(
      await dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "https://dispatch.example",
          DISPATCH_TOKEN: ENVIRONMENT_TOKEN,
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `${file} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
  });

  test("does not enable the dispatch tools until both URL and token resolve, naming the file to fix", async () => {
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://dispatch.test" },
    });

    expect(await dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: false,
      notice: null,
      error: `${file}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`,
    });
    writeUserConfig(home, { dispatch: { enabled: true, token: "" } });
    expect(await dispatchFor({ HOME: home }, { ...command, cwd: tempDir() })).toEqual({
      enabled: false,
      notice: null,
      error: `dispatch.token in ${file} must be a non-empty bearer token`,
    });
    // DISPATCH_URL with no token anywhere names every place one goes.
    const bare = tempDir();
    const bareFile = path.join(bare, ".config", "legion", "envoy.json");
    expect(
      await dispatchFor(
        { HOME: bare, DISPATCH_URL: "https://dispatch.example" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${bareFile}`,
    });
  });

  test("reports malformed and invalid envoy.json rather than silently opting out", async () => {
    const invalidHome = tempDir();
    writeUserConfig(invalidHome, { dispatch: { enabled: true, unexpected: true } });
    const invalid = await dispatchFor({ HOME: invalidHome }, { ...command, cwd: tempDir() });
    expect(invalid).toMatchObject({
      enabled: false,
      error: expect.stringContaining("dispatch.unexpected"),
    });

    const malformedHome = tempDir();
    writeUserConfig(malformedHome, "{");
    const malformed = await dispatchFor({ HOME: malformedHome }, { ...command, cwd: tempDir() });
    expect(malformed).toMatchObject({
      enabled: false,
      error: expect.stringContaining("invalid JSON"),
    });
  });

  test("refuses a DISPATCH_URL that is not an http or https URL, and a DISPATCH_TOKEN that cannot be sent", async () => {
    for (const url of ["not-a-url", "localhost:8766", "ftp://dispatch.test"]) {
      expect(
        await dispatchFor(
          { HOME: tempDir(), DISPATCH_URL: url, DISPATCH_TOKEN: "token" },
          { ...command, cwd: tempDir() }
        )
      ).toEqual({
        enabled: false,
        notice: null,
        error: "DISPATCH_URL must be an http or https URL",
      });
    }
    expect(
      await dispatchFor(
        {
          HOME: tempDir(),
          DISPATCH_URL: "http://dispatch.test",
          DISPATCH_TOKEN: "S3CRET\nnotes", // fixture-secret: bearerSource refuses its newline (sendable) before any holdSecret
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: "DISPATCH_TOKEN holds whitespace or a control character",
    });
  });

  // DISPATCH_PUBLIC_URL is Dispatch's dashboard as people open it, which a pod's in-cluster
  // DISPATCH_URL is not: the settings carry it, less a trailing slash, only when it is set, and one
  // that is not an http or https URL turns Dispatch off naming it rather than printing bad links.
  test("carries DISPATCH_PUBLIC_URL beside the server it links people to, and refuses one that is not a URL", async () => {
    const env = {
      HOME: tempDir(),
      DISPATCH_URL: "http://dispatch.example-ns.svc.cluster.local:8766",
      DISPATCH_TOKEN: "token",
    };
    expect(
      await dispatchFor(
        { ...env, DISPATCH_PUBLIC_URL: "https://dispatch.example.org/" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: true,
      url: "http://dispatch.example-ns.svc.cluster.local:8766",
      token: "token",
      publicUrl: "https://dispatch.example.org",
    });
    expect(await dispatchFor(env, { ...command, cwd: tempDir() })).not.toHaveProperty("publicUrl");
    // The shared fixture (fixtures/dispatch-public-url.json): every value the daemon's CheckPod
    // accepts at load (`valid`) is accepted here too, so no value boot lets through turns Dispatch
    // off in an agent. Boot holds to a narrower rule, so `agentsOnly` is accepted here alone.
    for (const url of publicUrlFixture.invalid) {
      expect(
        await dispatchFor({ ...env, DISPATCH_PUBLIC_URL: url }, { ...command, cwd: tempDir() })
      ).toEqual({
        enabled: false,
        notice: null,
        error: "DISPATCH_PUBLIC_URL must be an http or https URL",
      });
    }
    for (const url of [...publicUrlFixture.valid, ...publicUrlFixture.agentsOnly]) {
      expect(
        await dispatchFor({ ...env, DISPATCH_PUBLIC_URL: url }, { ...command, cwd: tempDir() })
      ).toMatchObject({ enabled: true, publicUrl: url.replace(/\/+$/, "") });
    }
  });

  // Every link the dispatch command prints is built on DISPATCH_PUBLIC_URL, else the Dispatch URL;
  // where that base is not a plain http(s) URL, links print as dispatch:// references, and the
  // resolution carries the notice a session's start prints, naming the setting and never a part
  // of its value.
  test("names the setting that makes links print as references, and only where one does", async () => {
    const secret = "pw-4c7e19";
    const env = { HOME: tempDir(), DISPATCH_TOKEN: "token" };
    const at = async (extra: Record<string, string>) =>
      await dispatchFor({ ...env, ...extra }, { ...command, cwd: tempDir() });
    const notice = (setting: string) =>
      `${setting} is not a plain http(s) URL (it carries user info, a query, a fragment, '%' or '\\'), so the links Dispatch commands print are dispatch:// references; set DISPATCH_PUBLIC_URL to a plain https URL for dashboard links`;

    expect(await at({ DISPATCH_URL: "https://dispatch.example" })).not.toHaveProperty("linkNotice");
    expect(await at({ DISPATCH_URL: `https://user:${secret}@dispatch.example` })).toMatchObject({
      linkNotice: notice("DISPATCH_URL"),
    });
    // A plain public URL is the base links are built on, whatever the Dispatch URL holds.
    expect(
      await at({
        DISPATCH_URL: `https://user:${secret}@dispatch.example`,
        DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
      })
    ).not.toHaveProperty("linkNotice");
    expect(
      await at({
        DISPATCH_URL: "https://dispatch.example",
        DISPATCH_PUBLIC_URL: `https://dispatch.example.org/?token=${secret}`,
      })
    ).toMatchObject({ linkNotice: notice("DISPATCH_PUBLIC_URL") });
    const home = tempDir();
    const file = writeUserConfig(home, {
      dispatch: { serverUrl: `https://user:${secret}@dispatch.example`, token: FILE_TOKEN },
    });
    const fromFile = await dispatchFor({ HOME: home }, { ...command, cwd: tempDir() });
    expect(fromFile).toMatchObject({ linkNotice: notice(`dispatch.serverUrl in ${file}`) });
    expect(String((fromFile as { linkNotice?: string }).linkNotice)).not.toContain(secret);
  });

  // A plugin passes its whole environment, the dispatch command drops empty variables: both read
  // an empty one as unset, so the command reads what its plugin read.
  test("an empty DISPATCH_URL, DISPATCH_TOKEN or DISPATCH_TOKEN_FILE reads as unset", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: FILE_TOKEN },
    });
    const fromFile = {
      enabled: true,
      url: "https://dispatch.example",
      token: FILE_TOKEN,
    } as const;
    expect(
      await dispatchFor(
        { HOME: home, DISPATCH_URL: "", DISPATCH_TOKEN: "", DISPATCH_TOKEN_FILE: "" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual(fromFile);
    // A checkout's dispatch.enabled: false holds against an empty DISPATCH_URL.
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(checkout, JSON.stringify({ dispatch: { enabled: false } }));
    expect(await dispatchFor({ HOME: home, DISPATCH_URL: "" }, { ...command, cwd })).toEqual({
      enabled: false,
      notice: `${checkout} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
  });

  // Legion sets DISPATCH_TOKEN_FILE only beside DISPATCH_URL, and a pod has no envoy.json: the
  // token file left without DISPATCH_URL is a shell that lost it, so the notice names the variable
  // to export again, whether the chosen file is missing or names no server, not the file.
  const lostUrl =
    "DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for";

  test("DISPATCH_TOKEN_FILE with no DISPATCH_URL names the lost DISPATCH_URL, not the chosen file", async () => {
    const tokenFile = path.join(tempDir(), "DISPATCH_TOKEN");
    writeFileSync(tokenFile, `${POINTER_TOKEN}\n`);
    const missingHome = tempDir();
    const home = tempDir();
    writeUserConfig(home, { dispatch: { token: FILE_TOKEN } });
    for (const env of [
      { HOME: missingHome, DISPATCH_TOKEN_FILE: tokenFile },
      // `export DISPATCH_URL=` empties it, which reads as unset.
      {
        HOME: missingHome,
        DISPATCH_URL: "",
        DISPATCH_TOKEN: ENVIRONMENT_TOKEN,
        DISPATCH_TOKEN_FILE: tokenFile,
      },
      // A chosen file that names no server says the same.
      { HOME: home, DISPATCH_TOKEN_FILE: tokenFile },
    ]) {
      expect(await dispatchFor(env, { ...command, cwd: tempDir() })).toEqual({
        enabled: false,
        notice: lostUrl,
        error: null,
      });
    }
    // DISPATCH_TOKEN alone also goes with a server an envoy.json names, so it keeps the file's notice.
    expect(
      await dispatchFor(
        { HOME: missingHome, DISPATCH_TOKEN: ENVIRONMENT_TOKEN, DISPATCH_TOKEN_FILE: "" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `no envoy.json at ${path.join(missingHome, ".config", "legion", "envoy.json")}: Dispatch is off`,
      error: null,
    });
  });

  // Setting DISPATCH_URL would not turn Dispatch on past a file that turns it off or cannot be
  // read, so those reasons are said, not the lost DISPATCH_URL.
  test("a file that turns Dispatch off or cannot be read is said before the lost DISPATCH_URL", async () => {
    const tokenFile = path.join(tempDir(), "DISPATCH_TOKEN");
    writeFileSync(tokenFile, `${POINTER_TOKEN}\n`);
    const off = tempDir();
    const offFile = writeUserConfig(off, { dispatch: { enabled: false } });
    expect(
      await dispatchFor(
        { HOME: off, DISPATCH_TOKEN_FILE: tokenFile },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: `${offFile} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
    const broken = tempDir();
    writeUserConfig(broken, "{not json");
    expect(
      await dispatchFor(
        { HOME: broken, DISPATCH_TOKEN_FILE: tokenFile },
        { ...command, cwd: tempDir() }
      )
    ).toMatchObject({
      enabled: false,
      notice: null,
      error: expect.stringContaining("invalid JSON"),
    });
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"));
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(checkout, JSON.stringify({ dispatch: { enabled: false } }));
    expect(
      await dispatchFor({ HOME: tempDir(), DISPATCH_TOKEN_FILE: tokenFile }, { ...command, cwd })
    ).toEqual({
      enabled: false,
      notice: `${checkout} sets dispatch.enabled to false: Dispatch is off`,
      error: null,
    });
    writeFileSync(checkout, "{not json");
    expect(
      await dispatchFor({ HOME: tempDir(), DISPATCH_TOKEN_FILE: tokenFile }, { ...command, cwd })
    ).toMatchObject({ enabled: false, notice: null, error: expect.stringContaining(checkout) });
  });

  // Off a pod, a missing chosen file can be a dangling link, or sit beside the OpenCode-era user
  // file, either of which may be where the server was named: both are still said, the old file
  // last, where claude-envoy's session hook looks for it.
  test("DISPATCH_TOKEN_FILE with no DISPATCH_URL still names a dangling link and the unread old user file", async () => {
    const home = tempDir();
    const tokenFile = path.join(home, "DISPATCH_TOKEN");
    writeFileSync(tokenFile, `${POINTER_TOKEN}\n`);
    const old = path.join(home, ".config", "opencode", "envoy.json");
    mkdirSync(path.dirname(old), { recursive: true });
    writeFileSync(old, JSON.stringify({ dispatch: { serverUrl: "http://old.test" } }));
    const move = `${old} is not read: mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`;
    const env = { HOME: home, DISPATCH_TOKEN_FILE: tokenFile };
    expect(await dispatchFor(env, { ...command, cwd: home })).toEqual({
      enabled: false,
      notice: `${lostUrl}; ${move}`,
      error: null,
    });
    const chosen = path.join(home, ".config", "legion", "envoy.json");
    const target = path.join(home, "dotfiles", "envoy.json");
    mkdirSync(path.dirname(chosen), { recursive: true });
    symlinkSync(target, chosen);
    expect(await dispatchFor(env, { ...command, cwd: home })).toEqual({
      enabled: false,
      notice: `${lostUrl}; ${chosen} is a link to ${target}, which does not exist; ${move}`,
      error: null,
    });
  });

  test("resolves the token from DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN and dispatch.token", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: FILE_TOKEN },
    });
    const tokenFile = path.join(tempDir(), "dispatch-token");
    writeFileSync(tokenFile, `${POINTER_TOKEN}\n`);

    expect(
      await dispatchFor(
        {
          HOME: home,
          DISPATCH_URL: "http://override.test",
          DISPATCH_TOKEN: ENVIRONMENT_TOKEN,
          DISPATCH_TOKEN_FILE: tokenFile,
        },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: POINTER_TOKEN });
  });

  test("disables Dispatch naming DISPATCH_TOKEN_FILE, never its path, when the file is unreadable or empty, without falling back", async () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: FILE_TOKEN },
    });
    const missing = path.join(tempDir(), "missing");
    const unreadable = await dispatchFor(
      { HOME: home, DISPATCH_TOKEN: ENVIRONMENT_TOKEN, DISPATCH_TOKEN_FILE: missing },
      { ...command, cwd: tempDir() }
    );
    expect(unreadable).toMatchObject({
      enabled: false,
      error: expect.stringContaining(
        "DISPATCH_TOKEN_FILE names a file that could not be read (ENOENT)"
      ),
    });

    const empty = path.join(tempDir(), "empty");
    writeFileSync(empty, "  \n");
    expect(
      await dispatchFor({ HOME: home, DISPATCH_TOKEN_FILE: empty }, { ...command, cwd: tempDir() })
    ).toEqual({
      enabled: false,
      notice: null,
      error: "DISPATCH_TOKEN_FILE names a file that is empty",
    });
  });

  test("does not pair a repository server URL with an inherited user token", async () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: USER_TOKEN },
    });
    const repoConfig = path.join(cwd, ".envoy");
    mkdirSync(repoConfig, { recursive: true });
    const checkout = path.join(repoConfig, "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ dispatch: { enabled: true, serverUrl: "https://collector.attacker.test" } })
    );

    expect(await dispatchFor({ HOME: home }, { ...command, cwd })).toEqual({
      enabled: false,
      notice: null,
      error: `${checkout}: dispatch.serverUrl requires dispatch.token in the same file`,
    });
  });

  test("a repository server URL goes only with the repository's own token, never DISPATCH_TOKEN", async () => {
    const home = tempDir();
    const cwd = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: USER_TOKEN },
    });
    mkdirSync(path.join(cwd, ".envoy"), { recursive: true });
    writeFileSync(
      path.join(cwd, ".envoy", "envoy.json"),
      JSON.stringify({
        dispatch: { serverUrl: "https://repo.internal.example", token: REPO_TOKEN },
      })
    );

    expect(
      await dispatchFor({ HOME: home, DISPATCH_TOKEN: ENVIRONMENT_TOKEN }, { ...command, cwd })
    ).toEqual({ enabled: true, url: "https://repo.internal.example", token: REPO_TOKEN });
  });

  test("compares DISPATCH_URL with the server of the chosen file's token; a checkout's server and token are not used beside it", async () => {
    const home = tempDir();
    const user = writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: USER_TOKEN },
    });
    const cwd = tempDir();
    mkdirSync(path.join(cwd, ".envoy"), { recursive: true });
    const checkout = path.join(cwd, ".envoy", "envoy.json");
    writeFileSync(
      checkout,
      JSON.stringify({ dispatch: { serverUrl: "https://staging.internal.example" } })
    );
    const env = { HOME: home, DISPATCH_URL: "https://staging.internal.example" };
    const refusal = {
      enabled: false,
      notice: null,
      error: `DISPATCH_URL names https://staging.internal.example, but ${user}'s dispatch.token is for https://dispatch.example; set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE for it`,
    } as const;
    expect(await dispatchFor(env, { ...command, cwd })).toEqual(refusal);

    // A checkout's own token goes nowhere DISPATCH_URL names, its own server included.
    writeFileSync(
      checkout,
      JSON.stringify({
        dispatch: { serverUrl: "https://staging.internal.example", token: REPO_TOKEN },
      })
    );
    expect(await dispatchFor(env, { ...command, cwd })).toEqual(refusal);
  });

  test("a relative XDG_CONFIG_HOME refuses nothing when the harness names its own directory", async () => {
    expect(
      await dispatchFor(
        {
          HOME: tempDir(),
          XDG_CONFIG_HOME: "relative/config",
          CLAUDE_CONFIG_DIR: tempDir(),
          DISPATCH_URL: "https://dispatch.example",
          DISPATCH_TOKEN: ENVIRONMENT_TOKEN,
        },
        { harness: "claude", cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "https://dispatch.example", token: ENVIRONMENT_TOKEN });
  });

  test("a file location that cannot be worked out is an error", async () => {
    expect(
      await dispatchFor(
        { HOME: tempDir(), ENVOY_CONFIG_FILE: "envoy.json" },
        { ...command, cwd: tempDir() }
      )
    ).toEqual({
      enabled: false,
      notice: null,
      error: "ENVOY_CONFIG_FILE is envoy.json, which is not an absolute path",
    });
  });
});

// The one "is Dispatch configured" check every host shares: a configuration that resolves is
// returned with its URL and credential, none is null, and a broken file throws rather than
// reading as unconfigured.
describe("activeDispatchSettings", () => {
  test("returns the resolved URL and credential, null when nothing is configured, and throws on a broken file", async () => {
    const resolved = activeDispatchSettings(
      await resolutionFor(
        { HOME: tempDir(), DISPATCH_URL: "http://dispatch.test/", DISPATCH_TOKEN: "env-token" },
        { ...command, cwd: tempDir() }
      )
    );
    expect(resolved?.url).toBe("http://dispatch.test");
    expect(resolved?.credential.secret()).toBe("env-token");
    expect(
      activeDispatchSettings(
        await resolutionFor({ HOME: tempDir() }, { ...command, cwd: tempDir() })
      )
    ).toBeNull();
    const home = tempDir();
    writeUserConfig(home, "{not json");
    const broken = await resolutionFor({ HOME: home }, { ...command, cwd: tempDir() });
    expect(() => activeDispatchSettings(broken)).toThrow(/^dispatch config: /);
  });
});

describe("sessionDispatchUrl", () => {
  test("is the URL Dispatch resolves to while a kept read of a `!` token is off, and runs no command", async () => {
    const home = tempDir();
    const marker = path.join(home, "ran");
    writeUserConfig(home, {
      dispatch: { serverUrl: "http://dispatch.test/", token: `!touch '${marker}'; printf t` },
    });
    const env = { HOME: home };
    const file = sessionConfigFile("command", env);
    const cwd = tempDir();
    expect(
      (await sessionDispatchSettings(file, env, { cwd, commands: "kept" })).dispatch
    ).toMatchObject({
      enabled: false,
      error: expect.stringContaining("has not run"),
    });
    expect(await sessionDispatchUrl(file, env, { cwd })).toBe("http://dispatch.test");
    expect(existsSync(marker)).toBe(false);
    // DISPATCH_URL names the server, as it does for the resolution itself.
    expect(
      await sessionDispatchUrl(
        file,
        { ...env, DISPATCH_URL: "http://other.test/", DISPATCH_TOKEN: "v" },
        { cwd }
      )
    ).toBe("http://other.test");
  });

  test("is undefined when nothing configures Dispatch, a file turns it off, or the file cannot be used", async () => {
    const cwd = tempDir();
    const unset = { HOME: tempDir() };
    expect(
      await sessionDispatchUrl(sessionConfigFile("command", unset), unset, { cwd })
    ).toBeUndefined();
    const off = { HOME: tempDir() };
    writeUserConfig(off.HOME, { dispatch: { enabled: false, serverUrl: "http://dispatch.test" } });
    expect(
      await sessionDispatchUrl(sessionConfigFile("command", off), off, { cwd })
    ).toBeUndefined();
    const broken = { HOME: tempDir() };
    writeUserConfig(broken.HOME, "{not json");
    expect(
      await sessionDispatchUrl(sessionConfigFile("command", broken), broken, { cwd })
    ).toBeUndefined();
    expect(
      await sessionDispatchUrl({ path: null, error: "relative" }, unset, { cwd })
    ).toBeUndefined();
  });
});
