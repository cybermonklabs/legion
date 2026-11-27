// The process that owns this machine's end of a TCP connection to an IPv4 address and port, read
// from /proc, so a row that asks (./reopen-server) runs where iproute2 is not installed, as in a
// worker pod's image.
import { readdir, readFile, readlink } from "node:fs/promises"
import { endianness } from "node:os"
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

/** The far end of a connection: an IPv4 address, such as a loopback address of 127.0.0.0/8, and
 *  a port. */
export interface Endpoint {
  readonly host: string
  readonly port: number
}

/**
 * `host`, an IPv4 address, as /proc/net/tcp writes it, and as /proc/net/tcp6 writes it
 * IPv4-mapped: each 32-bit word of the address in this machine's byte order, in hexadecimal, so
 * 127.0.0.1 is `0100007F` on a little-endian machine. Undefined for any other text.
 */
function tableAddresses(host: string): Readonly<Record<string, true>> | undefined {
  const bytes = host.split(".").map((part) => (/^\d{1,3}$/.test(part) ? Number(part) : Number.NaN))
  if (bytes.length !== 4 || bytes.some((byte) => !(byte <= 255))) return undefined
  const word = (four: readonly number[]): string =>
    (endianness() === "LE" ? [...four].reverse() : four)
      .map((byte) => byte.toString(16).padStart(2, "0").toUpperCase())
      .join("")
  const v4 = word(bytes)
  return { [v4]: true, [`0000000000000000${word([0, 0, 0xff, 0xff])}${v4}`]: true }
}

/** The port of a table's `address:port`: hexadecimal, after the last colon, in either table. */
function portOf(address: string): number {
  return Number.parseInt(address.slice(address.lastIndexOf(":") + 1), 16)
}

/**
 * The rows of `table`, a /proc/net/tcp or /proc/net/tcp6 text, that are a connection from local
 * port `localPort` to `remote`, in a state `ss -t` lists. The local port alone does not name one
 * socket: Linux gives one ephemeral port to connects to different destinations, so other live
 * sockets on a busy machine can share it, and a higher pid's socket among them would be named in
 * place of the connection's. `ss -tn '( sport = :<port> )'` had that gap too. The address counts
 * as well as the port: a listener on another loopback address can have the same port.
 */
export function connectionRows(table: string, localPort: number, remote: Endpoint): SocketRow[] {
  const addresses = tableAddresses(remote.host)
  if (addresses === undefined) return []
  const rows: SocketRow[] = []
  // The first line is the column header.
  for (const line of table.split("\n").slice(1)) {
    const fields = line.trim().split(/\s+/)
    const local = fields[1]
    const remoteField = fields[2]
    const state = fields[3]
    const inode = fields[9]
    if (
      local === undefined ||
      remoteField === undefined ||
      state === undefined ||
      inode === undefined
    ) {
      continue
    }
    if (portOf(local) !== localPort || portOf(remoteField) !== remote.port) continue
    if (addresses[remoteField.slice(0, remoteField.lastIndexOf(":")).toUpperCase()] !== true) {
      continue
    }
    if (LISTED_STATES[state] !== true) continue
    rows.push({ row: line.trim(), inode })
  }
  return rows
}

/** What `socketOwner` takes besides the connection's local port and far end. */
export interface OwnerOptions {
  /** The procfs root: a stand-in's for a test. */
  readonly proc?: string
  /** The process the caller expects to hold the connection, such as the server it started. Its
   *  fds are read first, before the tables, and it is the owner when it held the connection. */
  readonly expected?: number
  /** How many processes' fds one step of the scan reads at once. */
  readonly batchSize?: number
}

/**
 * The process that owns the connection from local port `localPort` to `remote`, named as
 * `ss -tnp` names a socket's holder (`bun pid=123`), by its `comm`. It is the `expected` process
 * when that held the connection as the lookup began. Else it is, of the processes with an fd
 * linked to the connection's inode, the one `ss` lists first: the one with the highest pid. A
 * process this user cannot read is passed over, as `ss` run by this user passes it over. With no
 * owner found, it is `unknown (...)`, naming the rows it read, or `no connection`, and each table
 * it could not read, or naming a `remote` host that is no IPv4 address: it never throws, so a
 * lookup started for a diagnosis cannot fail the row that started it.
 */
export async function socketOwner(
  localPort: number,
  remote: Endpoint,
  { proc = "/proc", expected, batchSize = SCAN_BATCH }: OwnerOptions = {},
): Promise<string> {
  if (tableAddresses(remote.host) === undefined) {
    return `unknown (${remote.host} is not an IPv4 address)`
  }
  // The expected process's fds are read before the tables. A connection the tables then list
  // with an inode is still held: by that process when its fds named the inode, else by another,
  // which the scan looks for. Once its last holder closes it, a connection keeps no inode in the
  // tables, so a connection the expected process closed in between starts no scan.
  const held = expected === undefined ? [] : await fdTargets(join(proc, String(expected), "fd"))
  const rows: SocketRow[] = []
  const unread: string[] = []
  for (const table of ["tcp", "tcp6"]) {
    const path = join(proc, "net", table)
    try {
      rows.push(...connectionRows(await readFile(path, "utf8"), localPort, remote))
    } catch (error) {
      unread.push(`cannot read ${path}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }
  // A socket with no fd, one being closed, has inode 0.
  const links = new Set(
    rows.filter((each) => each.inode !== "0").map((each) => `socket:[${each.inode}]`),
  )
  if (links.size > 0) {
    const pid = held.some((target) => links.has(target))
      ? expected
      : await highestHolder(proc, links, batchSize)
    if (pid !== undefined) {
      const comm = await readFile(join(proc, String(pid), "comm"), "utf8").catch(() => "?")
      return `${comm.trim()} pid=${pid}`
    }
  }
  const seen = rows.length === 0 ? "no connection" : rows.map((each) => each.row).join("; ")
  return `unknown (${[seen, ...unread].join("; ")})`
}

/** How many processes' fds one step of a scan reads at once. Measured 2026-11-20 at 23:20Z on a
 *  288-CPU box of 27,500 processes at a load of 1,400, a whole scan: every process at once took
 *  0.36 s and 1.9 to 3.7 s of this process's CPU; 256 at a step took 0.75 to 0.91 s and 1.3 to
 *  1.4 s of CPU. `ss -tnpH`, which reads every process too, took 1.2 to 2.4 s and 0.8 s of CPU in
 *  its own process, and 6 to 53 ms of this one's: a whole scan costs this process far more than
 *  `ss` did, which is why a caller that knows the process to expect names it. */
const SCAN_BATCH = 256

/**
 * The highest pid under `proc` with an fd linked to one of `links`. `ss` prints a socket's
 * holders highest pid first: for a shell's socket that its `sleep` inherited,
 * `users:(("sleep",pid=3203274,fd=3),("bash",pid=3202662,fd=3))`. The scan reads the fds from
 * the highest pid down, `batchSize` processes at a time, and stops at the first batch with a
 * holder: every later batch holds only lower pids. It reads asynchronously, since this process
 * also carries the gate and the hub a row watches, which a synchronous scan would stall for as
 * long as it takes.
 */
async function highestHolder(
  proc: string,
  links: ReadonlySet<string>,
  batchSize: number,
): Promise<number | undefined> {
  const pids = (await readdir(proc).catch(() => []))
    .filter((name) => /^\d+$/.test(name))
    .map(Number)
    .sort((a, b) => b - a)
  for (let start = 0; start < pids.length; start += batchSize) {
    const batch = pids.slice(start, start + batchSize)
    const holds = await Promise.all(
      batch.map(async (pid) => {
        const targets = await fdTargets(join(proc, String(pid), "fd"))
        return targets.some((target) => links.has(target))
      }),
    )
    const pid = batch.find((_, index) => holds[index])
    if (pid !== undefined) return pid
  }
  return undefined
}

/** The targets of the fd links under `fdDirectory`; none when it cannot be read. */
async function fdTargets(fdDirectory: string): Promise<string[]> {
  const fds = await readdir(fdDirectory).catch(() => [])
  return Promise.all(fds.map((fd) => readlink(join(fdDirectory, fd)).catch(() => "")))
}
