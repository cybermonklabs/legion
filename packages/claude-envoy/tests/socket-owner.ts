// The process that owns a local TCP socket, read from /proc as `ss -tnp` reads it, so a row that
// asks (./reopen-server) runs where iproute2 is not installed, as in a worker pod's image.
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

/** The rows of `table`, a /proc/net/tcp or /proc/net/tcp6 text, whose local port is `port`, in a
 *  state `ss -t` lists: the sockets `ss -tn '( sport = :<port> )'` shows. */
export function localPortRows(table: string, port: number): SocketRow[] {
  const rows: SocketRow[] = []
  // The first line is the column header.
  for (const line of table.split("\n").slice(1)) {
    const fields = line.trim().split(/\s+/)
    const local = fields[1]
    const state = fields[3]
    const inode = fields[9]
    if (local === undefined || state === undefined || inode === undefined) continue
    // The address is hexadecimal and the port after its last colon, in either table.
    if (Number.parseInt(local.slice(local.lastIndexOf(":") + 1), 16) !== port) continue
    if (LISTED_STATES[state] !== true) continue
    rows.push({ row: line.trim(), inode })
  }
  return rows
}

/**
 * The process that owns the TCP socket bound to local `port`, as `ss -tnp` names it
 * (`bun pid=123`): of the processes with an fd linked to the socket's inode, the one `ss` lists
 * first, which is the one with the highest pid, by its `comm`. A process this user cannot read
 * is passed over, as `ss` run by this user passes it over. With no owner found, it is
 * `unknown (...)`, naming the rows it read, or `no socket`, and each table it could not read: it
 * never throws, so a lookup started for a diagnosis cannot fail the row that started it.
 *
 * `proc` is the procfs root, a stand-in's for a test.
 */
export async function socketOwner(port: number, proc = "/proc"): Promise<string> {
  const rows: SocketRow[] = []
  const unread: string[] = []
  for (const table of ["tcp", "tcp6"]) {
    const path = join(proc, "net", table)
    try {
      rows.push(...localPortRows(await readFile(path, "utf8"), port))
    } catch (error) {
      unread.push(`cannot read ${path}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }
  // A socket with no fd, one being closed, has inode 0.
  const inodes = new Set(rows.map((each) => each.inode).filter((inode) => inode !== "0"))
  const owner = inodes.size === 0 ? undefined : await highestHolder(proc, inodes)
  if (owner !== undefined) return owner
  const seen = rows.length === 0 ? "no socket" : rows.map((each) => each.row).join("; ")
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
