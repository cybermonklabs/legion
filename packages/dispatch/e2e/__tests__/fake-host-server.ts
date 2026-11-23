/** The control server of a host of one fake, the process `fakeHost` of
 *  `outside-path-reach-fixtures.ts` starts for each fake a row asks for. Two programs run it: the
 *  fake Dispatch host `outside-path-reach-origin.ts` and the stand-in edge host
 *  `outside-edge-refusals-edge.ts`. Each hands it the start of its own fake; what that fake answers
 *  stays in its program, and the protocol both share with `fakeHost` is here, once.
 *
 *  It serves a control server on 127.0.0.1, on a port the kernel picks, and prints that port as its
 *  first line on stdout, and nothing after it. `POST /` with a fake's configuration starts the fake
 *  and answers `{ port }`, the port the fake answers on; a second `POST /` is refused with 409,
 *  since the host serves one fake. `GET /seen` answers the fake's record of every request it
 *  answered, in the order it answered them, and 404 before the fake started. The host exits once
 *  its stdin closes: the test process holds the other end of that pipe, so the host never outlives
 *  the test process. A row stops it by killing it, which closes the fake's connections.
 *
 *  `bun test` collects only `*.test.ts` and its kin, so this module runs nowhere of its own.
 */

/** A fake a host serves: the port it answers on, and its record of every request it answered. */
export interface Serving<Entry> {
  port: number;
  seen: Entry[];
}

/** Serves the control server of a host whose one fake `start` starts from the configuration the
 *  first `POST /` carries, and exits once stdin closes. */
export async function serveFakeHost<Config, Entry>(
  start: (config: Config) => Serving<Entry> | Promise<Serving<Entry>>
): Promise<never> {
  /** The one fake, once `POST /` has started it, or the start in flight. */
  let fake: Promise<Serving<Entry>> | undefined;
  const control = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    routes: {
      "/": {
        async POST(request) {
          const config = (await request.json()) as Config;
          if (fake !== undefined) {
            return Response.json({ code: "ALREADY_STARTED" }, { status: 409 });
          }
          fake = Promise.resolve(start(config));
          return Response.json({ port: (await fake).port });
        },
      },
      "/seen": {
        async GET() {
          return fake === undefined
            ? Response.json({ code: "NOT_STARTED" }, { status: 404 })
            : Response.json((await fake).seen);
        },
      },
    },
  });
  console.log(control.port);
  // The test process writes nothing here: this only waits for the pipe to close.
  await Bun.stdin.text();
  process.exit(0);
}
