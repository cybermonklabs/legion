// The process that owns this machine's end of a TCP connection to a port of 127.0.0.1, read from
// /proc, so a row that asks (./reopen-server) runs where iproute2 is not installed, as in a worker
// pod's image.
import { readdir, readFile, readlink } from "node:fs/promises"
import { join } from "node:path"

/** One socket of a /proc/net/tcp or /proc/net/tcp6 table. */
export interface SocketRow {
  /** The table's line, trimmed: what an owner nobody holds is reported by. */
  readonly row: string
  /** The socket's inode, which each fd that holds it links to as `socket:[<inode>]`. */
  readonly inode: string
}

/** The states `ss -t` lists with no state filter: every one but LISTEN (0A), TIME-WAIT (06),
 *  CLOSE (07) and SYN-RECV (03), the kernel's hexadecimal codes in the `st` column. */
const LISTED_STATES: Readonly<Record<string, true>> = {
  "01": true,
  "02": true,
  "04": true,
  "05": true,
  "08": true,
  "09": true,
  "0B": true,
}

/** 127.0.0.1 as /proc/net/tcp writes it, and as /proc/net/tcp6 writes it IPv4-mapped. */
const LOOPBACK_V4: Readonly<Record<string, true>> = {
  "0100007F": true,
  "0000000000000000FFFF00000100007F": true,
}

/** The port of a table's `address:port`: hexadecimal, after the last colon, in either table. */
function portOf(address: string): number {
  return Number.parseInt(address.slice(address.lastIndexOf(":") + 1), 16)
}

/**
 * The rows of `table`, a /proc/net/tcp or /proc/net/tcp6 text, that are a connection from local
 * port `localPort` to port `remotePort` of 127.0.0.1, in a state `ss -t` lists. The local port
 * alone does not name one socket: Linux gives one ephemeral port to connects to different
 * destinations, so other live sockets on a busy machine can share it, and a higher pid's socket
 * among them would be named in place of the connection's. `ss -tn '( sport = :<port> )'` had
 * that gap too.
 */
export function connectionRows(table: string, localPort: number, remotePort: number): SocketRow[] {
  const rows: SocketRow[] = []
  // The first line is the column header.
  for (const line of table.split("\n").slice(1)) {
    const fields = line.trim().split(/\s+/)
    const local = fields[1]
    const remote = fields[2]
    const state = fields[3]
    const inode = fields[9]
    if (local === undefined || remote === undefined || state === undefined) continue
    if (inode === undefined) continue
    if (portOf(local) !== localPort || portOf(remote) !== remotePort) continue
    if (LOOPBACK_V4[remote.slice(0, remote.lastIndexOf(":")).toUpperCase()] !== true) continue
    if (LISTED_STATES[state] !== true) continue
    rows.push({ row: line.trim(), inode })
  }
  return rows
}

/**
 * The process that owns the connection from local port `localPort` to port `remotePort` of
 * 127.0.0.1, named as `ss -tnp` names a socket's holder (`bun pid=123`): of the processes with
 * an fd linked to the connection's inode, the one `ss` lists first, which is the one with the
 * highest pid, by its `comm`. A process this user cannot read is passed over, as `ss` run by
 * this user passes it over. With no owner found, it is `unknown (...)`, naming the rows it read,
 * or `no connection`, and each table it could not read: it never throws, so a lookup started
 * for a diagnosis cannot fail the row that started it.
 *
 * `proc` is the procfs root, a stand-in's for a test.
 */
export async function socketOwner(
  localPort: number,
  remotePort: number,
  proc = "/proc",
): Promise<string> {
  const rows: SocketRow[] = []
  const unread: string[] = []
  for (const table of ["tcp", "tcp6"]) {
    const path = join(proc, "net", table)
    try {
      rows.push(...connectionRows(await readFile(path, "utf8"), localPort, remotePort))
    } catch (error) {
      unread.push(`cannot read ${path}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }
  // A socket with no fd, one being closed, has inode 0.
  const inodes = new Set(rows.map((each) => each.inode).filter((inode) => inode !== "0"))
  const owner = inodes.size === 0 ? undefined : await highestHolder(proc, inodes)
  if (owner !== undefined) return owner
  const seen = rows.length === 0 ? "no connection" : rows.map((each) => each.row).join("; ")
  return `unknown (${[seen, ...unread].join("; ")})`
}

/** How many processes' fds one step of the scan reads at once. Measured 2026-11-20 on a 288-CPU
 *  box of 14,000 processes, a whole scan: every process at once took 190 to 250 ms and 2.2 to
 *  6.9 s of this process's CPU; 256 at once took 170 to 190 ms and 1.2 to 1.4 s of CPU, about
 *  the 0.3 s `ss -tnp` takes there. */
const SCAN_BATCH = 256

/**
 * `<comm> pid=<pid>` of the highest pid under `proc` with an fd linked to one of `inodes`. `ss`
 * prints a socket's holders highest pid first: for a shell's socket that its `sleep` inherited,
 * `users:(("sleep",pid=3203274,fd=3),("bash",pid=3202662,fd=3))`. The scan reads the fds from
 * the highest pid down, a batch at a time, and stops at the first batch with a holder: every
 * later batch holds only lower pids. It reads asynchronously, since this process also carries
 * the gate and the hub a row watches, which a synchronous scan would stall for as long as it
 * takes.
 */
async function highestHolder(
  proc: string,
  inodes: ReadonlySet<string>,
): Promise<string | undefined> {
  const links = new Set([...inodes].map((inode) => `socket:[${inode}]`))
  const pids = (await readdir(proc).catch(() => []))
    .filter((name) => /^\d+$/.test(name))
    .map(Number)
    .sort((a, b) => b - a)
  for (let start = 0; start < pids.length; start += SCAN_BATCH) {
    const batch = pids.slice(start, start + SCAN_BATCH)
    const holds = await Promise.all(
      batch.map((pid) => holdsOne(join(proc, String(pid), "fd"), links)),
    )
    const pid = batch.find((_, index) => holds[index])
    if (pid === undefined) continue
    const comm = await readFile(join(proc, String(pid), "comm"), "utf8").catch(() => "?")
    return `${comm.trim()} pid=${pid}`
  }
  return undefined
}

/** Whether an fd under `fdDirectory` links to one of `links`; false when it cannot be read. */
async function holdsOne(fdDirectory: string, links: ReadonlySet<string>): Promise<boolean> {
  const fds = await readdir(fdDirectory).catch(() => [])
  const targets = await Promise.all(
    fds.map((fd) => readlink(join(fdDirectory, fd)).catch(() => "")),
  )
  return targets.some((target) => links.has(target))
}
