import { dlopen, FFIType } from "bun:ffi";
import { onTestFinished } from "bun:test";

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
 * cannot bind a TCP socket without listening on it. The C library is opened for the platform the
 * test runs on: glibc Linux, musl Linux or macOS, the platforms this repository's tests run on.
 */
export type RefusedPort = {
  /** The held port, on 127.0.0.1. */
  readonly port: number;
  /** `http://127.0.0.1:<port>`. */
  readonly origin: string;
  /** Closes the socket, which frees the port; a second call does nothing. */
  release(): void;
};

/** `AF_INET` and `SOCK_STREAM`, the same on Linux (glibc's `<bits/socket.h>`, musl's
 *  `<sys/socket.h>`) and on macOS (`<sys/socket.h>`), and `sizeof(struct sockaddr_in)` on each. */
const AF_INET = 2;
const SOCK_STREAM = 1;
const SOCKADDR_IN_BYTES = 16;
const LOOPBACK = [127, 0, 0, 1];

/** The calls each platform's C library has under the same signature. */
const SOCKET_SYMBOLS = {
  socket: { args: [FFIType.i32, FFIType.i32, FFIType.i32], returns: FFIType.i32 },
  bind: { args: [FFIType.i32, FFIType.ptr, FFIType.u32], returns: FFIType.i32 },
  getsockname: { args: [FFIType.i32, FFIType.ptr, FFIType.ptr], returns: FFIType.i32 },
  close: { args: [FFIType.i32], returns: FFIType.i32 },
} as const;

/** One platform's socket calls. */
interface SocketCalls {
  /** A TCP socket no child the test starts inherits a copy of, or a negative number. */
  open(): number;
  /** A `struct sockaddr_in` of 127.0.0.1 on port 0, for the kernel to choose the port. */
  loopback(): Uint8Array;
  bind(fd: number, address: Uint8Array, length: number): number;
  getsockname(fd: number, address: Uint8Array, length: Uint32Array): number;
  close(fd: number): number;
}

/** Linux's `SOCK_CLOEXEC` (glibc's `<bits/socket_type.h>`, musl's `<sys/socket.h>`). */
const LINUX_SOCK_CLOEXEC = 0o2000000;

/**
 * Linux's socket calls, from glibc or musl. `libc.so.6` is glibc's C library, and musl's dynamic
 * linker answers a request for any `libc.` name with itself, not with a file (musl's
 * `ldso/dynlink.c`, "Catch and block attempts to reload the implementation itself"), so the one
 * name opens the C library the process runs on with either. Linux's `struct sockaddr_in` starts
 * with a 16-bit `sin_family` in the host's byte order.
 */
function linuxCalls(): SocketCalls {
  const { socket, bind, getsockname, close } = dlopen("libc.so.6", SOCKET_SYMBOLS).symbols;
  return {
    open: () => socket(AF_INET, SOCK_STREAM | LINUX_SOCK_CLOEXEC, 0),
    loopback() {
      const address = new Uint8Array(SOCKADDR_IN_BYTES);
      new Uint16Array(address.buffer, 0, 1)[0] = AF_INET;
      address.set(LOOPBACK, 4);
      return address;
    },
    bind,
    getsockname,
    close,
  };
}

/** macOS's `FIOCLEX`, `_IO('f', 1)` in `<sys/filio.h>`: `IOC_VOID` (0x20000000) | 'f' << 8 | 1. */
const MACOS_FIOCLEX = 0x20006601;

/**
 * macOS's socket calls. Its C library is `libSystem.B.dylib`, which since macOS 11 is only in the
 * dynamic linker's shared cache, where `dlopen` of its path still finds it. macOS has no
 * `SOCK_CLOEXEC`, so the socket is made close-on-exec by `ioctl(fd, FIOCLEX)`. `ioctl` is variadic,
 * and Apple's arm64 calls pass a variadic argument where they would not pass a fixed one, so this
 * declares the two fixed arguments alone, which both conventions pass alike; the third argument
 * `ioctl` reads is one `FIOCLEX` does not use. The test's JavaScript runs on one thread, so no
 * child starts between the two calls. macOS's `struct sockaddr_in` starts with an 8-bit length
 * (`sin_len`) and an 8-bit `sin_family` (`<netinet/in.h>`).
 */
function macCalls(): SocketCalls {
  const { socket, ioctl, bind, getsockname, close } = dlopen("/usr/lib/libSystem.B.dylib", {
    ...SOCKET_SYMBOLS,
    ioctl: { args: [FFIType.i32, FFIType.u64], returns: FFIType.i32 },
  }).symbols;
  return {
    open() {
      const fd = socket(AF_INET, SOCK_STREAM, 0);
      if (fd < 0 || ioctl(fd, MACOS_FIOCLEX) === 0) return fd;
      close(fd);
      return -1;
    },
    loopback() {
      const address = new Uint8Array(SOCKADDR_IN_BYTES);
      address[0] = SOCKADDR_IN_BYTES;
      address[1] = AF_INET;
      address.set(LOOPBACK, 4);
      return address;
    },
    bind,
    getsockname,
    close,
  };
}

/** Opened on the first hold. */
let calls: SocketCalls | undefined;

/** This platform's socket calls; where they cannot be opened, a refusal that names the platform. */
function socketCalls(): SocketCalls {
  try {
    if (process.platform === "linux") return linuxCalls();
    if (process.platform === "darwin") return macCalls();
    throw new Error("only Linux's and macOS's C libraries are known here");
  } catch (error) {
    throw new Error(
      `refused-port: cannot open the C library's socket calls on ${process.platform}/` +
        `${process.arch} (${error instanceof Error ? error.message : String(error)})`
    );
  }
}

/**
 * Binds a TCP socket to 127.0.0.1 on a port the kernel chooses and never listens on it, and
 * answers that port. The caller releases it; {@link refusingOrigin} is the form that releases it
 * when the row is done.
 */
export function holdRefusedPort(): RefusedPort {
  calls ??= socketCalls();
  const held = calls;
  const fd = held.open();
  if (fd < 0) throw new Error("refused-port: socket(AF_INET, SOCK_STREAM) failed");
  const address = held.loopback();
  const length = new Uint32Array([SOCKADDR_IN_BYTES]);
  if (
    held.bind(fd, address, SOCKADDR_IN_BYTES) !== 0 ||
    held.getsockname(fd, address, length) !== 0
  ) {
    held.close(fd);
    throw new Error("refused-port: binding 127.0.0.1 on a port the kernel chooses failed");
  }
  // sin_port, in network order, follows the family on each platform.
  const port = new DataView(address.buffer).getUint16(2, false);
  let open = true;
  return {
    port,
    origin: `http://127.0.0.1:${port}`,
    release() {
      if (!open) return;
      open = false;
      held.close(fd);
    },
  };
}

/**
 * `http://127.0.0.1:<port>` of a {@link holdRefusedPort} held until the row it is called in
 * finishes (`onTestFinished`). Call it inside the row that dials it: a hold that fails then fails
 * that row alone, and the file's other rows run. Called while Bun collects the file (at its module
 * scope, in a `describe`'s body), it throws before it holds anything.
 */
export function refusingOrigin(): string {
  let held: RefusedPort | undefined;
  onTestFinished(() => held?.release());
  held = holdRefusedPort();
  return held.origin;
}
