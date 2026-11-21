package cputest

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// spinSteps is how many steps of userWork the smaller window of the test below takes: about 25 ms
// on a core of a few GHz, so the window of scale of them lasts 50 ms or more.
const spinSteps = 25_000_000

// faultedBytes is how much fresh memory faultPages writes to, one byte in each page, and
// faultRounds how many times it does that: about 300 ms of the kernel's work on the thread, several
// times the user work of the smaller window.
const (
	faultedBytes = 256 << 20
	faultRounds  = 2
)

// spinSink keeps the compiler from dropping the work of userWork. It is atomic because
// holdEachProcessor runs userWork on several goroutines at once.
var spinSink atomic.Uint64

// userWork is steps of xorshift, work in user mode that makes no system call and allocates nothing.
func userWork(steps int) {
	state := uint64(88172645463325252)
	for range steps {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
	}
	spinSink.Add(state)
}

// faultPages maps faultedBytes of fresh memory faultRounds times, in small pages, and writes one
// byte in each page: the kernel finds and zeroes a page at each write, the work that it does, with
// the reclaim of a page first, when a machine or a cgroup is short of memory.
func faultPages(t *testing.T) {
	for range faultRounds {
		memory, err := unix.Mmap(-1, 0, faultedBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
		if err != nil {
			t.Fatalf("mmap %d bytes: %v", faultedBytes, err)
		}
		if err := unix.Madvise(memory, unix.MADV_NOHUGEPAGE); err != nil {
			t.Fatalf("madvise MADV_NOHUGEPAGE: %v", err)
		}
		for page := 0; page < len(memory); page += unix.Getpagesize() {
			memory[page] = 1
		}
		if err := unix.Munmap(memory); err != nil {
			t.Fatalf("munmap: %v", err)
		}
	}
}

// Growth counts the work of the caller's own code, and not the kernel's work for the caller's
// thread. The larger window does scale times the user work of the smaller one, and it also faults
// fresh pages, which is kernel work several times the user work of the smaller window. Its growth
// stays near scale. The control: the faults take more of the thread's whole CPU time (threadCPU)
// than the smaller window takes, so a measure that counted them would read a growth of more than
// twice scale.
func TestGrowthLeavesOutTheKernelsWorkForTheThread(t *testing.T) {
	const scale, samples = 4, 3
	growth, small, large, err := Growth(samples, scale,
		Again(func() { userWork(spinSteps) }),
		Again(func() {
			userWork(scale * spinSteps)
			faultPages(t)
		}))
	if err != nil {
		t.Fatal(err)
	}
	var kernel time.Duration
	for sample := range samples {
		if took, _, _ := onThread(threadCPU, func() { faultPages(t) }); sample == 0 || took < kernel {
			kernel = took
		}
	}
	t.Logf("smaller window %v, larger window %v, %.1f times; the faults alone on the thread's whole CPU time %v", small, large, growth, kernel)
	if kernel < small {
		t.Fatalf("the faults took %v of the thread's CPU time, less than the smaller window's %v, so this test reads nothing", kernel, small)
	}
	if growth >= 1.5*scale {
		t.Errorf("the work grew %.1f times, from %v for %d runs to %v for one run of %d times the work and the faults, want less than %.0f times",
			growth, small, scale, large, scale, 1.5*scale)
	}
}

// blockedSleep is how long blockInTheKernel sleeps: longer than the 10 ms after which the runtime
// gives the processor of a goroutine that a system call blocks to another thread in any case.
const blockedSleep = 20 * time.Millisecond

// blockInTheKernel blocks the calling thread in a system call for blockedSleep, so the runtime can
// give the goroutine's processor to another thread while it sleeps.
func blockInTheKernel(t *testing.T) {
	for sleep := (unix.Timespec{Nsec: blockedSleep.Nanoseconds()}); ; {
		err := unix.Nanosleep(&sleep, &sleep)
		if err == nil {
			return
		}
		if err != unix.EINTR {
			t.Fatalf("nanosleep: %v", err)
		}
	}
}

// holdEachProcessor runs two processors of the runtime and three goroutines of user work on them
// until the test ends, so that a goroutine whose system call returns finds no processor free and
// waits for one, which a thread that runs another goroutine gives it.
func holdEachProcessor(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	var stop atomic.Bool
	var spinners sync.WaitGroup
	for range 3 {
		spinners.Go(func() {
			for !stop.Load() {
				userWork(10_000)
			}
		})
	}
	t.Cleanup(func() {
		stop.Store(true)
		spinners.Wait()
		runtime.GOMAXPROCS(previous)
	})
}

// Growth refuses a window that it cannot read, with a line that says why: a window shorter than
// MinWindow (ErrShortWindow), of which the kernel's split of a thread's time into user and system
// time by the ticks of the scheduler is no small part; a window that reads no time; and a window
// whose two readings are of two threads. A window that blocks in a system call while other
// goroutines hold each processor ends on another thread unless onThread holds the goroutine on its
// thread: so with the lock taken out, the last row fails.
func TestGrowthRefusesAWindowThatItCannotRead(t *testing.T) {
	for _, test := range []struct {
		name         string
		scale        int
		small, large func()
		refusal      string
		is           error
	}{
		{"a window shorter than the floor", 2, func() { userWork(spinSteps / 5) }, func() { userWork(2 * spinSteps / 5) },
			"the smaller window lasts", ErrShortWindow},
		{"a window that reads no time", 1, func() {}, func() {}, "the smaller window reads 0s, no time", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, small, large, err := Growth(3, test.scale, Again(test.small), Again(test.large))
			if err == nil || !strings.Contains(err.Error(), test.refusal) || test.is != nil && !errors.Is(err, test.is) {
				t.Fatalf("Growth of windows of %v and %v answered %v, want a refusal that says %q (%v)", small, large, err, test.refusal, test.is)
			}
			t.Logf("refused: %v", err)
		})
	}
	t.Run("a window that blocks while other goroutines hold each processor", func(t *testing.T) {
		holdEachProcessor(t)
		const scale = 4
		growth, small, large, err := Growth(3, scale,
			Again(func() {
				userWork(spinSteps)
				blockInTheKernel(t)
			}),
			Again(func() {
				userWork(scale * spinSteps)
				blockInTheKernel(t)
			}))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("smaller window %v, larger window %v, %.1f times", small, large, growth)
		if growth >= 1.5*scale {
			t.Errorf("the work grew %.1f times, from %v for %d runs to %v for one run of %d times the work, want less than %.0f times",
				growth, small, scale, large, scale, 1.5*scale)
		}
	})
}
