// Package fencetest holds what a test that reads queued outbox rows back needs of the fence every
// row carries (record.Fence): a case that compares what a row says of its issue's work compares it
// without the run and the phase entry the row was queued at, which a case of the fence itself reads
// instead (record.FenceOf).
package fencetest

import "github.com/sjawhar/legion/daemon/internal/record"

// Unfenced is payload without the parts of the fence a writer of this release stamps on it
// (record.Fenced): the phase entry and the pod, and the run on a kind whose run field is the
// fence's alone. A run a kind carried before the fence (a supervise request's, a gate seed's, a
// pod effect's, a branch's) is part of what the row says, and stays. A supervise start keeps its
// entry, which is its task's entry and no fence.
func Unfenced(payload record.OutboxPayload) record.OutboxPayload {
	switch value := payload.(type) {
	case record.StatusWrite:
		value.Generation, value.Entry = 0, 0
		return value
	case record.MessagePost:
		value.Generation, value.Entry = 0, 0
		return value
	case record.Notice:
		value.Generation, value.Entry = 0, 0
		return value
	case record.ControllerNotice:
		value.Generation, value.Entry = 0, 0
		return value
	case record.RoleMessage:
		value.Generation, value.Entry = 0, 0
		return value
	case record.PhaseEnded:
		value.Generation, value.Entry = 0, 0
		return value
	case record.HookPrompt:
		value.Generation, value.Entry = 0, 0
		return value
	case record.UpdateAsk:
		value.Generation, value.Entry = 0, 0
		return value
	case record.MergeForwardMessage:
		value.Generation, value.Entry = 0, 0
		return value
	case record.MergeQueueRemoval:
		value.Generation, value.Entry = 0, 0
		return value
	case record.ClaimRelease:
		value.Generation, value.Entry = 0, 0
		return value
	case record.CheckWakeDue:
		value.Generation = 0
		return value
	case record.CheckLook:
		value.Generation = 0
		return value
	case record.CheckBound:
		value.Generation = 0
		return value
	case record.SuperviseRequest:
		if value.Stops() {
			value.Entry, value.PodUID = 0, ""
		}
		return value
	case record.IssueSuspend:
		value.Entry, value.PodUID = 0, ""
		return value
	case record.IssueBundle:
		value.Entry, value.PodUID = 0, ""
		return value
	case record.IssueRelease:
		value.Entry, value.PodUID = 0, ""
		return value
	}
	return payload
}
