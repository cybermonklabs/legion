package daemonpermissions

import (
	"slices"
	"testing"
)

// The daemon's grant is written out literally rather than built from Envoy's names, so a change
// to the grant or to a name it is built from fails here until reviewed: the notification stream's
// calls of the daemon's durables and its state read, then the task bucket's, which the daemon
// creates, reads, writes and watches.
func TestDaemonPermissionsAreTheStreamsCallsThenTheTaskBuckets(t *testing.T) {
	want := Permissions{
		Publish: []string{
			"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
			"$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS",
			"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>",
			"$JS.ACK.ENVOY_NOTIFICATIONS.>",
			"$JS.API.INFO",
			"$JS.API.STREAM.INFO.KV_legion_tasks",
			"$JS.API.STREAM.CREATE.KV_legion_tasks",
			"$JS.API.STREAM.UPDATE.KV_legion_tasks",
			"$JS.API.DIRECT.GET.KV_legion_tasks.>",
			"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
			"$KV.legion_tasks.>",
			"$JS.API.CONSUMER.CREATE.KV_legion_tasks.>",
			"$JS.API.CONSUMER.DELETE.KV_legion_tasks.>",
			"$JS.FC.KV_legion_tasks.>",
		},
		Subscribe: []string{
			"notifications.envoy.exceptions.notifications.role.>",
			"_INBOX.>",
		},
	}
	got := Grant()
	if !slices.Equal(got.Publish, want.Publish) {
		t.Errorf("Grant().Publish =\n%q\nwant\n%q", got.Publish, want.Publish)
	}
	if !slices.Equal(got.Subscribe, want.Subscribe) {
		t.Errorf("Grant().Subscribe =\n%q\nwant\n%q", got.Subscribe, want.Subscribe)
	}
}

// Each call returns lists of its own, so a caller that narrows one, as the daemon's refusal test
// does, changes no later call's.
func TestDaemonPermissionsAreEachCallersToChange(t *testing.T) {
	first := Grant()
	first.Publish[0] = "edited"
	first.Subscribe[0] = "edited"
	second := Grant()
	if second.Publish[0] != "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS" {
		t.Errorf("a caller's edit reached a later grant's publish: %q", second.Publish[0])
	}
	if second.Subscribe[0] != "notifications.envoy.exceptions.notifications.role.>" {
		t.Errorf("a caller's edit reached a later grant's subscribe: %q", second.Subscribe[0])
	}
}
