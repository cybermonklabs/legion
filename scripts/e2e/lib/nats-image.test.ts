import { afterAll, describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { noHookLimit, removeScratch } from "../../../packages/dispatch/e2e/__tests__/step-end";
import {
  command,
  type Row,
  rowMs,
  rowOf,
  type Wait,
} from "../../../packages/test-fixtures/src/row-waits";

// The listener and the Legion daemon refuse a nats-server below the floor packages/envoy/natsversion
// holds (every 2.10 among them), so whatever starts a NATS server for them must start one the floor
// takes, or its stack never comes up. A live proof that runs a container starts the image
// lib/nats-image.sh names, which it reads from the one constant the Go tests run Envoy on,
// testnats.HubImage (packages/envoy/internal/testnats/nats.go), the hub's nats:2.15.1-alpine. A
// script or a page that installs a host nats-server with `go install` or `go run` pins a release
// the floor takes. Both are scans of every tracked file, so a new driver or a new page is held too.
// What neither scan sees, and nothing in the tree uses: a NATS start continued over lines, `podman`
// or a launcher that is not a shell script; an `export name=` or `${name:-<version>}` assignment;
// and a version set in another file the script sources.

const root = join(import.meta.dir, "..", "..", "..");

/** testnats.HubImage as packages/envoy/internal/testnats/nats.go declares it. */
function hubImage(): string {
  const source = readFileSync(join(root, "packages/envoy/internal/testnats/nats.go"), "utf8");
  const found = /^const HubImage = "([^"]+)"$/m.exec(source);
  if (found?.[1] === undefined)
    throw new Error("packages/envoy/internal/testnats/nats.go declares no HubImage");
  return found[1];
}

/** git's stdout in the checkout, NUL-separated names split; any exit but those allowed throws. */
function gitNames(args: string[], allowed: number[] = [0]): string[] {
  const result = Bun.spawnSync(["git", ...args], { cwd: root, stderr: "pipe" });
  if (!allowed.includes(result.exitCode)) {
    throw new Error(`git ${args.join(" ")} exited ${result.exitCode}: ${result.stderr.toString()}`);
  }
  return result.stdout
    .toString()
    .split("\0")
    .filter((name) => name !== "" && existsSync(join(root, name)));
}

/** A line that starts a NATS server: a `docker run`, or lib/rig.sh's `start_container` (a `docker
 *  create` and `docker start` of the run's own), whose arguments end in `-js`, as a JetStream
 *  server's do. */
const natsStart = /\b(?:docker run|start_container)\b.*\s-js\b/;

/** The names among `names` whose text holds a line that starts a NATS server. */
function startingNats(names: readonly string[], read: (name: string) => string): string[] {
  return names.filter((name) => natsStart.test(read(name)));
}

/** The drivers of `known` that the scan did not find among `found`. */
function unfound(known: readonly string[], found: readonly string[]): string[] {
  return known.filter((driver) => !found.includes(driver));
}

/**
 * Each problem a driver's text has: a line that starts a NATS server on any image but
 * `"$nats_image"`, and a driver that starts one and never sources lib/nats-image.sh, which sets
 * that variable.
 */
function natsProblems(name: string, text: string): string[] {
  const problems: string[] = [];
  let starts = 0;
  text.split("\n").forEach((line, index) => {
    if (!natsStart.test(line)) return;
    starts++;
    if (!line.includes('"$nats_image"')) {
      problems.push(
        `${name}:${index + 1} starts NATS on an image other than "$nats_image": ${line.trim()}`
      );
    }
  });
  if (starts > 0 && !/^\s*\. "\$root\/scripts\/e2e\/lib\/nats-image\.sh"/m.test(text)) {
    problems.push(`${name} starts NATS and never sources "$root/scripts/e2e/lib/nats-image.sh"`);
  }
  return problems;
}

/**
 * The tracked shell scripts that start a NATS server of their own. One is left out:
 * .github/scripts/check-ci-images.test.sh writes a rig's start line into a fixture tree for
 * check-ci-images.sh to read, inside a heredoc, and starts no server.
 */
const drivers = startingNats(
  gitNames(["ls-files", "-z", "--", "*.sh"]).filter(
    (name) => name !== ".github/scripts/check-ci-images.test.sh"
  ),
  (name) => readFileSync(join(root, name), "utf8")
);

/** The drivers known to start a NATS server, a floor under the scan: a scan finding none fails. */
const knownDrivers = [
  "scripts/e2e/stage3-devbox-workflow.sh",
  "scripts/e2e/claude-backend-tmux.sh",
  "scripts/e2e/claude-backend-sandbox.sh",
  "scripts/e2e/controller-start-tmux.sh",
  "scripts/e2e/dispatch-user-turns.sh",
  "scripts/e2e/parked-decision-devbox.sh",
  "scripts/e2e/stage2-tmux-supervision.sh",
  "scripts/e2e/verifiers-staging-token.sh",
  "docs/site/media/walkthroughs/legion-operator/record-casts.sh",
  "packages/pi-legion/scripts/skill-scenarios/rig.sh",
];

/**
 * natsversion's own rule, built once from packages/envoy/natsversion/cmd/natsfloor: the scanner
 * asks it whether the floor takes a version rather than hold a copy of the rule, so the floor the
 * listener and the daemon refuse a server by is the one this scan holds every pin to.
 */
const floorDir = mkdtempSync(join(tmpdir(), "natsfloor-"));

/** What the rows of this file wait for, each row its own list of them, which its limit is the sum
 *  of (`rowMs` of `packages/test-fixtures/src/row-waits.ts`) and which it counts as it makes them
 *  (`rowOf`): a `bash` that sources lib/nats-image.sh, and an ask of natsfloor, which `floorTakes`
 *  makes once for each version it is asked about. Each is a process of its own. */
const theHelper = command("a bash that sources lib/nats-image.sh");
const theFloor = command("an ask of natsfloor");
/** A row that runs the helper once, and one that asks the floor once. */
const oneHelper = [theHelper];
const oneAsk = [theFloor];

// The directory goes in a process of its own, which may take what the CI step leaves, or the limit
// of this file's shortest rows, one process's, and the hook has no limit of Bun's (step-end.ts).
afterAll(() => removeScratch([floorDir], rowMs(oneAsk)), noHookLimit);
const floor = join(floorDir, "natsfloor");
const build = Bun.spawnSync(["go", "build", "-o", floor, "./natsversion/cmd/natsfloor"], {
  cwd: join(root, "packages/envoy"),
  stdout: "pipe",
  stderr: "pipe",
});
if (build.exitCode !== 0) {
  throw new Error(`go build ./natsversion/cmd/natsfloor exited ${build.exitCode}: ${build.stderr}`);
}

/** natsfloor's answer lines for args; any exit but 0 throws. */
function askFloor(args: string[]): string[] {
  const result = Bun.spawnSync([floor, ...args], { stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) {
    throw new Error(`natsfloor ${args.join(" ")} exited ${result.exitCode}: ${result.stderr}`);
  }
  return result.stdout.toString().trimEnd().split("\n");
}

/** natsversion.FixedReleases, which names the floor in every refusal. */
const floorReleases = askFloor(["-releases"])[0] ?? "";

const floorVerdicts = new Map<string, boolean>();

/** Whether the floor takes a nats-server version: natsversion.Refusal's verdict, through natsfloor,
 *  which it asks once for each version, counting that ask against `row`. */
function floorTakes(row: Row, version: string): boolean {
  let taken = floorVerdicts.get(version);
  if (taken === undefined) {
    row.charge(theFloor);
    const [verdict, answered] = (askFloor([version])[0] ?? "").split("\t");
    if (answered !== version) throw new Error(`natsfloor answered ${answered} for ${version}`);
    taken = verdict === "taken";
    floorVerdicts.set(version, taken);
  }
  return taken;
}

/** The module a host nats-server install names, as `go install <serverModule>@<version>`. */
const serverModule = "github.com/nats-io/nats-server/v2";

/** `<serverModule>@<what follows>`, up to a space, a quote, a bracket or an angle bracket. */
const pinAt = /github\.com\/nats-io\/nats-server\/v2@([^\s`'"()<>]*)/g;

/**
 * Each pin of a file's text, in the order the scan reads them, with the index of the line that
 * holds its version: `<serverModule>@<version>`, or `@$name` with a literal `name=<version>` in the
 * same file. Anything else after the `@` (a placeholder, a version read from go.mod at run time)
 * pins nothing here.
 */
function pinsOf(text: string): { index: number; version: string }[] {
  const lines = text.split("\n");
  const pins: { index: number; version: string }[] = [];
  lines.forEach((line, index) => {
    for (const [, pin = ""] of line.matchAll(pinAt)) {
      if (/^v?\d/.test(pin)) {
        pins.push({ index, version: pin });
        continue;
      }
      const variable = /^\$\{?([A-Za-z_]\w*)\}?$/.exec(pin)?.[1];
      if (variable === undefined) continue;
      const assignment = new RegExp(
        `^\\s*(?:(?:local|readonly|declare)\\s+)?${variable}=["']?(v?\\d[^\\s"';]*)`
      );
      lines.forEach((other, at) => {
        const version = assignment.exec(other)?.[1];
        if (version !== undefined) pins.push({ index: at, version });
      });
    }
  });
  return pins;
}

/** What a row that scans `text` waits for: one ask of the floor for each version the text pins,
 *  which `floorTakes` asks once. */
function asksOf(text: string): Wait[] {
  return [...new Set(pinsOf(text).map((pin) => pin.version))].map(() => theFloor);
}

/** Each pin below the floor in a file's text (`pinsOf`), the floor's asks counted against `row`. */
function hostPinProblems(row: Row, name: string, text: string): string[] {
  const lines = text.split("\n");
  // Each refused version and the index of the line that holds it.
  const refusals = new Map<number, string>();
  for (const { index, version } of pinsOf(text)) {
    if (!floorTakes(row, version)) refusals.set(index, version);
  }
  return [...refusals]
    .sort(([a], [b]) => a - b)
    .map(
      ([index, version]) =>
        `${name}:${index + 1} pins nats-server ${version}, below ${floorReleases}: ${lines[index]?.trim()}`
    );
}

/**
 * The tracked text files that name a host install. History is left out, as it records what was
 * run then: the dated plans under docs/plans and docs/superpowers, each package's CHANGELOG.md, and
 * its changelog fragments under .changes.
 */
const pinFiles = gitNames(
  [
    "grep",
    "-l",
    "-z",
    "-I",
    "-F",
    `${serverModule}@`,
    "--",
    ".",
    ":(exclude)docs/plans",
    ":(exclude)docs/superpowers",
    ":(exclude)packages/*/CHANGELOG.md",
    ":(exclude)packages/*/.changes/*",
  ],
  // git grep exits 1 when no file matches: the floor below then names every file it missed.
  [0, 1]
);

/** Files known to pin a host nats-server, one of each form: a floor under the scan. */
const knownPins = [
  "skills/legion-worker/references/tools.md",
  "packages/claude-envoy/README.md",
  "packages/claude-envoy/scripts/smoke-session-title.sh",
  "docs/solutions/testing/the-daemons-nats-tests-run-in-a-docker-less-pod-against-a-local-nats-server-named-by-two-variables-INFRA-388.md",
  "scripts/e2e/lib/live-daemon-proof.sh",
  "scripts/e2e/dispatch-session-settings.sh",
];

describe("lib/nats-image.sh", () => {
  test(
    "names testnats.HubImage, the image the Go tests run Envoy on",
    () => {
      rowOf(oneHelper).charge(theHelper);
      const result = Bun.spawnSync(
        [
          "bash",
          "-c",
          `root=${JSON.stringify(root)}; . "$root/scripts/e2e/lib/nats-image.sh"; printf %s "$nats_image"`,
        ],
        { stderr: "pipe" }
      );
      expect(result.exitCode, result.stderr.toString()).toBe(0);
      expect(result.stdout.toString()).toBe(hubImage());
    },
    rowMs(oneHelper)
  );

  test(
    "ends the driver when it cannot read the constant",
    () => {
      const helper = join(root, "scripts/e2e/lib/nats-image.sh");
      rowOf(oneHelper).charge(theHelper);
      const result = Bun.spawnSync(
        ["bash", "-c", `root=/nonexistent; . ${JSON.stringify(helper)}; echo reached`],
        {
          stderr: "pipe",
        }
      );
      expect(result.exitCode).not.toBe(0);
      expect(result.stdout.toString()).not.toContain("reached");
      expect(result.stderr.toString()).toContain("HubImage");
    },
    rowMs(oneHelper)
  );

  // Drivers run under `set -Eeuo pipefail`, where a failed read of the constant would end the
  // driver before the helper says why.
  test(
    "says why it ends a driver under set -Eeuo pipefail",
    () => {
      const helper = join(root, "scripts/e2e/lib/nats-image.sh");
      rowOf(oneHelper).charge(theHelper);
      const result = Bun.spawnSync(
        [
          "bash",
          "-c",
          `set -Eeuo pipefail; root=/nonexistent; . ${JSON.stringify(helper)}; echo reached`,
        ],
        { stderr: "pipe" }
      );
      expect(result.exitCode).not.toBe(0);
      expect(result.stdout.toString()).not.toContain("reached");
      expect(result.stderr.toString()).toContain(
        "nats-image.sh: cannot read const HubImage from /nonexistent/packages/envoy/internal/testnats/nats.go"
      );
    },
    rowMs(oneHelper)
  );
});

describe("every live proof starts the NATS server the floor takes", () => {
  // The control the check below leans on: the line every driver had before the floor is found.
  test("a driver starting nats:2.10 is named", () => {
    const old = `. "$root/scripts/e2e/lib/nats-image.sh"\ndocker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null\n`;
    expect(natsProblems("fixture.sh", old)).toEqual([
      'fixture.sh:2 starts NATS on an image other than "$nats_image": docker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null',
    ]);
  });

  test("a driver starting nats:2.10 through start_container is named", () => {
    const old = `. "$root/scripts/e2e/lib/nats-image.sh"\nstart_container containers "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js\n`;
    expect(natsProblems("fixture.sh", old)).toEqual([
      'fixture.sh:2 starts NATS on an image other than "$nats_image": start_container containers "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js',
    ]);
  });

  // The control of the floor below: a known driver that starts NATS in a third form, which the
  // scan does not read, is named, never passed over.
  test("a known driver that starts NATS in a form the scan does not read fails the floor", () => {
    const third = `. "$root/scripts/e2e/lib/nats-image.sh"\ndocker create --name "$nats_container" -p 127.0.0.1::4222 "$nats_image" -js\ndocker start "$nats_container"\n`;
    expect(
      unfound(
        ["fixture.sh"],
        startingNats(["fixture.sh"], () => third)
      )
    ).toEqual(["fixture.sh"]);
  });

  test("the scan finds every driver known to start one", () => {
    expect(unfound(knownDrivers, drivers)).toEqual([]);
  });

  for (const driver of drivers) {
    test(driver, () => {
      expect(natsProblems(driver, readFileSync(join(root, driver), "utf8"))).toEqual([]);
    });
  }
});

describe("every host nats-server install pins a release the floor takes", () => {
  test("the floor the scan names is natsversion's", () => {
    const source = readFileSync(join(root, "packages/envoy/natsversion/natsversion.go"), "utf8");
    expect(/^const FixedReleases = "([^"]+)"$/m.exec(source)?.[1]).toBe(floorReleases);
  });

  // Which versions the floor takes is natsversion's own test (TestRefusalNamesEveryServerThatCanSkipAFilter).
  // These hold the scanner to naming a pin exactly when natsversion refuses its version, so a
  // change of that rule moves what the scan names with no edit here but these expectations.
  test.each([
    ["2.10.22", true],
    ["2.11.4", true],
    ["2.11.12", false],
    ["2.12.4", false],
    ["2.12.4-RC.1", true],
    ["2.14.0", false],
    ["2.15.1", false],
  ])(
    "a pin of %s is named: %p",
    (version, refused) => {
      const pin = `go install ${serverModule}@${version}\n`;
      const named = `fixture.sh:1 pins nats-server ${version}, below ${floorReleases}: ${pin.trim()}`;
      expect(hostPinProblems(rowOf(oneAsk), "fixture.sh", pin)).toEqual(refused ? [named] : []);
    },
    rowMs(oneAsk)
  );

  // The controls the scan below leans on: the pins this repository had before the floor are found.
  test(
    "a pin of v2.11.4 is named",
    () => {
      const old = `fail "nats-server: go install ${serverModule}@v2.11.4"\n`;
      expect(hostPinProblems(rowOf(oneAsk), "fixture.sh", old)).toEqual([
        `fixture.sh:1 pins nats-server v2.11.4, below ${floorReleases}: ${old.trim()}`,
      ]);
    },
    rowMs(oneAsk)
  );

  test(
    "a pin of v2.10.22 through a variable is named",
    () => {
      const old = `nats_version=v2.10.22\ngo install "${serverModule}@$nats_version"\n`;
      expect(hostPinProblems(rowOf(oneAsk), "fixture.sh", old)).toEqual([
        `fixture.sh:1 pins nats-server v2.10.22, below ${floorReleases}: nats_version=v2.10.22`,
      ]);
    },
    rowMs(oneAsk)
  );

  test("the scan finds every file known to pin one", () => {
    expect(knownPins.filter((file) => !pinFiles.includes(file))).toEqual([]);
  });

  for (const file of pinFiles) {
    const text = readFileSync(join(root, file), "utf8");
    const asks = asksOf(text);
    test(
      file,
      () => {
        expect(hostPinProblems(rowOf(asks), file, text)).toEqual([]);
      },
      rowMs(asks)
    );
  }
});
