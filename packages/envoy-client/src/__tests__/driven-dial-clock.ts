import { until } from "@legion/test-fixtures/wait";
import type { ConnectionOptions, NatsConnection } from "nats";
import {
  connectNats,
  type NatsDialClock,
  type NatsDialHooks,
  type NatsDialPlan,
} from "../nats-connection";

/** A wait or a bound that a dial armed on a {@link DrivenDialClock}, held until the row ends it. */
export interface HeldTimer {
  readonly kind: "wait" | "bound";
  readonly ms: number;
  /** The clock's reading when the dial armed it. */
  readonly armedAt: number;
}

/**
 * A clock for `connectNats` (`NatsDialHooks.clock`) that a row drives, as Go's `clocktest.Fake`
 * is driven: it reads 0 until the row moves it, and each wait and bound the dial arms is held
 * until the row ends it. So a row reads the delay of each wait and the length of each bound the
 * dial chose, and the order of those and of its dials, and no clock: a loaded machine changes
 * none of them. nats.js's own timers (each address's handshake, its reconnect wait) are the
 * process's, as in production.
 */
export interface DrivenDialClock {
  readonly clock: NatsDialClock;
  /** The waits and bounds armed and neither ended nor disarmed yet, in the order armed. */
  held(): readonly HeldTimer[];
  /** The clock's reading. */
  now(): number;
  /** Moves the reading `ms` on and ends nothing: time the dial's own code took, such as an event
   *  loop its host blocked. */
  advance(ms: number): void;
  /** Ends `timer`: moves the reading to the moment it was due, unless the reading is past that,
   *  and runs it. */
  end(timer: HeldTimer): void;
}

/**
 * A {@link DrivenDialClock}. `write` is handed each step as it happens, in one order with whatever
 * else the row writes there (its dials): `wait 250 armed`, `wait 250 ended`, `bound 1000 armed`,
 * `bound 1000 disarmed` (the dial settled first), `bound 1000 ended`, `clock moved 700 ms on`.
 */
export function drivenDialClock(write: (step: string) => void): DrivenDialClock {
  let reading = 0;
  const held = new Map<HeldTimer, () => void>();
  const hold = (kind: HeldTimer["kind"], ms: number, run: () => void): HeldTimer => {
    const timer = { kind, ms, armedAt: reading };
    held.set(timer, run);
    write(`${kind} ${ms} armed`);
    return timer;
  };
  return {
    clock: {
      now: () => reading,
      sleep: (ms) => {
        const { promise, resolve } = Promise.withResolvers<void>();
        hold("wait", ms, resolve);
        return promise;
      },
      arm: (ms, expire) => {
        const timer = hold("bound", ms, expire);
        return () => {
          if (held.delete(timer)) write(`bound ${ms} disarmed`);
        };
      },
    },
    held: () => [...held.keys()],
    now: () => reading,
    advance: (ms) => {
      reading += ms;
      write(`clock moved ${ms} ms on`);
    },
    end: (timer) => {
      const run = held.get(timer);
      if (run === undefined) throw new Error(`the ${timer.kind} of ${timer.ms} ms is not held`);
      held.delete(timer);
      reading = Math.max(reading, timer.armedAt + timer.ms);
      write(`${timer.kind} ${timer.ms} ended`);
      run();
    },
  };
}

/** How a promise settled: its value, or the error it was rejected with, read for its code and
 *  message. */
export type Outcome<T> =
  | { readonly value: T; readonly error?: undefined }
  | {
      readonly value?: undefined;
      readonly error: { readonly code?: unknown; readonly message: string };
    };

/** `promise`'s outcome, which resolves either way. */
export async function outcomeOf<T>(promise: Promise<T>): Promise<Outcome<T>> {
  try {
    return { value: await promise };
  } catch (error) {
    return { error: error as { code?: unknown; message: string } };
  }
}

/** A `connectNats` run on a {@link DrivenDialClock}, and what it did. */
export interface DrivenDial {
  /** Each dial (`dial 2`) and each step of the clock, in order. */
  readonly steps: readonly string[];
  readonly driven: DrivenDialClock;
  /** Each dial's options, as `connectNats` handed them to `connect`, and its `connect()`. */
  readonly dials: readonly {
    readonly options: ConnectionOptions;
    readonly connecting: Promise<NatsConnection>;
  }[];
  /** How the run ended, once it has. */
  readonly outcome: Promise<Outcome<NatsConnection>>;
  /** How the run ended, or undefined while it runs. */
  ended(): Outcome<NatsConnection> | undefined;
}

/** Starts `connectNats` with `connect`, `options`, `plan` and `hooks` on a driven clock. */
export function dialOnDrivenClock(
  connect: (options: ConnectionOptions) => Promise<NatsConnection>,
  options: ConnectionOptions,
  plan: NatsDialPlan,
  hooks: Omit<NatsDialHooks, "clock"> = {}
): DrivenDial {
  const steps: string[] = [];
  const driven = drivenDialClock((step) => steps.push(step));
  const dials: { options: ConnectionOptions; connecting: Promise<NatsConnection> }[] = [];
  let ended: Outcome<NatsConnection> | undefined;
  const outcome = outcomeOf(
    connectNats(
      (dialOptions) => {
        const connecting = connect(dialOptions);
        dials.push({ options: dialOptions, connecting });
        steps.push(`dial ${dials.length}`);
        return connecting;
      },
      options,
      plan,
      { ...hooks, clock: driven.clock }
    )
  );
  void outcome.then((settled) => {
    ended = settled;
  });
  return { steps, driven, dials, outcome, ended: () => ended };
}

/**
 * The bound `run`'s dial holds once `ready` holds too (the server holds the dial's connection,
 * say). `connectNats` arms a dial's bound as it calls `connect()`, before any I/O, so a dial under
 * way with no bound held, or a run that ends before it arms one, fails the row here, saying which.
 */
export async function heldBound(
  run: DrivenDial,
  ready: () => boolean = () => true
): Promise<HeldTimer> {
  const next = await until("the dial's bound", () => {
    const ended = run.ended();
    if (ended !== undefined) return ended;
    if (!ready()) return false;
    return run.driven.held().find((timer) => timer.kind === "bound") ?? "none";
  });
  if (next === "none") throw new Error("the dial is under way and holds no bound");
  if ("kind" in next) return next;
  throw new Error(`the dial ended before it armed a bound: ${Bun.inspect(next)}`);
}
