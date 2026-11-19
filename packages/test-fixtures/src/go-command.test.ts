import { describe, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, readdirSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { goCommand, goCommandSync, goFetchCommand } from "./go-command";
import { hostTool, skipWithout } from "./host-tools";
import { temporaryDirectories } from "./temporary-directories";

// Every row runs the real go command against a module proxy on disk that serves one small module,
// so no row reaches the network: GOPROXY=file://<dir> stands for the proxy a test must not fetch
// from, and the control row proves the same build does fetch from it when GOPROXY=off is not set.
//
// A host without go skips the rows with a line naming it, except on GitHub Actions: the one step
// that runs this file there (`bun run test:go`, in ci.yaml's test job, after its setup-go) is the
// only run of the rows, so a go missing there fails the file instead of skipping it. The package's
// own `bun run test` leaves this file out, so the test-fixtures job, which sets up no Go, neither
// runs it on whatever go the runner image carries nor skips it. GITHUB_ACTIONS, not CI: agent
// harnesses on the devbox export CI=true.
const onActions = process.env["GITHUB_ACTIONS"] === "true";
if (onActions && hostTool("go") === null) {
  throw new Error(
    "go is not on PATH on GitHub Actions, so the go command rows of this file would not run: " +
      "the step that runs them sets Go up first (ci.yaml's test job, actions/setup-go)"
  );
}
const withoutGo = !onActions && skipWithout("go");
const makeDirectory = temporaryDirectories("test-fixtures-go-command");
// A row compiles and links real programs, which on a cold build cache or a loaded machine takes
// longer than bun's 5 s default; a minute each.
setDefaultTimeout(60_000);

const DEP = "example.com/legion-go-command/dep";
const VERSION = "v1.0.0";
const REFUSAL =
  "The module cache lacks a module this build needs, and a test fetches no module while it runs. " +
  "Fetch them before the tests, from the repository root:";
const REPOSITORY_ROOT = join(import.meta.dir, "..", "..", "..");
// The build cache stays the caller's, where the go command puts it (docs/testing.md): it holds no
// module, so sharing it reads nothing a row's own module cache could miss, and spares each row a
// compile of the standard library.
const { PATH, HOME, XDG_CACHE_HOME, GOCACHE } = process.env;

interface Fixture {
  /** The row's own directory, which holds the rest. */
  readonly root: string;
  /** A module whose one package imports DEP, which its go.mod requires. */
  readonly consumer: string;
  /** The module cache every go command of the row reads and writes, empty at first. */
  readonly cache: string;
  /** The environment of the row's go commands: GOPROXY names the proxy on disk. */
  readonly env: Record<string, string | undefined>;
}

/**
 * A fresh module cache, a module proxy on disk serving DEP at VERSION (the layout the GOPROXY
 * protocol reads: `<module>/@v/list`, `.info`, `.mod` and `.zip`), and a module that imports DEP.
 * Nothing outside the row's directory is read but the go toolchain: GOENV=off keeps the caller's
 * go env file out, GOWORK=off any workspace, and GOSUMDB=off the checksum database, which this
 * made-up module is not in. -mod=mod lets the build write the go.sum the module has none of.
 */
function fixture(): Fixture {
  const root = makeDirectory("row");
  const versions = join(root, "proxy", DEP, "@v");
  mkdirSync(versions, { recursive: true });
  const goMod = `module ${DEP}\n\ngo 1.21\n`;
  writeFileSync(join(versions, "list"), `${VERSION}\n`);
  writeFileSync(
    join(versions, `${VERSION}.info`),
    JSON.stringify({ Version: VERSION, Time: "2026-01-01T00:00:00Z" })
  );
  writeFileSync(join(versions, `${VERSION}.mod`), goMod);
  writeFileSync(
    join(versions, `${VERSION}.zip`),
    storedZip([
      [`${DEP}@${VERSION}/go.mod`, goMod],
      [`${DEP}@${VERSION}/dep.go`, "package dep\n"],
    ])
  );
  const consumer = join(root, "consumer");
  mkdirSync(consumer);
  writeFileSync(
    join(consumer, "go.mod"),
    `module example.com/legion-go-command/consumer\n\ngo 1.21\n\nrequire ${DEP} ${VERSION}\n`
  );
  writeFileSync(join(consumer, "consumer.go"), `package consumer\n\nimport _ "${DEP}"\n`);
  // Made here, so a row that reads it finds a directory: a cache the reader could not list would
  // otherwise read as an empty one.
  const cache = join(root, "gopath", "pkg", "mod");
  mkdirSync(cache, { recursive: true });
  return {
    root,
    consumer,
    cache,
    env: {
      PATH,
      HOME,
      XDG_CACHE_HOME,
      GOCACHE,
      GOPATH: join(root, "gopath"),
      GOMODCACHE: cache,
      GOFLAGS: "-modcacherw -mod=mod",
      GOTOOLCHAIN: "local",
      GOENV: "off",
      GOWORK: "off",
      GOSUMDB: "off",
      GOPROXY: `file://${join(root, "proxy")}`,
    },
  };
}

/**
 * Every file under `cache` but the lock files, by path from `cache`: what a download left there.
 * The go command takes a lock beside a module's entry before it asks the proxy, so a refused
 * lookup leaves one lock file and nothing else.
 */
function downloaded(cache: string): string[] {
  const files = readdirSync(cache, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && !entry.name.endsWith(".lock"))
    .map((entry) => relative(cache, join(entry.parentPath, entry.name)));
  return files.sort();
}

/** A zip archive of `files`, stored without compression, as the GOPROXY protocol's `.zip`. */
function storedZip(files: readonly (readonly [string, string])[]): Uint8Array {
  const encoder = new TextEncoder();
  const locals: Uint8Array[] = [];
  const centrals: Uint8Array[] = [];
  let offset = 0;
  for (const [name, text] of files) {
    const nameBytes = encoder.encode(name);
    const data = encoder.encode(text);
    const crc = Bun.hash.crc32(data);
    const local = new Uint8Array(30 + nameBytes.length + data.length);
    const localView = new DataView(local.buffer);
    localView.setUint32(0, 0x04034b50, true);
    localView.setUint16(4, 20, true);
    localView.setUint16(12, 0x21, true); // 1980-01-01, the earliest date a zip can hold
    localView.setUint32(14, crc, true);
    localView.setUint32(18, data.length, true);
    localView.setUint32(22, data.length, true);
    localView.setUint16(26, nameBytes.length, true);
    local.set(nameBytes, 30);
    local.set(data, 30 + nameBytes.length);
    const central = new Uint8Array(46 + nameBytes.length);
    const centralView = new DataView(central.buffer);
    centralView.setUint32(0, 0x02014b50, true);
    centralView.setUint16(4, 20, true);
    centralView.setUint16(6, 20, true);
    centralView.setUint16(14, 0x21, true);
    centralView.setUint32(16, crc, true);
    centralView.setUint32(20, data.length, true);
    centralView.setUint32(24, data.length, true);
    centralView.setUint16(28, nameBytes.length, true);
    centralView.setUint32(42, offset, true);
    central.set(nameBytes, 46);
    locals.push(local);
    centrals.push(central);
    offset += local.length;
  }
  const centralSize = centrals.reduce((sum, entry) => sum + entry.length, 0);
  const end = new Uint8Array(22);
  const endView = new DataView(end.buffer);
  endView.setUint32(0, 0x06054b50, true);
  endView.setUint16(8, files.length, true);
  endView.setUint16(10, files.length, true);
  endView.setUint32(12, centralSize, true);
  endView.setUint32(16, offset, true);
  const archive = new Uint8Array(offset + centralSize + end.length);
  let at = 0;
  for (const part of [...locals, ...centrals, end]) {
    archive.set(part, at);
    at += part.length;
  }
  return archive;
}

describe.skipIf(withoutGo)("goCommandSync and goCommand", () => {
  test("a build that needs a module the cache lacks is refused, names the fetch, and downloads nothing", () => {
    const { consumer, cache, env } = fixture();
    let refusal = "";
    try {
      goCommandSync({ command: "build", cwd: consumer, packages: ["."], env });
    } catch (error) {
      refusal = error instanceof Error ? error.message : String(error);
    }
    expect(refusal).toContain("module lookup disabled by GOPROXY=off");
    expect(refusal).toContain(`${REFUSAL}\n  .github/scripts/go-fetch.sh deps ${consumer} .`);
    expect(downloaded(cache)).toEqual([]);
  });

  // The control of the row above, planted here: the same build, spawned without GOPROXY=off,
  // downloads the module from the proxy into the same empty cache. So the env above can fetch, and
  // only the GOPROXY=off the helper sets over it keeps the cache empty there.
  test("control: the same build without GOPROXY=off downloads the module into an empty cache", () => {
    const { consumer, cache, env } = fixture();
    expect(downloaded(cache)).toEqual([]);
    // go-offline: the control fetches on purpose, from the row's proxy on disk, never the network.
    const control = Bun.spawnSync(["go", "build", "."], { cwd: consumer, env, stderr: "pipe" });
    expect(control.stderr.toString()).toContain(`go: downloading ${DEP} ${VERSION}`);
    expect(control.exitCode).toBe(0);
    expect(downloaded(cache)).toContain(`cache/download/${DEP}/@v/${VERSION}.zip`);
  });

  test("a build whose modules the cache already holds succeeds and fetches nothing more", () => {
    const { consumer, cache, env } = fixture();
    // go-offline: the row's own fetch before the build, from its proxy on disk, never the network.
    const fetch = Bun.spawnSync(["go", "mod", "download", DEP], { cwd: consumer, env });
    expect(fetch.exitCode).toBe(0);
    const held = downloaded(cache);
    expect(held).toContain(`cache/download/${DEP}/@v/${VERSION}.zip`);
    const built = goCommandSync({ command: "build", cwd: consumer, packages: ["."], env });
    expect(built).toMatchObject({ exitCode: 0, stdout: "" });
    expect(downloaded(cache)).toEqual(held);
  });

  test("a failure that is no refusal of a fetch is returned to the caller, not thrown", () => {
    const { consumer, env } = fixture();
    const failed = goCommandSync({ command: "build", cwd: consumer, packages: ["./absent"], env });
    expect(failed.exitCode).not.toBe(0);
    expect(failed.stderr).toContain("absent");
    expect(failed.stderr).not.toContain("GOPROXY=off");
  });

  test("the async form refuses the same build with the same words", async () => {
    const { consumer, cache, env } = fixture();
    const refused = goCommand({ command: "build", cwd: consumer, packages: ["."], env });
    await expect(refused).rejects.toThrow(
      `${REFUSAL}\n  .github/scripts/go-fetch.sh deps ${consumer} .`
    );
    expect(downloaded(cache)).toEqual([]);
  });

  test("a go run's program arguments and standard input reach the program", () => {
    const { consumer, env } = fixture();
    const main = join(consumer, "cmd", "echo");
    mkdirSync(main, { recursive: true });
    writeFileSync(
      join(main, "main.go"),
      'package main\n\nimport (\n\t"fmt"\n\t"io"\n\t"os"\n)\n\n' +
        'func main() {\n\tin, _ := io.ReadAll(os.Stdin)\n\tfmt.Printf("%s %s", os.Args[1], in)\n}\n'
    );
    const ran = goCommandSync({
      command: "run",
      cwd: consumer,
      packages: ["./cmd/echo"],
      args: ["argument"],
      stdin: new TextEncoder().encode("input"),
      env,
    });
    expect(ran).toMatchObject({ exitCode: 0, stdout: "argument input" });
  });

  // As daemon-pane.ts runs daemon-pane.go: a main that belongs to no module, put in place of a
  // file the module does not have, so the package exists only under the overlay.
  test("the fetch of a go run -overlay names the packages the caller gives, not the overlay's main", () => {
    const { root, consumer, cache, env } = fixture();
    const program = join(root, "pane.go");
    writeFileSync(program, `package main\n\nimport _ "${DEP}"\n\nfunc main() {}\n`);
    const overlay = join(root, "overlay.json");
    writeFileSync(
      overlay,
      JSON.stringify({ Replace: { [join(consumer, "cmd", "pane", "main.go")]: program } })
    );
    expect(() =>
      goCommandSync({
        command: "run",
        cwd: consumer,
        flags: ["-overlay", overlay],
        packages: ["./cmd/pane"],
        fetch: ["./..."],
        env,
      })
    ).toThrow(`${REFUSAL}\n  .github/scripts/go-fetch.sh deps ${consumer} ./...`);
    expect(downloaded(cache)).toEqual([]);
  });
});

test("goFetchCommand names a directory of the repository from its root, and any other whole", () => {
  expect(goFetchCommand(join(REPOSITORY_ROOT, "packages", "envoy"), ["./cmd/listener"])).toBe(
    ".github/scripts/go-fetch.sh deps packages/envoy ./cmd/listener"
  );
  expect(goFetchCommand(REPOSITORY_ROOT, ["./..."])).toBe(
    ".github/scripts/go-fetch.sh deps . ./..."
  );
  expect(goFetchCommand("/elsewhere/module", ["."])).toBe(
    ".github/scripts/go-fetch.sh deps /elsewhere/module ."
  );
});
