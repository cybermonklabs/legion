// `socketOwner` (./socket-owner) names the process that owns a local TCP socket from /proc, as
// `ss -tnp` names it, so the reopen rows that ask it (./reopen-server) need no iproute2: a worker
// pod's image has none. The rows on a stand-in /proc hold the parse and the choice of owner; the
// last holds a real socket on this machine's /proc, with no `ss` on PATH.
import { expect, test } from "bun:test"
import { mkdir, symlink, writeFile } from "node:fs/promises"
import { basename, join } from "node:path"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { localPortRows, socketOwner } from "./socket-owner"

const makeDirectory = temporaryDirectories("claude-envoy-socket-owner")

const HEADER =
  "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"

/** A /proc/net/tcp row: `local` and `remote` as the kernel writes them, `state` its hex code. */
function row(sl: number, local: string, remote: string, state: string, inode: string): string {
  return `  ${sl}: ${local} ${remote} ${state} 00000000:00000000 00:00000000 00000000  1000        0 ${inode} 1 0000000000000000 20 4 30 10 -1`
}

// 54321 is D431 and 4222 is 107E in the kernel's hexadecimal.
const ESTABLISHED = row(0, "0100007F:D431", "0100007F:107E", "01", "111")
const ITS_PEER = row(1, "0100007F:107E", "0100007F:D431", "01", "222")
const LISTENING = row(2, "0100007F:D431", "00000000:0000", "0A", "333")
const TIME_WAIT = row(3, "0100007F:D431", "0100007F:1F90", "06", "0")
const SYN_SENT = row(4, "0100007F:D431", "0100007F:1F91", "02", "555")
const TABLE = [HEADER, ESTABLISHED, ITS_PEER, LISTENING, TIME_WAIT, SYN_SENT, ""].join("\n")

test("a row is the socket whose local port is the one asked for, in a state ss lists by default", () => {
  // Not ITS_PEER, whose remote port is 54321; not the listening or TIME-WAIT socket on 54321,
  // which `ss -t` leaves out with no state filter.
  expect(localPortRows(TABLE, 54321).map((each) => each.inode)).toEqual(["111", "555"])
  expect(localPortRows(TABLE, 54321).map((each) => each.row)).toEqual([
    ESTABLISHED.trim(),
    SYN_SENT.trim(),
  ])
  expect(localPortRows(TABLE, 4222).map((each) => each.inode)).toEqual(["222"])
  expect(localPortRows(TABLE, 8080)).toEqual([])
})

test("a /proc/net/tcp6 row is read by the port after its last colon", () => {
  const table = [
    HEADER,
    row(
      0,
      "0000000000000000FFFF00000100007F:D431",
      "0000000000000000FFFF00000100007F:107E",
      "01",
      "666",
    ),
    "",
  ].join("\n")
  expect(localPortRows(table, 54321).map((each) => each.inode)).toEqual(["666"])
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
  for (const each of processes) {
    await mkdir(join(proc, String(each.pid)))
    await writeFile(join(proc, String(each.pid), "comm"), `${each.comm}\n`)
    if (each.fds === undefined) continue
    await mkdir(join(proc, String(each.pid), "fd"))
    for (const [fd, target] of Object.entries(each.fds)) {
      await symlink(target, join(proc, String(each.pid), "fd", fd))
    }
  }
  return proc
}

const EMPTY = `${HEADER}\n`

test("the owner is the holder of the socket's inode that ss names first, the highest pid, by its comm", async () => {
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
  expect(await socketOwner(54321, proc)).toBe("sleep pid=1000")
  expect(await socketOwner(4222, proc)).toBe("nats-server pid=300")
})

test("a holder whose pid is below every pid of the scan's first batches is found", async () => {
  const others = Array.from({ length: 600 }, (_, index) => ({
    pid: 1000 + index,
    comm: "other",
    fds: { "0": "/dev/null" },
  }))
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    ...others,
    { pid: 3, comm: "bash", fds: { "3": "socket:[111]" } },
    { pid: 9, comm: "sleep", fds: { "3": "socket:[111]" } },
  ])
  expect(await socketOwner(54321, proc)).toBe("sleep pid=9")
})

test("a socket on /proc/net/tcp6 has its owner named too", async () => {
  const tcp6 = [
    HEADER,
    row(
      0,
      "00000000000000000000000001000000:D431",
      "00000000000000000000000001000000:107E",
      "01",
      "777",
    ),
    "",
  ].join("\n")
  const proc = await standInProc({ tcp: EMPTY, tcp6 }, [
    { pid: 41, comm: "bun", fds: { "9": "socket:[777]" } },
  ])
  expect(await socketOwner(54321, proc)).toBe("bun pid=41")
})

test("what no process this user can read holds is unknown, naming the rows read", async () => {
  const proc = await standInProc({ tcp: TABLE, tcp6: EMPTY }, [
    { pid: 300, comm: "nats-server", fds: { "3": "socket:[222]" } },
  ])
  expect(await socketOwner(54321, proc)).toBe(`unknown (${ESTABLISHED.trim()}; ${SYN_SENT.trim()})`)
  expect(await socketOwner(8080, proc)).toBe("unknown (no socket)")
})

test("a table that cannot be read is named in the unknown, never read as no socket", async () => {
  const proc = await standInProc({ tcp6: EMPTY }, [])
  expect(await socketOwner(54321, proc)).toMatch(
    new RegExp(`^unknown \\(no socket; cannot read ${join(proc, "net", "tcp")}: .*ENOENT`),
  )
})

test("a live socket's owner is named from this machine's /proc, with no ss on PATH", async () => {
  const { promise: clientPort, resolve: accepted } = Promise.withResolvers<number>()
  const listener = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        accepted(socket.remotePort)
      },
      data() {},
    },
  })
  // Another process dials and holds its socket until its stdin ends: the owner to tell from this
  // one, which holds the accepted end, whose local port is the listener's.
  const client = Bun.spawn(
    [
      process.execPath,
      "-e",
      `await Bun.connect({ hostname: "127.0.0.1", port: ${listener.port}, socket: { data() {} } }); await Bun.stdin.text()`,
    ],
    { stdin: "pipe", stdout: "ignore", stderr: "inherit" },
  )
  const path = process.env["PATH"]
  try {
    const port = await clientPort
    process.env["PATH"] = ""
    // The kernel names a process by the first 15 bytes of the file it executed.
    expect(await socketOwner(port)).toBe(
      `${basename(process.execPath).slice(0, 15)} pid=${client.pid}`,
    )
  } finally {
    process.env["PATH"] = path
    client.kill("SIGKILL")
    await client.exited
    listener.stop(true)
  }
})
