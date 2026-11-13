/** The read `--compare` of `e2e/delivery-live-probe.ts` makes of one GET route, bounded by its
 *  silence and not by its length, as the page bounds a GET (`REQUEST_DEADLINE_MS` of
 *  `web/src/api/client.ts`): the time starts at the request and starts over as the headers arrive
 *  and as each piece of the body does, so a body that keeps coming is read however slowly, and one
 *  that sends nothing for that long fails the read, naming its route.
 *
 *  It is a module of its own because the probe ends its process once its run is over, so a test
 *  cannot import the probe. It takes its timers as an argument, so
 *  `e2e/__tests__/silence-read.test.ts` moves the deadline on a clock of its own and reads each
 *  restart without waiting on a real one. */

/** The two timer calls the read makes: the system's own unless a caller hands in others. */
export interface Timers {
  readonly setTimeout: (fire: () => void, ms: number) => unknown;
  readonly clearTimeout: (handle: unknown) => void;
}

const systemTimers: Timers = {
  clearTimeout: (handle) => clearTimeout(handle as Timer),
  setTimeout: (fire, ms) => setTimeout(fire, ms),
};

/** What one read answered: its response, whose body is read, and that body as text. */
export interface SilenceRead {
  readonly response: Response;
  readonly text: string;
}

/** Reads the answer `request` makes for `route`, given `withinMs` of silence before the headers
 *  and between any two pieces of the body. `request` gets the signal that aborts the read when
 *  that time passes. Rejects with the request's own error, or, once the silence has run out, with
 *  an error naming `route` and the `--read-within` it ran out of. */
export async function readUntilSilent(
  route: string,
  request: (signal: AbortSignal) => Promise<Response>,
  withinMs: number,
  timers: Timers = systemTimers
): Promise<SilenceRead> {
  const silence = new AbortController();
  let timer = timers.setTimeout(() => silence.abort(), withinMs);
  const arrived = () => {
    timers.clearTimeout(timer);
    timer = timers.setTimeout(() => silence.abort(), withinMs);
  };
  let response: Response;
  let text = "";
  try {
    response = await request(silence.signal);
    arrived();
    if (response.body !== null) {
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      for (let chunk = await reader.read(); !chunk.done; chunk = await reader.read()) {
        arrived();
        text += decoder.decode(chunk.value, { stream: true });
      }
      text += decoder.decode();
    }
  } catch (error) {
    if (!silence.signal.aborted) throw error;
    throw new Error(
      `the read of ${route} received nothing for ${withinMs / 1000} s (--read-within)`
    );
  } finally {
    timers.clearTimeout(timer);
  }
  return { response, text };
}
