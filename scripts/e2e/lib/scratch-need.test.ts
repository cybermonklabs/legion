import { beforeAll, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scratchNeed } from "./scratch-need";

// Each heavy suite's need (lib/scratch-need.ts) against real tmpfs mounts of the row's own. In a
// mount namespace of its own, a row mounts one tmpfs a page smaller than the need and one of
// exactly the need, and memoryRoot, the fixture's own code, reads that namespace's mount table and
// each mount's free space. The smaller gives tmpdir(), the scratch disk, so a suite never writes
// into a /dev/shm too full for it; the other gives the mount.
//
// The namespace is a user and mount namespace this user may make (`unshare --user
// --map-root-user --mount`), else a mount namespace root makes (`sudo -n unshare --mount`), for a
// kernel that refuses the first to an unprivileged user. A host that allows neither skips the
// rows and says why.
const page = 4096;
const fixture = join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "packages",
  "test-fixtures",
  "src",
  "temporary-directories.ts"
);
const dir = temporaryDirectory("scratch-need-test", { root: memoryRoot() });
const judge = join(dir, "judge.ts");
const fallback = join(dir, "tmp");

const namespaces = [
  ["unshare", "--user", "--map-root-user", "--mount"],
  ["sudo", "-n", "unshare", "--mount"],
];
const namespace = namespaces.find(
  (prefix) =>
    Bun.spawnSync([...prefix, "true"], { stdout: "ignore", stderr: "ignore" }).exitCode === 0
);
if (namespace === undefined) {
  console.warn(
    "scratch-need.test.ts: skipped: this host lets this user make no mount namespace (neither `unshare --user --map-root-user --mount true` nor `sudo -n unshare --mount true` ran)"
  );
}

beforeAll(() => {
  if (namespace === undefined) return;
  mkdirSync(fallback);
  writeFileSync(
    judge,
    `import { memoryRoot } from ${JSON.stringify(fixture)};
const [need, small, exact] = process.argv.slice(2);
console.log(JSON.stringify({
  small: memoryRoot({ need: Number(need), at: small }),
  exact: memoryRoot({ need: Number(need), at: exact }),
}));
`
  );
});

// Two mounts, then the judge as the namespace's process, with TMPDIR at the row's own fallback.
const mountAndJudge =
  'mount -t tmpfs -o size="$1" tmpfs "$3" && mount -t tmpfs -o size="$2" tmpfs "$4" && exec env TMPDIR="$5" "$6" "$7" "$2" "$3" "$4"';

for (const [name, need] of Object.entries(scratchNeed)) {
  test.skipIf(namespace === undefined)(
    `${name}: a tmpfs a page short of its need gives tmpdir(), one of its need gives the tmpfs`,
    () => {
      const small = join(dir, `${name}-small`);
      const exact = join(dir, `${name}-exact`);
      mkdirSync(small);
      mkdirSync(exact);
      const run = Bun.spawnSync([
        ...(namespace ?? []),
        "sh",
        "-c",
        mountAndJudge,
        "sh",
        String(need - page),
        String(need),
        small,
        exact,
        fallback,
        process.execPath,
        judge,
      ]);
      expect(run.stderr.toString()).toBe("");
      expect(JSON.parse(run.stdout.toString())).toEqual({ small: fallback, exact });
    },
    // A hang guard: the row starts one Bun, which reads two mounts.
    30_000
  );
}
