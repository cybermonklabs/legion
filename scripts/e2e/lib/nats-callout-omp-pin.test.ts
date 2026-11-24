import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// nats-callout-sessions.sh --real-hosts holds the omp it runs to the build .omp-pin names: omp
// --version prints the release alone, so a mise install of the pinned tool is held to the build by
// its directory (omp_install_refusal). mise names that directory for the tool, `:` and `/` turned
// into `-` (measured: github:cybermonklabs/oh-my-pi installs under
// ~/.mise/installs/github-cybermonklabs-oh-my-pi/<build>), so the check reads it from the pin. The
// check it replaces matched */github-sjawhar-oh-my-pi/*/bin/omp, and once .omp-pin named
// github:cybermonklabs/oh-my-pi it held no mise install to any build.
const e2e = join(import.meta.dir, "..");
const fn = scriptFunctions(join(e2e, "nats-callout-sessions.sh"));
const pin = readFileSync(join(e2e, "..", "..", ".omp-pin"), "utf8").trim();

/** What omp_install_refusal prints for PIN and the resolved binary OMP: empty where it holds. */
function refusal(pinned: string, omp: string): string {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `${fn("omp_install_refusal")}\nomp_install_refusal "$1" "$2"`,
      "harness",
      pinned,
      omp,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  expect({ code: ran.exitCode, stderr: ran.stderr.toString() }).toEqual({ code: 0, stderr: "" });
  return ran.stdout.toString();
}

const installs = "/home/someone/.mise/installs";

test("a mise install of today's pinned fork at another build is refused, naming both builds", () => {
  // The fork as .omp-pin names it on the day this was written, spelled as mise spelled it then.
  const today = "github:cybermonklabs/oh-my-pi@18.18.6-sami.20261117-013345";
  const other = `${installs}/github-cybermonklabs-oh-my-pi/18.18.6-sami.20261101-000000/bin/omp`;
  expect(refusal(today, other)).toBe(
    `${other} is the mise install of github:cybermonklabs/oh-my-pi at 18.18.6-sami.20261101-000000; .omp-pin names 18.18.6-sami.20261117-013345\n`
  );
  expect(
    refusal(today, `${installs}/github-cybermonklabs-oh-my-pi/18.18.6-sami.20261117-013345/bin/omp`)
  ).toBe("");
});

test("the pin .omp-pin names now holds its own mise install to its build", () => {
  const [tool, build] = [pin.slice(0, pin.lastIndexOf("@")), pin.slice(pin.lastIndexOf("@") + 1)];
  const directory = `${installs}/${tool.replaceAll(":", "-").replaceAll("/", "-")}`;
  expect(refusal(pin, `${directory}/${build}/bin/omp`)).toBe("");
  expect(refusal(pin, `${directory}/0.0.0-elsewhere/bin/omp`)).toContain(`.omp-pin names ${build}`);
});

test("the directory follows the pin's owner, so a fork that moves needs no edit here", () => {
  const moved = "github:someone-else/oh-my-pi@19.0.0-sami.20270101-000000";
  expect(
    refusal(moved, `${installs}/github-someone-else-oh-my-pi/18.0.0-sami.20261231-000000/bin/omp`)
  ).toContain("github:someone-else/oh-my-pi at 18.0.0-sami.20261231-000000");
  // An install of the fork's old owner is no install of the pinned tool, whatever its build.
  expect(
    refusal(moved, `${installs}/github-cybermonklabs-oh-my-pi/18.0.0-sami.20261231-000000/bin/omp`)
  ).toBe("");
});

test("an omp that is no mise install is not held to a build by its path", () => {
  expect(refusal(pin, "/opt/omp/bin/omp")).toBe("");
  expect(refusal(pin, "/usr/local/bin/omp")).toBe("");
});
