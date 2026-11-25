package supervise

import (
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// plainCause is the sentence the daemon tells a death by where the runtime read its cause
// (runtime.DeathCause), and "" for every other death. It is the daemon's own words, with numbers
// it formats from the quantities the runtime parsed (plainSize), and it names what the role's own
// work can change: the memory limit of a `roles` container the kernel killed for memory; the
// resource, the use and the request of the container a node-pressure eviction named over its
// memory or ephemeral-storage request, since that pod passed its request and not its limit; and
// the disk limit of an eviction for the pod's own disk (dispatch://INFRA-1117 R9). It reads after
// "because", as every cause a let-go records does (EndedWork.Cause), and it leads the death's
// cause wherever that is told: the claim's record of it, which the notices of a claim its deaths
// fail carry (deathCause), and the record of what the death ended, which the role's next task
// carries (endDied). The container a node-pressure eviction named is one the pod's spec holds,
// which the runtime checked, so no word of the kubelet's message reaches the sentence. Ephemeral
// storage over its request is told as the container's own disk, outside $TMPDIR, since that is
// the whole of what the kubelet counts as the container's there: $TMPDIR is on the issue's
// volume, which it does not count. In the role's next task alone the sentence of either disk
// eviction is followed by a note of what the kubelet counted and where large files go (diskTask,
// diskLimitTask).
//
// The memory limit's sentence is the one statement the role's next task makes that its pod ran
// out of memory: it says what outOfMemoryTask's own statement says, that the container runs every
// role of the issue, and names the limit, and the note behind it keeps only its guidance
// (outOfMemoryNote, which reads the record's EndedWork.Death).
func plainCause(cause runtime.DeathCause) string {
	switch cause.Kind {
	case runtime.DeathMemoryLimit:
		return plainKilled + plainSize(cause.Limit) + " memory limit"
	case runtime.DeathMemoryRequest:
		return plainEvicted + "for node memory pressure because its " + cause.Container + " container was using " +
			plainSize(cause.Use) + ", more than its " + plainSize(cause.Request) + " memory request"
	case runtime.DeathDiskRequest:
		return plainEvicted + "for node disk pressure because its " + cause.Container + " container was using " +
			plainSize(cause.Use) + " of its own disk, outside $TMPDIR, more than its " + plainSize(cause.Request) + " ephemeral storage request"
	case runtime.DeathDiskLimit:
		return plainEvicted + "because its container's own disk, outside $TMPDIR, passed its " + plainSize(cause.Limit) + " limit"
	}
	return ""
}

// The two openings every sentence of plainCause starts with: the kernel's kill of the `roles`
// container for memory, and the kubelet's eviction of the pod. endRecordedDeath reads a recorded
// cause as a plain sentence followed by the runtime's account only where it opens with one of
// them (opensAsPlainCause), and the runtime's account never does, since it opens with the
// observation's kind (deathAccount).
const (
	plainKilled  = "the pod's roles container, which runs every role of this issue, was killed because it used more than its "
	plainEvicted = "the kubelet evicted the pod "
)

// opensAsPlainCause is whether cause, a death's cause as the claim records it (deathCause), opens
// as every sentence of plainCause does, with plainKilled or plainEvicted: the one read of that
// record endRecordedDeath makes, since the claim keeps no kind beside the sentence. It sits beside
// the sentences it reads, so a sentence that opens another way is one place to change.
func opensAsPlainCause(cause string) bool {
	return strings.HasPrefix(cause, plainKilled) || strings.HasPrefix(cause, plainEvicted)
}

// diskTask follows the plain sentence of an eviction over the pod's ephemeral-storage request
// (runtime.DeathDiskRequest) in the role's next task (endDied), and it closes the note of an
// eviction for the pod's disk limit (diskLimitTask). The kubelet's message of an eviction over the
// request names only the container's own layer and its logs, so the note there says nothing of
// what else the pod's disk holds: $TMPDIR is on the issue's volume, outside what the kubelet
// counted, so that is where the role's large files go (dispatch://INFRA-1117 R9). The notices of
// a failed claim do not carry it, since their reader is not the role.
const diskTask = "Keep large files under $TMPDIR, on the issue's volume."

// diskLimitCounts is what the kubelet's ephemeral-storage limit counts of an issue pod, told to a
// role whose pod passed it (diskLimitTask): the role homes volume, an emptyDir that holds each
// role's home and its /tmp and /var/tmp, and the `roles` container's own logs and writable layer.
// The issue's volume, where $TMPDIR is, is a disk a claim provides, which the limit does not
// count, and so is the test database's where the deployment gives it one
// (runtime.kubernetes.test_postgres_disk, dispatch://INFRA-1117 R11).
const diskLimitCounts = "That limit counts each role's home, /tmp and /var/tmp, the container's logs and what is written in the container's own layer."

// testDatabaseCounts follows diskLimitCounts where the limit also counted the pod's test
// database (runtime.DeathCause.TestDatabaseCounted), a pod that keeps it in an emptyDir: the
// databases test runs make and never drop stay on that volume, and they filled it in 48 of the 53
// disk-limit evictions dispatch://INFRA-1117 R11 counted, so the role is told to drop them.
const testDatabaseCounts = "It also counts the pod's test database, which fills up with the databases test runs make and never drop. " +
	"Drop each database a test run makes when the run ends."

// diskLimitTask follows the plain sentence of an eviction for the pod's disk limit
// (runtime.DeathDiskLimit) in the role's next task (endDied): what that limit counted, the pod's
// test database among it where the runtime read the pod kept it on a counted volume, and then
// where large files go (diskTask). The notices of a failed claim do not carry it, since their
// reader is not the role.
func diskLimitTask(cause runtime.DeathCause) string {
	if cause.TestDatabaseCounted {
		return diskLimitCounts + " " + testDatabaseCounts + " " + diskTask
	}
	return diskLimitCounts + " " + diskTask
}

// runtimeDetail introduces the runtime's own account of a death behind the plain sentence of its
// cause (deathCause, endDied), so a reader tells the daemon's words from the runtime's.
const runtimeDetail = "The runtime's detail: "

// plainSize is a number of bytes as a person reads one: in GiB from one GiB up and in MiB from one
// MiB up, rounded to two decimals with the zeros that end them dropped (8 GiB, 15.34 GiB, 768 MiB),
// and in bytes below a MiB. Two decimals write a use more than 0.005 of its unit over a request as
// more than it, where one decimal would write 8.04 GiB as 8 GiB; a use less than 0.005 of its unit
// over a request still reads as equal to it (8 GiB and 1 MiB is written 8 GiB).
func plainSize(bytes int64) string {
	const mib, gib = 1 << 20, 1 << 30
	amount := func(value float64) string {
		return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(value, 'f', 2, 64), "0"), ".")
	}
	switch {
	case bytes >= gib:
		return amount(float64(bytes)/gib) + " GiB"
	case bytes >= mib:
		return amount(float64(bytes)/mib) + " MiB"
	}
	return strconv.FormatInt(bytes, 10) + " bytes"
}
