/** A start's removal of one container that a dead stack's Postgres left (`removeContainer`): the
 *  container sweep of `e2e/harness-postgres-removal.ts` runs it for each container Docker lists
 *  with the mark of a stack that is gone.
 *
 *  A CONTAINER IS CLAIMED BY DOCKER'S OWN REMOVAL. One `docker rm` removes it, and a start whose
 *  `docker rm` prints no id says that another start removed it. While the other removal is in
 *  progress Docker refuses this one, and the start sends no `docker rm` again, whatever `docker
 *  inspect` reads meanwhile: it asks Docker every 5 s whether it still lists the container, and
 *  says that another start removed it once Docker lists it no more, with what Docker said.
 *  A REMOVAL TAKES AS LONG AS DOCKER TAKES, WITHIN A BOUND. A daemon that many sessions share can
 *  take minutes over one `docker rm` while it answers every other call at once, so a start waits
 *  for a removal while Docker answers each ask, a `docker ps` that exits 0 (`removeOnce`). Once
 *  the removal has run 60 s it says so on one line, and it gives the removal up, and names the
 *  container on one line, where an ask gets no answer (a daemon that hangs, or one it cannot
 *  reach) or where the removal has run 10 minutes (`removalMs`). The bound is a deadline: each
 *  wait of a removal, an ask and each pause between two asks, takes the less of its own limit and
 *  what is left of the bound (`within`), and one that reaches the bound ends the removal there.
 *  ONE CLOCK. A removal reads the time, waits and runs `docker` on one machine (`Machine`): a
 *  start's is the machine's own (`machine`), and a row's is one whose clock the row moves, so that
 *  the row runs the start's own 10-minute bound in a moment of real time and sees a limit of any
 *  length that ends a removal before it. */
import { execFile } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";

import { firstLine } from "./harness-postgres-marks";

/** What this module's own lines are prefixed with, as every starter in `e2e/` names itself. */
const named = "e2e/harness-postgres-container-removal.ts";

/** How long one ask of Docker - a `docker ps` - may go unanswered before a start judges that the
 *  daemon hangs: a daemon that answers lists containers in well under a second, and goes on doing
 *  so while it takes minutes over a removal. On a shared Linux machine on 2026-11-18, a start
 *  gave up its `docker rm` of a dead stack's container at the 60 s it then allowed, and the next
 *  `docker rm` of that row took 74 s; Docker had ended each container's process within 0.2 s, and
 *  answered each `docker create`, `start`, `logs` and `inspect` between them within a second. A
 *  removal that has run this long while Docker answers is one the start says it waits for. */
export const dockerMs = 60_000;
/** How long a start waits in all for its removal of one container while Docker answers each ask:
 *  10 minutes, about six times the slowest such removal seen, 91 s on a shared Linux machine on
 *  2026-11-18, so a daemon that slow is waited for, while a removal Docker never ends holds a
 *  start, and each start after it on the machine, no longer. At the bound the start leaves the
 *  container, names it on one line, and goes on; a later start removes it. */
const removalMs = 600_000;
/** How long a removal of a start's own runs before the start asks whether Docker still answers,
 *  and how long between two such asks: a removal of a daemon that answers mostly ends sooner. */
const dockerWatchMs = 5_000;

/** The bounds of a start's removal of one container: how long one ask of Docker may go
 *  unanswered, which is also how long the removal runs before the start says that it waits
 *  (`answerMs`), how long the start waits in all (`boundMs`), and how long it waits before each
 *  ask (`watchMs`). A start keeps `dockerMs`, `removalMs` and `dockerWatchMs`; a row sets each low,
 *  a few seconds, to see each end in that time and not in the start's own, and the source check
 *  of the rows holds each row's limit to the program's sum of what it waits for. */
export interface RemovalBounds {
  readonly answerMs: number;
  readonly boundMs: number;
  readonly watchMs: number;
}
/** The start's own bounds, which a row runs too, to see that nothing ends a removal before them. */
export const removalBounds: RemovalBounds = {
  answerMs: dockerMs,
  boundMs: removalMs,
  watchMs: dockerWatchMs,
};

/** What a start's removal of a container reads the time from, waits on and runs `docker` on: the
 *  machine's own (`machine`), or a row's, whose clock the row moves itself and whose `docker`
 *  answers on that clock. Every wait of a removal is a `sleep` of it, and every end of a `docker`
 *  call is that call's `signal` or `timeout`, so a limit the removal sets itself ends a removal of
 *  such a row on the row's clock, where the row reads it; one of any other clock is refused by the
 *  row that reads this module's source (`harness-postgres-source-rules.test.ts`). */
export interface Machine {
  /** The time, in milliseconds since the epoch. */
  now(): number;
  /** Resolves once `ms` have passed, or at once where `signal` aborts. */
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
  /** Runs `docker` with `args` as `execFile` does: answers its standard output where it exits 0,
   *  and throws, with its standard error, where it exits otherwise, and where `signal` aborts or
   *  `timeout` ms pass first, either of which ends it. */
  docker(
    args: readonly string[],
    options: { readonly signal: AbortSignal; readonly timeout?: number }
  ): Promise<{ readonly stdout: string }>;
}

/** The machine's own time and `docker`, which each start's removal runs on. It sets no limit of its
 *  own: the row that reads this module's source refuses a number in it, and a read of the time or
 *  a run of a program anywhere else in this module. */
export const machine: Machine = {
  now: () => Date.now(),
  sleep: (ms, signal) => sleep(ms, undefined, { signal }).catch(() => undefined),
  docker: (args, options) => promisify(execFile)("docker", args, options),
};

/** What the daemon said of a `docker` call that failed: the first line of its standard error,
 *  which is Docker's own reason, or the error's own first line where it wrote none. */
function dockerSaid(error: unknown): string {
  const stderr =
    typeof error === "object" && error !== null && "stderr" in error ? error.stderr : undefined;
  const said = typeof stderr === "string" ? (stderr.trim().split("\n")[0] ?? "") : "";
  return said === "" ? firstLine(error) : said;
}

/** How one `docker rm` of a start ended: with Docker's answer, the containers it printed; with its
 *  refusal, in Docker's words; or given up, where Docker answered no ask while it ran or the
 *  removal reached its bound, with why. */
type RemovalAnswer =
  | { readonly kind: "answered"; readonly stdout: string }
  | { readonly kind: "refused"; readonly reason: string }
  | { readonly kind: "given up"; readonly reason: string };

/** What one ask of Docker found of the container `id`, on the machine `on`: a `docker ps` of it
 *  that exits 0 within `answerMs` of `on`'s clock and lists it, or lists no such container; or no
 *  answer, with why: no end within `answerMs` (`late`), or an exit status that is not 0, as a
 *  daemon the start cannot reach gives. The end of `ended`, the removal the ask watches, stops the
 *  ask: that removal's own answer then decides, and the ask answers "listed". */
type Ask =
  | { readonly kind: "listed" }
  | { readonly kind: "gone" }
  | { readonly kind: "none"; readonly reason: string; readonly late: boolean };

async function ask(id: string, answerMs: number, on: Machine, ended?: AbortSignal): Promise<Ask> {
  const late = new AbortController();
  const asked = new AbortController();
  void on.sleep(answerMs, asked.signal).then(() => {
    if (!asked.signal.aborted) late.abort();
  });
  try {
    const { stdout } = await on.docker(
      ["ps", "--all", "--quiet", "--no-trunc", "--filter", `id=${id}`],
      { signal: ended === undefined ? late.signal : AbortSignal.any([late.signal, ended]) }
    );
    return stdout.trim() === "" ? { kind: "gone" } : { kind: "listed" };
  } catch (error) {
    if (ended?.aborted) return { kind: "listed" };
    if (late.signal.aborted) {
      return {
        kind: "none",
        reason: `Docker answered no docker ps within ${answerMs / 1000} s`,
        late: true,
      };
    }
    return { kind: "none", reason: `docker ps failed (${dockerSaid(error)})`, late: false };
  } finally {
    asked.abort();
  }
}

/** The wait of one start for its removal of a container: how long it may still wait (`left`), and
 *  its one line (`tell`). */
interface Wait {
  left(): number;
  tell(): void;
}

/** The wait of a removal of the container that `what` names on a line, which begins now on the
 *  clock of `on`: `tell` prints its one line once the removal has run `answerMs` while Docker
 *  answered each ask. */
function waitOf(what: string, { answerMs, boundMs }: RemovalBounds, on: Machine): Wait {
  const began = on.now();
  let told = false;
  return {
    left: () => began + boundMs - on.now(),
    tell: () => {
      const ran = on.now() - began;
      if (told || ran < answerMs) return;
      told = true;
      console.error(
        `${named}: waits for Docker's removal of ${what}: the removal has run ${Math.floor(ran / 1000)} s and Docker answers each ask, so the start waits for it, at most ${boundMs / 1000} s in all`
      );
    },
  };
}

/** How long the next wait of a removal may take: its own limit, `ms`, or what is left of the
 *  removal's bound (`wait`), whichever is less, so that no wait carries a removal past its bound.
 *  Every pause and every ask of a removal takes its limit from here. */
function within(ms: number, wait: Wait): number {
  return Math.max(0, Math.min(ms, wait.left()));
}

/** Whether an ask whose limit was `limit` went unanswered because the removal's bound cut that
 *  limit short (`within`): its end is the bound's, not a daemon's that answers no more. */
function cutShort(asked: Ask, limit: number, { answerMs }: RemovalBounds): boolean {
  return asked.kind === "none" && asked.late && limit < answerMs;
}

/** Runs `docker rm --force --volumes id` on `on` for as long as Docker takes over it, within
 *  `boundMs` of the start of the removal (`wait`). While it runs, the start asks Docker every
 *  `watchMs` whether it still answers (`ask`), and says that it waits once the removal has
 *  run `answerMs` (`wait`); where one ask gets no answer, the daemon hangs or cannot be reached,
 *  and where the bound comes first, the removal is given up: the start stops its `docker rm`. Each
 *  pause and each ask takes the less of its own limit and what is left of the bound (`within`). */
async function removeOnce(
  id: string,
  bounds: RemovalBounds,
  wait: Wait,
  on: Machine
): Promise<RemovalAnswer> {
  const ended = new AbortController();
  const removal = on.docker(["rm", "--force", "--volumes", id], { signal: ended.signal }).then(
    ({ stdout }): RemovalAnswer => ({ kind: "answered", stdout }),
    (error: unknown): RemovalAnswer => ({ kind: "refused", reason: dockerSaid(error) })
  );
  void removal.then(() => ended.abort());
  const bound = `Docker did not end the removal within ${bounds.boundMs / 1000} s`;
  let why = `${bound}, while it answered each ask`;
  while (wait.left() > 0) {
    await on.sleep(within(bounds.watchMs, wait), ended.signal);
    if (ended.signal.aborted) return removal;
    if (wait.left() <= 0) break;
    const limit = within(bounds.answerMs, wait);
    const asked = await ask(id, limit, on, ended.signal);
    if (ended.signal.aborted) return removal;
    if (asked.kind === "none") {
      why = cutShort(asked, limit, bounds)
        ? `${bound}, and it had not answered the last ask when that bound came`
        : `${asked.reason} while this start removed it`;
      break;
    }
    wait.tell();
  }
  ended.abort();
  const answer = await removal;
  // A removal that ended in the moment before the stop is Docker's answer still.
  if (answer.kind === "answered") return answer;
  return { kind: "given up", reason: why };
}

/** How a removal of a container ended: this start removed it; another start removed it, with
 *  what Docker said of it; or it is not removed, with Docker's reason. */
export type ContainerRemoval =
  | { readonly kind: "removed" }
  | { readonly kind: "elsewhere"; readonly reason: string }
  | { readonly kind: "failed"; readonly reason: string };

/** Waits for another start's removal of the container `id`, for which Docker refused this start's
 *  own as already in progress (`refusal`), and sends no `docker rm` meanwhile, whatever `docker
 *  inspect` reads: Docker marks a removal in progress before it ends a container that runs
 *  (moby v28.5.2, `daemon/delete.go:40-44`, then `:91-101`), and reads that container as `running`
 *  until its process has ended (`container/state.go:121-136`), which a kill sent through containerd
 *  with no deadline (`daemon/kill.go:111`) can make last as long as the daemon hangs. It asks
 *  Docker every `watchMs` whether it still lists the container (`ask`), and says that it
 *  waits once the removal has run `answerMs` (`wait`), within the bound of the start's removal,
 *  each pause and each ask taking the less of its own limit and what is left of that bound
 *  (`within`), on `on`. Answers that another start removed it, in Docker's words, once Docker
 *  lists it no more; or that it is not removed, where an ask gets no answer or the bound comes
 *  first: a later start removes a container whose other removal ended and left it. */
async function otherRemoval(
  id: string,
  refusal: string,
  bounds: RemovalBounds,
  wait: Wait,
  on: Machine
): Promise<ContainerRemoval> {
  while (wait.left() > 0) {
    await on.sleep(within(bounds.watchMs, wait));
    if (wait.left() <= 0) break;
    const limit = within(bounds.answerMs, wait);
    const listed = await ask(id, limit, on);
    if (listed.kind === "none") {
      return {
        kind: "failed",
        reason: cutShort(listed, limit, bounds)
          ? `${refusal}, and Docker had not answered whether it still lists it when the ${bounds.boundMs / 1000} s bound of this start's removal came`
          : `${refusal}, and then ${listed.reason}`,
      };
    }
    if (listed.kind === "gone") return { kind: "elsewhere", reason: refusal };
    wait.tell();
  }
  return {
    kind: "failed",
    reason: `${refusal}, and Docker still lists it ${bounds.boundMs / 1000} s after this start's removal began`,
  };
}

/** Removes the container `id` with its anonymous volumes, `what` as a line names it, however long
 *  Docker takes over it while it answers each ask, within `boundMs` (`removeOnce`), on `on`, the
 *  machine's own but for a row (`Machine`). `docker rm --force` prints each container it removed
 *  and nothing for one that is already gone, which another start removed first. Where another
 *  start's removal of it is in progress, Docker refuses this one, and the start waits for that
 *  removal and sends no `docker rm` again (`otherRemoval`), so what it says of the container is
 *  what is true when it returns. It gives up where Docker answers no ask within `answerMs`, or at
 *  `boundMs`, bounds a row sets low to see each end (`RemovalBounds`). */
export async function removeContainer(
  id: string,
  what = `container ${id.slice(0, 12)}`,
  bounds = removalBounds,
  on = machine
): Promise<ContainerRemoval> {
  const wait = waitOf(what, bounds, on);
  const answer = await removeOnce(id, bounds, wait, on);
  if (answer.kind === "given up") return { kind: "failed", reason: answer.reason };
  if (answer.kind === "answered") {
    return answer.stdout.trim() !== ""
      ? { kind: "removed" }
      : { kind: "elsewhere", reason: "docker rm printed no id, as for a container that is gone" };
  }
  if (!/removal of container \S+ is already in progress/.test(answer.reason)) {
    return { kind: "failed", reason: answer.reason };
  }
  return otherRemoval(id, answer.reason, bounds, wait, on);
}
