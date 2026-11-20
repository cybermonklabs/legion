// The one way a test keeps the real Oh My Pi off the network. Under a fresh HOME the pinned binary
// fetches model catalogs over HTTPS, on `omp models` and at every session start
// (catalog.stencil.so, and the public model lists of the providers that need no key), and no
// setting of the pin stops all of it. `ompOffline` starts a listener on loopback that refuses every
// request, and answers the entry that names it as HTTPS_PROXY, the proxy Bun's fetch reads. A
// stand-in a test serves over plain HTTP on loopback is reached directly, as before. The Go tests'
// own is testbin.OMPOffline (packages/daemon/internal/testbin/omp.go), and
// .github/scripts/check-omp-offline.sh refuses a test that starts the real binary under an
// environment that does not carry one of the two and gives no reason.

/**
 * The environment entry that keeps one Oh My Pi run off the network: HTTPS_PROXY, naming a listener
 * of this process's own on 127.0.0.1 that answers every request it is sent, a proxy's CONNECT
 * included, with 403 and closes the connection. The listener answers only while this process's
 * event loop runs, so the binary must be started with `Bun.spawn`, never `Bun.spawnSync`, which
 * holds the loop until the binary exits. The listener stops at `cleanup`, the caller's list of what
 * it undoes after a case (omp-harness.ts's `Cleanup`).
 */
export function ompOffline(cleanup: (() => Promise<void>)[]): { readonly HTTPS_PROXY: string } {
  const listener = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      data(socket) {
        socket.end("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      },
    },
  });
  cleanup.push(async () => {
    listener.stop(true);
  });
  return { HTTPS_PROXY: `http://127.0.0.1:${listener.port}` };
}
