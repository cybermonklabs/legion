/* A disk that stops answering, for the PostgreSQL server this library is preloaded into
 * (LD_PRELOAD; e2e/disk-stall.ts builds it and says who starts a server with it). While the file
 * PG_STALL_FILE names exists, every fsync and fdatasync of the server waits; once it is gone, each
 * call goes through. That is where a commit waits for the disk: a backend flushing its commit
 * record waits in fdatasync with its interrupts held, so it neither takes a cancel nor lets go of
 * its locks until the disk answers. Nothing else the server does changes.
 *
 * The calls are made with syscall(2) rather than found with dlsym, so the library takes from
 * glibc only getenv, access, nanosleep and syscall, at GLIBC_2.2.5 on x86-64, and the stack
 * protector's __stack_chk_fail, at GLIBC_2.4 (`objdump -T fsync-stall.so` lists them), and loads
 * into an image whose glibc is older than the compiler's. */
#define _GNU_SOURCE
#include <stdlib.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static void wait_while_stalled(void) {
  const char *file = getenv("PG_STALL_FILE");
  if (file == NULL || *file == '\0') return;
  const struct timespec tick = {0, 20 * 1000 * 1000};
  while (access(file, F_OK) == 0) nanosleep(&tick, NULL);
}

int fsync(int fd) {
  wait_while_stalled();
  return (int)syscall(SYS_fsync, fd);
}

int fdatasync(int fd) {
  wait_while_stalled();
  return (int)syscall(SYS_fdatasync, fd);
}
