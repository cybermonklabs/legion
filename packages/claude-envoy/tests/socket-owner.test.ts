// `socketOwner` (./socket-owner) names the process that owns this machine's end of a loopback TCP
// connection, read from /proc, so the reopen rows that ask it (./reopen-server) need no iproute2:
// a worker pod's image has none. The rows on a stand-in /proc hold the parse and the choice of
// owner; the last names a real connection's owner from this machine's /proc, in a process whose
// PATH finds no `ss`.
import { expect, test } from "bun:test"
import { mkdir, open, rm, symlink, writeFile } from "node:fs/promises"
import { basename, join } from "node:path"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { connectionRows, socketOwner } from "./socket-owner"

const makeDirectory = temporaryDirectories("claude-envoy-socket-owner")

/** The hang guard of the two rows that start a process, the package test script's own
 *  `--timeout`, so a run of this file alone has it too: the live row starts a Bun process, which
 *  scans every process on the machine, and the read-order row runs `mkfifo`. At loads of 500 to
 *  700 the live row passed Bun's default 5 s in 5 of 300 runs, each a correct run that had not
 *  finished. Each makes at most one small stand-in, so the hook that removes stand-ins after it
 *  has almost nothing to remove. */
const ROW_TIMEOUT_MS = 60_000

const HEADER =
  "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"

/** A /proc/net/tcp row: `local` and `remote` as the kernel writes them, `state` its hex code. */
function row(sl: number, local: string, remote: string, state: string, inode: string): string {
  return `  ${sl}: ${local} ${remote} ${state} 00000000:00000000 00:00000000 00000000  1000        0 ${inode} 1 0000000000000000 20 4 30 10 -1`
}

// The kernel's hexadecimal: the ports 54321 (D431), 4222 (107E), 8080 (1F90) and 443 (01BB); the
// addresses 127.0.0.1 (0100007F), 10.0.0.5 (0500000A), 10.0.0.9 (0900000A) and 10.1.2.3
// (0302010A).
/** The connection asked for: 127.0.0.1:54321 to 127.0.0.1:4222, the gate's port. */
const THE_CONNECTION = row(0, "0100007F:D431", "0100007F:107E", "01", "111")
/** Its other end: the gate's accepted socket, whose local port is 4222. */
const ITS_PEER = row(1, "0100007F:107E", "0100007F:D431", "01", "222")
const LISTENING = row(2, "0100007F:D431", "00000000:0000", "0A", "333")
/** An earlier connection of the same four parts, closed. */
const TIME_WAIT = row(3, "0100007F:D431", "0100007F:107E", "06", "0")
/** Linux gives one ephemeral port to connects to different destinations: three live sockets on
 *  local port 54321 that are not the connection, to another host's port, to another loopback
 *  port, and to port 4222 of another host. */
const OTHER_DESTINATION = row(4, "0500000A:D431", "0900000A:01BB", "01", "444")
const OTHER_LOOPBACK_PORT = row(5, "0100007F:D431", "0100007F:1F90", "01", "555")
const OTHER_HOST_SAME_PORT = row(6, "0500000A:D431", "0302010A:107E", "01", "666")
const TABLE = [
  HEADER,
  THE_CONNECTION,
  ITS_PEER,
  LISTENING,
  TIME_WAIT,
  OTHER_DESTINATION,
  OTHER_LOOPBACK_PORT,
  OTHER_HOST_SAME_PORT,
  "",
].join("\n")

/** The far end asked for: port 4222 of 127.0.0.1, the gate's. */
const GATE = { host: "127.0.0.1", port: 4222 }

test("a row is the connection from the local port to that port of 127.0.0.1, in a state ss lists by default", () => {
  // Not ITS_PEER, whose ports are the other way round; not the listening or TIME-WAIT socket,
  // which `ss -t` leaves out with no state filter; not a socket on 54321 to another destination.
  expect(connectionRows(TABLE, 54321, GATE)).toEqual([{ row: THE_CONNECTION.trim(), inode: "111" }])
  expect(
    connectionRows(TABLE, 4222, { host: "127.0.0.1", port: 54321 }).map((each) => each.inode),
  ).toEqual(["222"])
  expect(connectionRows(TABLE, 54321, { ...GATE, port: 443 })).toEqual([])
  expect(connectionRows(TABLE, 8080, GATE)).toEqual([])
  // A dial the gate has not accepted yet is listed, as `ss -t` lists it.
  const dialing = [HEADER, row(0, "0100007F:D431", "0100007F:107E", "02", "777"), ""].join("\n")
  expect(connectionRows(dialing, 54321, GATE).map((each) => each.inode)).toEqual(["777"])
})

test("a /proc/net/tcp6 row is read by the port after its last colon, its addresses IPv4-mapped", () => {
  const table = [
    HEADER,
    row(
      0,
      "0000000000000000FFFF00000100007F:D431",
      "0000000000000000FFFF00000100007F:107E",
      "01",
      "666",
    ),
    // ::1 is another address than 127.0.0.1: a connection to [::1]:4222 is not one to the gate.
    row(
      1,
      "00000000000000000000000001000000:D431",
      "00000000000000000000000001000000:107E",
      "01",
      "667",
    ),
    "",
  ].join("\n")
  expect(connectionRows(table, 54321, GATE).map((each) => each.inode)).toEqual(["666"])
})

interface StandInProcess {
  readonly pid: number
  readonly comm: string
  /** Its fd links; none, and no fd directory, for a process this user cannot read. */
  readonly fds?: Readonly<Record<string, string>>
}

/** A stand-in /proc: its two socket tables and each process's `comm` and `fd` links. */
async function standInProc(
  tables: { readonly tcp?: string; readonly tcp6?: string },
  processes: readonly StandInProcess[],
): Promise<string> {
  const proc = makeDirectory()
  await mkdir(join(proc, "net"))
  if (tables.tcp !== undefined) await writeFile(join(proc, "net", "tcp"), tables.tcp)
  if (tables.tcp6 !== undefined) await writeFile(join(proc, "net", "tcp6"), tables.tcp6)
  // The entries of /proc that are no process.
  await mkdir(join(proc, "self"))
  await writeFile(join(proc, "uptime"), "1.00 1.00\n")
  await Promise.all(
    processes.map(async (each) => {
      await mkdir(join(proc, String(each.pid)))
      await writeFile(join(proc, String(each.pid), "comm"), `${each.comm}\n`)
      if (each.fds === undefined) return
      await mkdir(join(proc, String(each.pid), "fd"))
      for (const [fd, target] of Object.entries(each.fds)) {
        await symlink(target, join(proc, String(each.pid), "fd", fd))
      }
    }),
  )
  return proc
}

const EMPTY = `${HEADER}\n`

test("the owner is the holder of the connection's inode that ss names first, the highest pid, by its comm", async () => {
  // A shell that dialed and the child that inherited its fd: `ss -tnp` prints
  // `users:(("sleep",pid=1000,fd=3),("bash",pid=200,fd=3))`. A sort of the pids as text would
  // put 200 above 1000.
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    { pid: 9000, comm: "other", fds: { "0": "/dev/null", "1": "pipe:[9]" } },
    { pid: 5000, comm: "unreadable" },
    { pid: 300, comm: "nats-server", fds: { "3": "socket:[222]", "4": "socket:[333]" } },
    { pid: 200, comm: "bash", fds: { "0": "/dev/null", "3": "socket:[111]" } },
    { pid: 1000, comm: "sleep", fds: { "1": "pipe:[9]", "3": "socket:[111]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc })).toBe("sleep pid=1000")
  expect(await socketOwner(4222, { host: "127.0.0.1", port: 54321 }, { proc })).toBe(
    "nats-server pid=300",
  )
})

test("the process the caller expects is the owner when it holds the connection, and is read before any scan", async () => {
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    { pid: 300, comm: "nats-server", fds: { "3": "socket:[222]" } },
    { pid: 200, comm: "bun", fds: { "3": "socket:[111]" } },
    { pid: 9000, comm: "sleep", fds: { "3": "socket:[111]" } },
  ])
  // A scan would name the higher pid, as `ss` does.
  expect(await socketOwner(54321, GATE, { proc, expected: 200 })).toBe("bun pid=200")
  expect(await socketOwner(54321, GATE, { proc })).toBe("sleep pid=9000")
  // One that does not hold it, or is no process, leaves the owner to the scan.
  expect(await socketOwner(54321, GATE, { proc, expected: 300 })).toBe("sleep pid=9000")
  expect(await socketOwner(54321, GATE, { proc, expected: 77 })).toBe("sleep pid=9000")
  // It is not the owner of a connection the tables do not list.
  expect(await socketOwner(54321, { ...GATE, port: 443 }, { proc, expected: 200 })).toBe(
    "unknown (no connection)",
  )
})

test(
  "the expected process's fds are read before the tables, so a scan starts only for a connection another process holds",
  async () => {
    // The tcp table is a FIFO: the lookup's read of it waits until this row writes it, and
    // opening it to write waits until the lookup opens it to read. The fd link goes between the
    // two, so only a read of the fds before the tables still sees it.
    const proc = await standInProc({ tcp6: EMPTY }, [
      { pid: 200, comm: "bun", fds: { "3": "socket:[111]" } },
    ])
    const tcp = join(proc, "net", "tcp")
    expect(await Bun.spawn(["mkfifo", tcp]).exited).toBe(0)
    const owner = socketOwner(54321, GATE, { proc, expected: 200 })
    const writer = await open(tcp, "w")
    await rm(join(proc, "200", "fd", "3"))
    await writer.writeFile(TABLE)
    await writer.close()
    expect(await owner).toBe("bun pid=200")
  },
  ROW_TIMEOUT_MS,
)

test("of the sockets on one local port, the owner is the connection's, not a higher pid's socket to another destination", async () => {
  // On a machine whose pids have wrapped, the other sockets on the port belong to higher pids.
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    { pid: 200, comm: "bun", fds: { "3": "socket:[111]" } },
    { pid: 8000, comm: "omp", fds: { "7": "socket:[444]" } },
    { pid: 8500, comm: "broker", fds: { "7": "socket:[555]" } },
    { pid: 8800, comm: "dispatch-head", fds: { "7": "socket:[666]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc })).toBe("bun pid=200")
  expect(await socketOwner(54321, { ...GATE, port: 443 }, { proc })).toBe("unknown (no connection)")
})

test("a holder whose pid is below every pid of the scan's first batches is found", async () => {
  // Processes this user cannot read, which the scan reads and passes over, as many as fill three
  // of its batches of two.
  const others = Array.from({ length: 6 }, (_, index) => ({ pid: 1000 + index, comm: "other" }))
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    ...others,
    { pid: 3, comm: "bash", fds: { "3": "socket:[111]" } },
    { pid: 9, comm: "sleep", fds: { "3": "socket:[111]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc, batchSize: 2 })).toBe("sleep pid=9")
})

test("a connection on /proc/net/tcp6 has its owner named too", async () => {
  const tcp6 = [
    HEADER,
    row(
      0,
      "0000000000000000FFFF00000100007F:D431",
      "0000000000000000FFFF00000100007F:107E",
      "01",
      "777",
    ),
    row(
      1,
      "00000000000000000000000001000000:D431",
      "00000000000000000000000001000000:107E",
      "01",
      "778",
    ),
    "",
  ].join("\n")
  const proc = await standInProc({ tcp: EMPTY, tcp6 }, [
    { pid: 41, comm: "bun", fds: { "9": "socket:[777]" } },
    { pid: 900, comm: "other", fds: { "9": "socket:[778]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc })).toBe("bun pid=41")
})

test("a connection to a port of another loopback address is told from one to that port of 127.0.0.1", async () => {
  // A gate of these rows listens on a loopback address of its own on Linux (fake-nats-server.ts's
  // fakeHost), here 127.86.2.1: the kernel writes it 0102567F. From the same local port, a
  // connection to the same port of 127.0.0.1, held by a higher pid, is another connection.
  const otherGate = { host: "127.86.2.1", port: 4222 }
  const tcp = [HEADER, THE_CONNECTION, row(7, "0100007F:D431", "0102567F:107E", "01", "888"), ""]
  const tcp6 = [
    HEADER,
    row(
      0,
      "0000000000000000FFFF00000100007F:D431",
      "0000000000000000FFFF00000102567F:107E",
      "01",
      "889",
    ),
    "",
  ]
  expect(connectionRows(tcp.join("\n"), 54321, otherGate).map((each) => each.inode)).toEqual([
    "888",
  ])
  expect(connectionRows(tcp6.join("\n"), 54321, otherGate).map((each) => each.inode)).toEqual([
    "889",
  ])
  const proc = await standInProc({ tcp: tcp.join("\n"), tcp6: EMPTY }, [
    { pid: 200, comm: "bun", fds: { "3": "socket:[888]" } },
    { pid: 9000, comm: "other", fds: { "3": "socket:[111]" } },
  ])
  expect(await socketOwner(54321, otherGate, { proc })).toBe("bun pid=200")
  expect(await socketOwner(54321, GATE, { proc })).toBe("other pid=9000")
  // A host that is no IPv4 address is named, never read as no connection.
  expect(await socketOwner(54321, { host: "localhost", port: 4222 }, { proc })).toBe(
    "unknown (localhost is not an IPv4 address)",
  )
})

test("what no process this user can read holds is unknown, naming the rows read", async () => {
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    { pid: 300, comm: "nats-server", fds: { "3": "socket:[222]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc })).toBe(`unknown (${THE_CONNECTION.trim()})`)
  expect(await socketOwner(8080, GATE, { proc })).toBe("unknown (no connection)")
})

test("a table that cannot be read is named in the unknown, never read as no connection", async () => {
  const proc = await standInProc({ tcp6: EMPTY }, [])
  const owner = await socketOwner(54321, GATE, { proc })
  expect(owner).toStartWith(`unknown (no connection; cannot read ${join(proc, "net", "tcp")}: `)
  expect(owner).toContain("ENOENT")
})

test("a host with no /proc/net/tcp6, as one with IPv6 off, names the owner from /proc/net/tcp", async () => {
  // A kernel booted with IPv6 off has no tcp6 table: its absence is no reason to name no owner.
  const proc = await standInProc({ tcp: TABLE }, [
    { pid: 200, comm: "bun", fds: { "3": "socket:[111]" } },
  ])
  expect(await socketOwner(54321, GATE, { proc })).toBe("bun pid=200")
})

// It reads this machine's own /proc, which only Linux has: a macOS host runs the stand-in rows.
test.skipIf(process.platform !== "linux")(
  "a live connection's owner is named from this machine's /proc, by a process whose PATH finds no ss",
  async () => {
    // A loopback address other than 127.0.0.1, as the reopen rows' gate listens on Linux
    // (fake-nats-server.ts's fakeHost): the kernel writes it in its own byte order.
    const host = "127.86.1.7"
    const listener = Bun.listen({ hostname: host, port: 0, socket: { data() {} } })
    const far = JSON.stringify({ host, port: listener.port })
    // Another process dials and names its own connection's owner twice, by a scan and as the
    // process it expects: the owner to tell from this one, which holds the accepted end, and from
    // any other socket that shares the client's port. It starts with a PATH of one directory
    // that does not exist, so it cannot run `ss`: Bun finds `ss` through a default search path
    // when PATH is empty or unset.
    const lookup = `
      import { socketOwner } from ${JSON.stringify(join(import.meta.dir, "socket-owner.ts"))}
      const socket = await Bun.connect({ hostname: ${JSON.stringify(host)}, port: ${listener.port}, socket: { data() {} } })
      const scanned = await socketOwner(socket.localPort, ${far})
      const expected = await socketOwner(socket.localPort, ${far}, { expected: process.pid })
      console.log(JSON.stringify([scanned, expected]))
      socket.end()
    `
    const client = Bun.spawn([process.execPath, "-e", lookup], {
      env: { PATH: "/nonexistent" },
      stdout: "pipe",
      stderr: "inherit",
    })
    try {
      const printed = await new Response(client.stdout).text()
      // The kernel names a process by the first 15 bytes of the file it executed.
      const owner = `${basename(process.execPath).slice(0, 15)} pid=${client.pid}`
      expect(printed.trim()).toBe(JSON.stringify([owner, owner]))
      expect(await client.exited).toBe(0)
    } finally {
      client.kill("SIGKILL")
      await client.exited
      listener.stop(true)
    }
  },
  ROW_TIMEOUT_MS,
)
