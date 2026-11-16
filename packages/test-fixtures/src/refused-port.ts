import { dlopen, FFIType } from "bun:ffi";
import { afterAll } from "bun:test";

/**
 * A loopback TCP port the test process holds bound and never listens on: a connection to it is
 * refused for as long as it is held, and no other process can bind it meanwhile. It is what a test
 * dials for "nothing serves here". A fixed port such as 1 is not that: it is refused only while
 * nothing on the machine listens there, and a privileged process, or any process in a network
 * namespace whose `ip_unprivileged_port_start` is 0 (a container's root), can. Nor is the port of
 * a server the test stopped: any other process may bind it once it is free.
 *
 * The socket is opened through the C library in the test's own process (`bun:ffi`), so the hold
 * needs no other program: `scripts/e2e/dispatch-session-settings.sh` holds its broken provider's
 * port the same way from a `python3`, and a test that ran the first `python3` on PATH would depend
 * on what that is (a version manager's shim fails under a test's reduced environment). Bun itself
 * cannot bind a TCP socket without listening on it.
 */
export type RefusedPort = {
  /** The held port, on 127.0.0.1. */
  readonly port: number;
  /** `http://127.0.0.1:<port>`. */
  readonly origin: string;
  /** Closes the socket, which frees the port; a second call does nothing. */
  release(): void;
};

/** Linux's `AF_INET`, `SOCK_STREAM` and `SOCK_CLOEXEC` (so no child the test starts inherits a
 *  copy of the socket), and `sizeof(struct sockaddr_in)`. */
const AF_INET = 2;
const SOCK_STREAM = 1;
const SOCK_CLOEXEC = 0x80000;
const SOCKADDR_IN_BYTES = 16;
/** glibc's C library, which the Bun this repository's tests run on is linked against: the tests
 *  run on glibc Linux (CI's ubuntu runners, the dev boxes, Legion's pods). */
const LIBC = "libc.so.6";

/** The C library's socket calls this module makes. */
interface SocketCalls {
  socket(domain: number, type: number, protocol: number): number;
  bind(fd: number, address: Uint8Array, length: number): number;
  getsockname(fd: number, address: Uint8Array, length: Uint32Array): number;
  close(fd: number): number;
}

/** Opened on the first hold. */
let libc: SocketCalls | undefined;

/** glibc's socket calls; anywhere they cannot be opened (macOS, a musl Linux), a refusal that
 *  names the platform and the library, since the hold has no other way to open its socket. */
function openLibc(): SocketCalls {
  try {
    if (process.platform !== "linux") throw new Error("it is a Linux library");
    return dlopen(LIBC, {
      socket: { args: [FFIType.i32, FFIType.i32, FFIType.i32], returns: FFIType.i32 },
      bind: { args: [FFIType.i32, FFIType.ptr, FFIType.u32], returns: FFIType.i32 },
      getsockname: { args: [FFIType.i32, FFIType.ptr, FFIType.ptr], returns: FFIType.i32 },
      close: { args: [FFIType.i32], returns: FFIType.i32 },
    }).symbols;
  } catch (error) {
    throw new Error(
      `refused-port: cannot open ${LIBC} on ${process.platform}/${process.arch} (` +
        `${error instanceof Error ? error.message : String(error)}): a held refused port needs glibc ` +
        "Linux, which this repository's tests run on"
    );
  }
}

/**
 * Binds a TCP socket to 127.0.0.1 on a port the kernel chooses and never listens on it, and
 * answers that port. The caller releases it; {@link refusingOrigin} is the form that releases it
 * when the file's tests are done.
 */
export function holdRefusedPort(): RefusedPort {
  libc ??= openLibc();
  const calls = libc;
  const fd = calls.socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0) throw new Error("refused-port: socket(AF_INET, SOCK_STREAM) failed");
  // struct sockaddr_in: the family in the host's order, the port in network order (0, for the
  // kernel to choose), then the address.
  const address = new Uint8Array(SOCKADDR_IN_BYTES);
  const view = new DataView(address.buffer);
  view.setUint16(0, AF_INET, true);
  address.set([127, 0, 0, 1], 4);
  const length = new Uint32Array([SOCKADDR_IN_BYTES]);
  if (
    calls.bind(fd, address, SOCKADDR_IN_BYTES) !== 0 ||
    calls.getsockname(fd, address, length) !== 0
  ) {
    calls.close(fd);
    throw new Error("refused-port: binding 127.0.0.1 on a port the kernel chooses failed");
  }
  const port = view.getUint16(2, false);
  let held = true;
  return {
    port,
    origin: `http://127.0.0.1:${port}`,
    release() {
      if (!held) return;
      held = false;
      calls.close(fd);
    },
  };
}

/**
 * `http://127.0.0.1:<port>` of a {@link holdRefusedPort} held until the tests of the scope it is
 * called in are done. Call it at a test file's module scope, or in the body of a `describe`: it
 * registers the `afterAll` that releases the port where it is called. A scope Bun evaluates and
 * runs no hook of (a `-t` filter that selects none of its rows, `describe.skipIf(true)`) never
 * releases it, and the socket closes with the process.
 */
export function refusingOrigin(): string {
  const held = holdRefusedPort();
  afterAll(() => {
    held.release();
  });
  return held.origin;
}
