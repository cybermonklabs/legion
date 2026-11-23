import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdirSync, utimesSync } from "node:fs";
import { createConnection, createServer, type Server, type Socket } from "node:net";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import {
  browserLauncher,
  browserNetwork,
  browserNetworkVariable,
  chromiumExecutable,
  chromiumLaunchOptions,
  guardChromiumLaunches,
  guardedChromium,
  launchPrefix,
  loopbackAddress,
  loopbackSocksServer,
  proxyPort,
  resolverMaps,
  sweepStaleLaunches,
} from "../browser-namespace";
import { rowLimit, runBounded } from "./bounded-subprocess";

// A local run's Chromium gets a network namespace of its own and reaches the host's loopback
// through a SOCKS5 proxy (e2e/browser-namespace.ts). These rows hold the choice of network, the
// projects that launch through it, the proxy's host half (what it opens, what it refuses, and the
// bytes it carries) and the sweep of directories earlier launches left. Chromium in the namespace
// itself needs an unprivileged user namespace, which no row here asks of the machine it runs on,
// so the forcing that shows the namespace keeping a request is run by hand (the pull request that
// brought this file names it).

const directory = temporaryDirectories("browser-namespace");

const opened: Server[] = [];
afterEach(() => {
  for (const server of opened.splice(0)) server.close();
});

async function listening(server: Server): Promise<number> {
  opened.push(server);
  const { promise, resolve } = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", resolve);
  await promise;
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("no TCP port");
  return address.port;
}

/** An upstream on the host's loopback that answers each line it reads with `echo <line>`. */
function echoServer(): Promise<number> {
  return listening(
    createServer((socket) => {
      socket.setEncoding("utf8");
      socket.on("data", (chunk: string) => socket.write(`echo ${chunk}`));
    })
  );
}

/** Reads from `socket` until `bytes` have come, or it ends. */
async function readBytes(socket: Socket, bytes: number): Promise<Buffer> {
  let read = Buffer.alloc(0);
  const { promise, resolve } = Promise.withResolvers<Buffer>();
  const take = (chunk: Buffer) => {
    read = Buffer.concat([read, chunk]);
    if (read.length >= bytes) resolve(read);
  };
  socket.on("data", take);
  socket.once("end", () => resolve(read));
  socket.once("close", () => resolve(read));
  const done = await promise;
  socket.off("data", take);
  return done;
}

/** A SOCKS5 CONNECT through the proxy on `proxy` to `host` (an IPv4 address or a name) and
 *  `port`: the reply's code, and the socket, joined to the upstream on success. */
async function connectThrough(
  proxy: number,
  host: string,
  port: number
): Promise<{ code: number | undefined; socket: Socket }> {
  const socket = createConnection({ host: "127.0.0.1", port: proxy });
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  socket.once("connect", resolve);
  socket.once("error", reject);
  await promise;
  socket.write(Buffer.from([5, 1, 0]));
  expect([...(await readBytes(socket, 2))]).toEqual([5, 0]);
  const address = /^\d+\.\d+\.\d+\.\d+$/.test(host)
    ? Buffer.from([1, ...host.split(".").map(Number)])
    : Buffer.concat([Buffer.from([3, host.length]), Buffer.from(host, "latin1")]);
  const portBytes = Buffer.alloc(2);
  portBytes.writeUInt16BE(port);
  socket.write(Buffer.concat([Buffer.from([5, 1, 0]), address, portBytes]));
  const answer = await readBytes(socket, 10);
  return { code: answer[1], socket };
}

const makes = () => undefined;
const refuses = () => "unshare: write failed /proc/self/uid_map: Operation not permitted";

test("the browser's network is its own where this machine makes one, unless CI or the variable says otherwise, and no other value is read", () => {
  expect(browserNetwork({ TEST_WORKER_INDEX: "0" }, makes)).toBe("own");
  expect(browserNetwork({ TEST_WORKER_INDEX: "0" }, refuses)).toBe("host");
  expect(browserNetwork({ CI: "true" }, makes)).toBe("host");
  expect(browserNetwork({ CI: "true", [browserNetworkVariable]: "own" }, refuses)).toBe("own");
  expect(browserNetwork({ [browserNetworkVariable]: "host" }, makes)).toBe("host");
  expect(() => browserNetwork({ [browserNetworkVariable]: "none" }, makes)).toThrow(
    `${browserNetworkVariable} is "none"`
  );
});

// The main process decides, and a worker reads its decision: a worker inherits the main process's
// environment, so the decision written there is the one every worker takes, with no probe of its
// own that could fall back without a word.
test("a decision is written into the environment, and read from it after", () => {
  const probed: string[] = [];
  const refusing = () => {
    probed.push("probe");
    return "refused";
  };
  const env: NodeJS.ProcessEnv = {};
  expect(browserNetwork(env, refusing)).toBe("host");
  expect(env[browserNetworkVariable]).toBe("host");
  expect(browserNetwork(env, makes)).toBe("host");
  expect(probed).toEqual(["probe"]);
  const made: NodeJS.ProcessEnv = {};
  expect(browserNetwork(made, makes)).toBe("own");
  expect(made[browserNetworkVariable]).toBe("own");
  const onCI: NodeJS.ProcessEnv = { CI: "true" };
  browserNetwork(onCI, makes);
  expect(onCI[browserNetworkVariable]).toBeUndefined();
});

/** A launch's directory under `root`, its socket `answering` or not, its time set `ageMs` ago. */
async function launchDirectory(root: string, name: string, answering: boolean, ageMs: number) {
  const path = join(root, `${launchPrefix}${name}`);
  mkdirSync(path);
  if (answering) {
    const server = createServer();
    opened.push(server);
    const { promise, resolve } = Promise.withResolvers<void>();
    server.listen(join(path, String(proxyPort)), resolve);
    await promise;
  }
  const at = new Date(Date.now() - ageMs);
  utimesSync(path, at, at);
  return path;
}

test(
  "a launch sweeps the directories earlier launches left, and no running or new one",
  async () => {
    const root = directory();
    const left = await launchDirectory(root, "left", false, 120_000);
    const running = await launchDirectory(root, "running", true, 120_000);
    const fresh = await launchDirectory(root, "fresh", false, 0);
    const other = join(root, "someone-elses");
    mkdirSync(other);
    await sweepStaleLaunches(root);
    expect({
      fresh: existsSync(fresh),
      left: existsSync(left),
      other: existsSync(other),
      running: existsSync(running),
    }).toEqual({ fresh: true, left: false, other: true, running: true });
  },
  rowLimit("short")
);

test("a Chromium launch names the launcher where the network is its own, and keeps its arguments", () => {
  const args = ["--enable-precise-memory-info"];
  expect(chromiumLaunchOptions({ args }, { [browserNetworkVariable]: "own" })).toEqual({
    args,
    executablePath: browserLauncher,
  });
  expect(chromiumLaunchOptions({ args }, { [browserNetworkVariable]: "host" })).toEqual({ args });
});

/** A stand-in for Playwright's Chromium `BrowserType` whose `browser` fixture filled
 *  `_defaultLaunchOptions` with `defaults`: it records each launch it is asked for. */
function standInChromium(defaults: object | undefined) {
  const launched: string[] = [];
  return {
    _playwright: { _defaultLaunchOptions: defaults },
    launch: async (options?: { executablePath?: string }) => {
      launched.push(`launch ${options?.executablePath ?? "-"}`);
    },
    launchPersistentContext: async (directory: string) => {
      launched.push(`persistent ${directory}`);
    },
    launchServer: async (options?: { executablePath?: string }) => {
      launched.push(`server ${options?.executablePath ?? "-"}`);
    },
    launched,
  };
}

// A row's or a file's `launchOptions` replace the project's whole; the guard catches one written
// without `chromiumLaunchOptions` at the launch, by name, where the network is the browser's own.
test("a Chromium launch that does not start through the launcher fails by name where the network is its own", async () => {
  const own = { [browserNetworkVariable]: "own" };
  const bypassed = standInChromium({ args: ["--enable-precise-memory-info"], handleSIGINT: false });
  guardChromiumLaunches(bypassed, own);
  await expect(bypassed.launch()).rejects.toThrow(
    'e2e: a Chromium launch did not start through e2e/browser-namespace.sh (executablePath null) while DISPATCH_E2E_BROWSER_NETWORK is "own"'
  );
  await expect(bypassed.launchPersistentContext("profile")).rejects.toThrow(
    "did not start through"
  );
  await expect(bypassed.launch({ executablePath: "/usr/bin/chromium" })).rejects.toThrow(
    '(executablePath "/usr/bin/chromium")'
  );
  // A server's browser is one a row reaches with `connect`, so it is held to the same rule.
  await expect(bypassed.launchServer({ executablePath: "/usr/bin/chromium" })).rejects.toThrow(
    '(executablePath "/usr/bin/chromium")'
  );
  await expect(bypassed.launchServer()).rejects.toThrow("(executablePath null)");
  expect(bypassed.launched).toEqual([]);

  const routed = standInChromium({ executablePath: browserLauncher });
  guardChromiumLaunches(routed, own);
  await routed.launch();
  await routed.launch({ executablePath: browserLauncher });
  await routed.launchServer();
  expect(routed.launched).toEqual([`launch -`, `launch ${browserLauncher}`, "server -"]);

  const onHost = standInChromium({ args: [] });
  guardChromiumLaunches(onHost, { [browserNetworkVariable]: "host" });
  await onHost.launch();
  expect(onHost.launched).toEqual(["launch -"]);

  const unreadable = {
    launch: async () => {},
    launchPersistentContext: async () => {},
    launchServer: async () => {},
  };
  guardChromiumLaunches(unreadable, own);
  await expect(unreadable.launch()).rejects.toThrow("has no `_playwright`");
});

// The guard wraps the object the `browser` fixture launches from - Playwright's own Chromium,
// which `@playwright/test` exports too - and reads the defaults where that Chromium's own `launch`
// and `launchServer` read them. A Playwright that merged them anywhere else would leave the guard
// reading none, and every routed launch would then fail by name; this row says so before a run does.
test("the guard reaches Playwright's own Chromium, whose launches read the default options where the guard does", async () => {
  const chromium = guardedChromium();
  const { chromium: exported } = await import("@playwright/test");
  expect(chromium).toBe(exported);
  expect(typeof Reflect.get(chromium, "_playwright")).toBe("object");
  for (const call of ["launch", "launchServer"]) {
    const own: unknown = Reflect.get(Object.getPrototypeOf(chromium), call);
    expect({
      call,
      merges: String(own).includes("...this._playwright._defaultLaunchOptions, ...options"),
    }).toEqual({
      call,
      merges: true,
    });
  }
});

// Every Chromium project the config runs on this machine's network launches through the
// launcher, and none else does: the visual projects run in a container with no network of its
// own, and the launcher starts Chromium alone. The config is read in a child, as Playwright reads
// it (the import is that child's own), with the namespace named and with CI set and nothing named.
test(
  "the config launches every Chromium project but the visual ones through the launcher, and none on CI",
  async () => {
    const read = async (extra: Record<string, string>) => {
      const run = await runBounded({
        argv: [
          process.execPath,
          "-e",
          'const c = (await import("./e2e/playwright.config.ts")).default; console.log(JSON.stringify(c.projects.map((p) => [p.name, p.use?.browserName ?? p.use?.defaultBrowserType, p.use?.launchOptions?.executablePath ?? null])));',
        ],
        bound: "short",
        cwd: new URL("../..", import.meta.url).pathname,
        env: { HOME: process.env.HOME, PATH: process.env.PATH, ...extra },
        from: import.meta.url,
      });
      expect(run.code).toBe(0);
      return JSON.parse(run.stdout) as [string, string, string | null][];
    };
    const own = await read({ [browserNetworkVariable]: "own" });
    const launched = own.filter(([, , path]) => path === browserLauncher).map(([name]) => name);
    expect(launched.sort()).toEqual(
      own
        .filter(([name, engine]) => engine === "chromium" && !name.startsWith("visual"))
        .map(([name]) => name)
        .sort()
    );
    expect(launched).toContain("chromium");
    expect(launched).toContain("stub");
    expect((await read({ CI: "true" })).filter(([, , path]) => path !== null)).toEqual([]);
  },
  rowLimit("short", "short")
);

// The guard is only as good as the config line that installs it: the rows above call
// `guardChromiumLaunches` themselves, so they pass with that line gone. This one loads the config
// in a child, as Playwright's processes load it, and then asks the Chromium `@playwright/test`
// exports for a launch and a server naming an executable that does not exist. Where the network
// is the browser's own, both must fail by the guard's name, before any browser starts; on the
// host's network the guard stands aside, and the same calls fail by Playwright's own refusal of
// the missing executable, which shows the child reached Playwright's launch at all.
test(
  "loading the config installs the launch guard: an unrouted launch or server fails by name there",
  async () => {
    const attempt = async (network: "own" | "host") => {
      const run = await runBounded({
        argv: [
          process.execPath,
          "-e",
          [
            'await import("./e2e/playwright.config.ts");',
            'const { chromium } = await import("@playwright/test");',
            'const missing = { executablePath: "/nonexistent/dispatch-e2e-chrome" };',
            'const said = async (call) => call().then(() => "started", (error) => String(error.message).split("\\n")[0]);',
            "console.log(JSON.stringify({ launch: await said(() => chromium.launch(missing)), launchServer: await said(() => chromium.launchServer(missing)) }));",
          ].join("\n"),
        ],
        bound: "short",
        cwd: new URL("../..", import.meta.url).pathname,
        env: {
          HOME: process.env.HOME,
          PATH: process.env.PATH,
          PLAYWRIGHT_BROWSERS_PATH: process.env.PLAYWRIGHT_BROWSERS_PATH,
          [browserNetworkVariable]: network,
        },
        from: import.meta.url,
      });
      expect({ code: run.code, stderr: run.stderr }).toEqual({ code: 0, stderr: run.stderr });
      return JSON.parse(run.stdout) as { launch: string; launchServer: string };
    };
    const guard =
      'e2e: a Chromium launch did not start through e2e/browser-namespace.sh (executablePath "/nonexistent/dispatch-e2e-chrome") while DISPATCH_E2E_BROWSER_NETWORK is "own"';
    const own = await attempt("own");
    expect(own.launch).toContain(guard);
    expect(own.launchServer).toContain(guard);
    const onHost = await attempt("host");
    for (const said of [onHost.launch, onHost.launchServer]) {
      expect(said).not.toContain("did not start through");
      expect(said).toContain("/nonexistent/dispatch-e2e-chrome");
    }
  },
  rowLimit("short", "short")
);

test("the proxy resolves the names Chromium's own resolver rules map, and loopback alone", () => {
  const names = resolverMaps([
    "--headless",
    "--host-resolver-rules=MAP dispatch-e2e.test 127.0.0.1, MAP *.wild 127.0.0.1",
  ]);
  expect([...names]).toEqual([["dispatch-e2e.test", "127.0.0.1"]]);
  expect(loopbackAddress("127.0.0.1", names)).toBe("127.0.0.1");
  expect(loopbackAddress("127.12.0.9", names)).toBe("127.12.0.9");
  expect(loopbackAddress("Dispatch-E2E.test", names)).toBe("127.0.0.1");
  expect(loopbackAddress("localhost", names)).toBe("127.0.0.1");
  expect(loopbackAddress("0:0:0:0:0:0:0:1", names)).toBe("::1");
  expect(loopbackAddress("10.0.0.1", names)).toBeUndefined();
  expect(loopbackAddress("example.com", names)).toBeUndefined();
  expect(loopbackAddress("0:0:0:0:0:0:0:2", names)).toBeUndefined();
});

test(
  "the proxy carries a connection to a loopback port both ways, by address and by mapped name",
  async () => {
    const proxy = await listening(
      loopbackSocksServer(new Map([["dispatch-e2e.test", "127.0.0.1"]]))
    );
    const echo = await echoServer();
    for (const host of ["127.0.0.1", "dispatch-e2e.test"]) {
      const { code, socket } = await connectThrough(proxy, host, echo);
      expect(code).toBe(0);
      socket.write("ping");
      expect((await readBytes(socket, "echo ping".length)).toString()).toBe("echo ping");
      socket.destroy();
    }
  },
  rowLimit("short")
);

test(
  "the proxy refuses an address off loopback, a name nothing maps, and says a refused port refused",
  async () => {
    const proxy = await listening(loopbackSocksServer(new Map()));
    // Port 1 of loopback: below 1024, where no unprivileged program can bind, and served by
    // nothing on these machines, so every connection to it is refused. A port a row freed would
    // be any program's to take.
    for (const [host, port, code] of [
      ["10.0.0.1", 80, 2],
      ["example.com", 80, 2],
      ["127.0.0.1", 1, 5],
    ] as const) {
      const { code: answered, socket } = await connectThrough(proxy, host, port);
      expect({ host, code: answered }).toEqual({ host, code });
      socket.destroy();
    }
  },
  rowLimit("short")
);

test("the launcher starts the headless shell for a headless launch and Chromium itself for a headed one", () => {
  expect(chromiumExecutable(["--headless", "--no-sandbox"])).toMatch(/chrome-headless-shell$/);
  expect(chromiumExecutable(["--no-sandbox"])).toMatch(/\/chrome$/);
});
