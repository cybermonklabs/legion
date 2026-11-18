import { afterAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { timeLeft, until } from "@legion/test-fixtures/wait";
import { SQL } from "bun";
import { connect } from "nats";

// lib/containers.sh's two starts and their polls, against real containers: each server publishes
// no port, so no port Docker's allocator chooses can be one another program holds
// (docs/testing.md, "A container a test starts publishes no port"), and a client on this machine
// reaches it at the address the poll read back. A host with no docker runs none of the rows.
const skip = skipWithout("docker");
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(import.meta.dir, "containers.sh");
const work = temporaryDirectory("containers-test");
const label = `legion.test=containers-${randomUUID()}`;
/** ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml). */
const natsImage =
  "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f";
/** ghcr.io/cybermonklabs/legion/ci, the copy of pgvector/pgvector (.github/ci-images.yaml). */
const postgresImage =
  "ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11";
/** The containers the rows started, removed by name when the file ends. */
const started: string[] = [];

afterAll(() => {
  if (started.length > 0) Bun.spawnSync(["docker", "rm", "-f", "-v", ...started], { cwd: root });
});

/**
 * Runs `script` in bash with lib/containers.sh sourced, the run's directory as `$work` and a
 * `fail` that exits 2, as the proofs define it; `$1`, `$2`… are `args`. A poll that finds its
 * container stopped prints wait_impossible and exits 2 too.
 */
function shell(script: string, ...args: string[]) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -u
fail() { echo "$*" >&2; exit 2; }
work=${JSON.stringify(work)}
wait_impossible=''
. ${JSON.stringify(lib)}
${script}`,
      "containers-test",
      ...args,
    ],
    { cwd: root, stdout: "pipe", stderr: "pipe" }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString().trim(),
    stderr: result.stderr.toString().trim(),
  };
}

/** The value `poll` prints once it succeeds; it throws at once on exit 2, a fact that cannot come. */
function polled(poll: string, name: string): () => string {
  return () => {
    const run = shell(
      `if ${poll}; then printf '%s\\n' "$value"; exit 0; fi
[ -z "$wait_impossible" ] || fail "$wait_impossible"
exit 1`,
      name
    );
    if (run.code === 2) throw new Error(run.stderr);
    return run.code === 0 ? run.stdout : "";
  };
}

/** What `docker port NAME` prints: every port Docker published for the container. */
function published(name: string): string {
  const run = Bun.spawnSync(["docker", "port", name], {
    cwd: root,
    stdout: "pipe",
    stderr: "pipe",
  });
  expect(run.stderr.toString()).toBe("");
  return run.stdout.toString().trim();
}

describe.skipIf(skip)("lib/containers.sh", () => {
  test("NATS runs on the host's network, publishes no port and serves a client at the port it logged", async () => {
    const name = `legion-containers-test-nats-${randomUUID()}`;
    const start = shell(
      `start_nats_container "$1" --label "$2" --cpus 1 --memory 512m -- "$3" -js`,
      name,
      label,
      natsImage
    );
    started.push(name);
    // stderr carries docker's pull progress on a host without the image, so it is shown, not judged.
    expect(start.code === 0 ? 0 : start.stderr).toBe(0);
    const port = await until(
      `NATS in ${name} to log its client port`,
      polled(`value=''; nats_container_ready "$1" value`, name)
    );
    expect(published(name)).toBe("");
    const nc = await connect({ servers: `nats://127.0.0.1:${port}`, timeout: timeLeft() });
    try {
      const subject = `containers.${randomUUID()}`;
      const subscription = nc.subscribe(subject, { max: 1 });
      nc.publish(subject, "reached");
      for await (const message of subscription) expect(message.string()).toBe("reached");
      expect(subscription.getProcessed()).toBe(1);
    } finally {
      await nc.close();
    }
  });

  test("Postgres listens on a socket in the run's directory alone, publishes no port and serves a client there", async () => {
    const name = `legion-containers-test-pg-${randomUUID()}`;
    const start = shell(
      `start_postgres_container "$1" sockets --label "$2" --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql -e POSTGRES_PASSWORD=test -- "$3"
printf '%s\\n' "$sockets"`,
      name,
      label,
      postgresImage
    );
    started.push(name);
    expect(start.code === 0 ? 0 : start.stderr).toBe(0);
    const sockets = start.stdout;
    expect(sockets.startsWith(`${work}/`) || sockets.startsWith("/tmp/")).toBe(true);
    await until(
      `Postgres in ${name} to be ready on its socket`,
      polled(`value=''; postgres_container_ready "$1" && value=ready`, name)
    );
    expect(published(name)).toBe("");
    expect(shell(`postgres_container_sockets "$1"`, name).stdout).toBe(sockets);
    const sql = new SQL({
      path: join(sockets, ".s.PGSQL.5432"),
      username: "postgres",
      password: "test",
      database: "postgres",
    });
    try {
      const [row] = await sql`select current_setting('listen_addresses') as listen, 1 as one`;
      expect(row).toEqual({ listen: "", one: 1 });
    } finally {
      await sql.close();
    }
  });
});
