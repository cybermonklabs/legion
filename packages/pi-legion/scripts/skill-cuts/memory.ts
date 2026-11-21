/**
 * The peak resident memory of this process, for the runner's commands that stop above a limit
 * (cli.ts's behaviour runs, copies.ts). `/proc/self/status` gives `VmHWM`, the kernel's own peak;
 * gVisor gives only `VmRSS`, so there the largest `VmRSS` read is kept, read every second once
 * `startPeakRssSampling` runs and at every call of `procPeakRssBytes`.
 */
import { existsSync, readFileSync } from "node:fs";

/** The largest `VmRSS` read so far, for a kernel whose `/proc/self/status` gives no `VmHWM`. */
let sampledPeakBytes = 0;
/** How often the resident memory is read where the kernel keeps no peak of its own. */
const RSS_SAMPLE_MS = 1_000;

/** A field of `/proc/self/status` counted in kB, in bytes, or undefined when it holds none. */
function statusBytes(status: string, name: "VmHWM" | "VmRSS"): number | undefined {
  const kib = new RegExp(`^${name}:\\s+(\\d+) kB$`, "m").exec(status)?.[1];
  return kib === undefined ? undefined : Number(kib) * 1024;
}

/** The peak resident memory of this process, in bytes: `VmHWM` of `/proc/self/status`, or, on a
 *  kernel that keeps none (gVisor gives `VmRSS` only), the largest `VmRSS` read so far. Throws
 *  where the file holds neither. */
export function procPeakRssBytes(): number {
  const status = readFileSync("/proc/self/status", "utf8");
  const peak = statusBytes(status, "VmHWM");
  if (peak !== undefined) return peak;
  const resident = statusBytes(status, "VmRSS");
  if (resident === undefined) throw new Error("/proc/self/status holds neither VmHWM nor VmRSS");
  sampledPeakBytes = Math.max(sampledPeakBytes, resident);
  return sampledPeakBytes;
}

/** Where the kernel keeps no `VmHWM`, reads the resident memory every RSS_SAMPLE_MS from now on,
 *  so that the peak misses little between the calls of `procPeakRssBytes`. */
export function startPeakRssSampling(): void {
  const status = existsSync("/proc/self/status")
    ? readFileSync("/proc/self/status", "utf8")
    : undefined;
  if (status !== undefined && statusBytes(status, "VmHWM") === undefined) {
    setInterval(procPeakRssBytes, RSS_SAMPLE_MS).unref();
  }
}
