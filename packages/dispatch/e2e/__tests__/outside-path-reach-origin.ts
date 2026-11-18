/** The fake Dispatch of the tests of `e2e/acceptance/outside-path-reach.sh`, in a process of its
 *  own. `fakeDispatch` in `outside-path-reach-fixtures.ts` runs it as
 *  `bun outside-path-reach-origin.ts <health> <projects> <refusal JSON>` and says why it is a
 *  process of its own. It serves on 127.0.0.1, on a port the kernel picks, and prints that port as
 *  its first line on stdout. `/healthz` answers `health`, and `/api/v1/projects` answers 200 to a
 *  request carrying a credential (an `Authorization` or a `Cookie`), as Dispatch would, and
 *  `projects` to one carrying none, with `refusal` as the body. It records every request it
 *  answers, and `seenPath`, a path the script never requests, answers with that record, which
 *  holds no request for `seenPath` itself. It exits once its stdin closes: the test process holds
 *  the other end of that pipe, so this process never outlives the test process.
 *  Imported, it serves nothing: `fakeDispatch` reads `seenPath` and `Seen` from it.
 */

/** The path of the record of the requests this fake Dispatch answered. */
export const seenPath = "/__fake-dispatch/seen";

/** One request the fake Dispatch answered. */
export interface Seen {
  method: string;
  path: string;
  credential: string | null;
}

if (import.meta.main) {
  const [health, projects] = Bun.argv.slice(2, 4).map(Number);
  const refusal: unknown = JSON.parse(Bun.argv[4]);
  const seen: Seen[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === seenPath) return Response.json(seen);
      const credential = request.headers.get("authorization") ?? request.headers.get("cookie");
      seen.push({ method: request.method, path: pathname, credential });
      if (pathname === "/healthz") return Response.json({ ok: health === 200 }, { status: health });
      if (pathname === "/api/v1/projects") {
        const status = credential === null ? projects : 200;
        if (status === 302) return Response.redirect("https://accounts.example/signin", 302);
        return Response.json(status === 200 ? [] : refusal, { status });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
  console.log(server.port);
  const stdin = Bun.stdin.stream().getReader();
  while (!(await stdin.read()).done) {
    // The test process writes nothing here: this process only waits for the pipe to close.
  }
  process.exit(0);
}
