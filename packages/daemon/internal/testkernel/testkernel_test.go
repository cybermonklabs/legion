package testkernel

import (
	"strings"
	"testing"
)

// The probe judges the status its shell read of itself, so each kind of kernel is a status here:
// Linux's, which records the shell's SIGUSR1 ignore and SIGUSR2 handler; the build cluster's
// gVisor's (kernel 4.19.0-gvisor), which has no mask line at all; and a kernel that prints the
// lines but not the process, all zero, as that gVisor's /proc/<pid>/stat gives them. Only the
// first has what a mask row needs, and each answer names what it saw.
func TestTheProbeHasTheMasksOnlyWhereTheyRecordItsShell(t *testing.T) {
	const linux = "Name:\tsh\nState:\tS (sleeping)\nSigQ:\t2/1546861\nSigPnd:\t0000000000000000\nShdPnd:\t0000000000000000\n" +
		"SigBlk:\t0000000000000000\nSigIgn:\t0000000000000200\nSigCgt:\t0000000000010802\nCapInh:\t0000000000000000\n"
	const gvisor = "Name:\tsh\nState:\tS (sleeping)\nThreads:\t1\nCapInh:\t0000000000000000\nSeccomp:\t0\n"
	const zero = "Name:\tsh\nSigBlk:\t0000000000000000\nSigIgn:\t0000000000000000\nSigCgt:\t0000000000000000\n"
	both := []string{"SigIgn", "SigCgt"}
	for _, tc := range []struct {
		name, status string
		lines        []string
		has          bool
		found        string
	}{
		{"Linux, both lines", linux, both, true, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200, and showed SigCgt 0000000000010802 with SIGUSR2's bit 0x800"},
		{"Linux, SigIgn alone", linux, []string{"SigIgn"}, true, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200"},
		{"gVisor, no mask line", gvisor, both, false, "showed no SigIgn line, and showed no SigCgt line"},
		{"lines that do not follow the process", zero, both, false, "showed SigIgn 0000000000000000 without SIGUSR1's bit 0x200, and showed SigCgt 0000000000000000 without SIGUSR2's bit 0x800"},
		{"the ignore recorded and the handler not", strings.Replace(linux, "0000000000010802", "0000000000010002", 1), both, false, "showed SigIgn 0000000000000200 with SIGUSR1's bit 0x200, and showed SigCgt 0000000000010002 without SIGUSR2's bit 0x800"},
		{"a line that is no mask", strings.Replace(linux, "0000000000000200", "-", 1), []string{"SigIgn"}, false, `showed SigIgn "-", which is no mask`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			has, found := judge(tc.status, tc.lines)
			if has != tc.has || found != tc.found {
				t.Fatalf("judge = %t, %q; want %t, %q", has, found, tc.has, tc.found)
			}
		})
	}
}
