import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

// The listener and the Legion daemon refuse a nats-server below the floor packages/envoy/natsversion
// holds (every 2.10 among them), so whatever starts a NATS server for them must start one the floor
// takes, or its stack never comes up. A live proof that runs a container starts the image
// lib/nats-image.sh names, which it reads from the one constant the Go tests run Envoy on,
// testnats.HubImage (packages/envoy/internal/testnats/nats.go), the hub's nats:2.15.1-alpine. A
// script or a page that installs a host nats-server with `go install` or `go run` pins a release
// the floor takes. Both are scans of every tracked file, so a new driver or a new page is held too.

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

/** A line that starts a NATS server: a `docker run` whose arguments end in `-js`, as a JetStream server's do. */
const natsStart = /\bdocker run\b.*\s-js\b/;

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
const drivers = gitNames(["ls-files", "-z", "--", "*.sh"]).filter(
  (name) =>
    name !== ".github/scripts/check-ci-images.test.sh" &&
    natsStart.test(readFileSync(join(root, name), "utf8"))
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

/** natsversion.FixedReleases, which names the floor in every refusal. */
const floorReleases = "v2.11.12, v2.12.4, or v2.14.0 and later";

/**
 * Whether the floor takes a nats-server version: natsversion's fixed() over its parse(). The first
 * fixed release of each line is 2.11.12, 2.12.4 and 2.14.0; every later line is taken, 2.13 (no
 * release) too, and every 2.10 and earlier is not. A prerelease (a `-` after the patch's digits)
 * counts as before the release it leads to, and a version it cannot read is not taken.
 */
function floorTakes(version: string): boolean {
  const parts = /^v?(\d+)\.(\d+)\.(\d+)(.*)$/.exec(version);
  if (parts === null) return false;
  const [major, minor, patch] = [Number(parts[1]), Number(parts[2]), Number(parts[3])];
  const prerelease = parts[4]?.startsWith("-") ?? false;
  if (major !== 2) return major > 2;
  const at = (first: number) => patch > first || (patch === first && !prerelease);
  switch (minor) {
    case 11:
      return at(12);
    case 12:
      return at(4);
    case 13:
      return true;
    case 14:
      return at(0);
    default:
      return minor > 14;
  }
}

/** The module a host nats-server install names, as `go install <serverModule>@<version>`. */
const serverModule = "github.com/nats-io/nats-server/v2";

/** `<serverModule>@<what follows>`, up to a space, a quote, a bracket or an angle bracket. */
const pinAt = /github\.com\/nats-io\/nats-server\/v2@([^\s`'"()<>]*)/g;

/**
 * Each pin below the floor in a file's text: `<serverModule>@<version>`, or `@$name` with a literal
 * `name=<version>` in the same file. Anything else after the `@` (a placeholder, a version read
 * from go.mod at run time) pins nothing here.
 */
function hostPinProblems(name: string, text: string): string[] {
  const lines = text.split("\n");
  // Each refused version and the index of the line that holds it.
  const refusals = new Map<number, string>();
  lines.forEach((line, index) => {
    for (const [, pin = ""] of line.matchAll(pinAt)) {
      if (/^v?\d/.test(pin)) {
        if (!floorTakes(pin)) refusals.set(index, pin);
        continue;
      }
      const variable = /^\$\{?([A-Za-z_]\w*)\}?$/.exec(pin)?.[1];
      if (variable === undefined) continue;
      const assignment = new RegExp(
        `^\\s*(?:(?:local|readonly|declare)\\s+)?${variable}=["']?(v?\\d[^\\s"';]*)`
      );
      lines.forEach((other, at) => {
        const version = assignment.exec(other)?.[1];
        if (version !== undefined && !floorTakes(version)) refusals.set(at, version);
      });
    }
  });
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
  test("names testnats.HubImage, the image the Go tests run Envoy on", () => {
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
  });

  test("ends the driver when it cannot read the constant", () => {
    const helper = join(root, "scripts/e2e/lib/nats-image.sh");
    const result = Bun.spawnSync(
      ["bash", "-c", `root=/nonexistent; . ${JSON.stringify(helper)}; echo reached`],
      {
        stderr: "pipe",
      }
    );
    expect(result.exitCode).not.toBe(0);
    expect(result.stdout.toString()).not.toContain("reached");
    expect(result.stderr.toString()).toContain("HubImage");
  });

  // Drivers run under `set -Eeuo pipefail`, where a failed read of the constant would end the
  // driver before the helper says why.
  test("says why it ends a driver under set -Eeuo pipefail", () => {
    const helper = join(root, "scripts/e2e/lib/nats-image.sh");
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
  });
});

describe("every live proof starts the NATS server the floor takes", () => {
  // The control the check below leans on: the line every driver had before the floor is found.
  test("a driver starting nats:2.10 is named", () => {
    const old = `. "$root/scripts/e2e/lib/nats-image.sh"\ndocker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null\n`;
    expect(natsProblems("fixture.sh", old)).toEqual([
      'fixture.sh:2 starts NATS on an image other than "$nats_image": docker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null',
    ]);
  });

  test("the scan finds every driver known to start one", () => {
    expect(knownDrivers.filter((driver) => !drivers.includes(driver))).toEqual([]);
  });

  for (const driver of drivers) {
    test(driver, () => {
      expect(natsProblems(driver, readFileSync(join(root, driver), "utf8"))).toEqual([]);
    });
  }
});

describe("every host nats-server install pins a release the floor takes", () => {
  test("the floor is natsversion's", () => {
    const source = readFileSync(join(root, "packages/envoy/natsversion/natsversion.go"), "utf8");
    expect(/^const FixedReleases = "([^"]+)"$/m.exec(source)?.[1]).toBe(floorReleases);
  });

  test.each([
    ["2.10.22", false],
    ["2.11.4", false],
    ["2.11.12", true],
    ["2.12.4", true],
    ["2.12.4-RC.1", false],
    ["2.14.0", true],
    ["2.15.1", true],
  ])("%s is taken: %p", (version, taken) => {
    expect(floorTakes(version)).toBe(taken);
  });

  // The controls the scan below leans on: the pins this repository had before the floor are found.
  test("a pin of v2.11.4 is named", () => {
    const old = `fail "nats-server: go install ${serverModule}@v2.11.4"\n`;
    expect(hostPinProblems("fixture.sh", old)).toEqual([
      `fixture.sh:1 pins nats-server v2.11.4, below ${floorReleases}: ${old.trim()}`,
    ]);
  });

  test("a pin of v2.10.22 through a variable is named", () => {
    const old = `nats_version=v2.10.22\ngo install "${serverModule}@$nats_version"\n`;
    expect(hostPinProblems("fixture.sh", old)).toEqual([
      `fixture.sh:1 pins nats-server v2.10.22, below ${floorReleases}: nats_version=v2.10.22`,
    ]);
  });

  test("the scan finds every file known to pin one", () => {
    expect(knownPins.filter((file) => !pinFiles.includes(file))).toEqual([]);
  });

  for (const file of pinFiles) {
    test(file, () => {
      expect(hostPinProblems(file, readFileSync(join(root, file), "utf8"))).toEqual([]);
    });
  }
});
