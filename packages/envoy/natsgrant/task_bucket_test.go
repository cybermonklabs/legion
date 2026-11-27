package natsgrant_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/natsgrant"
)

// listenerTaskBucketReads are the reads of the Legion daemons' task bucket that Listener and
// ListenerHost list after the session consumers' calls, written out literally: the bucket's stream
// info, its direct and stored-message gets, its watch's consumer create and delete, and their
// flow-control replies. No create, update or write of the bucket: the daemon is its one writer.
var listenerTaskBucketReads = []string{
	"$JS.API.STREAM.INFO.KV_legion_tasks",
	"$JS.API.DIRECT.GET.KV_legion_tasks.>",
	"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
	"$JS.API.CONSUMER.CREATE.KV_legion_tasks.>",
	"$JS.API.CONSUMER.DELETE.KV_legion_tasks.>",
	"$JS.FC.KV_legion_tasks.>",
}

// daemonTaskBucketCalls are the calls Daemon lists after the notification stream's, written out
// literally: JetStream's account info, then the task bucket's stream info, create and update, its
// direct and stored-message gets, the writes to its keys, its stream's purge, its watch's consumer
// create and delete, and their flow-control replies.
var daemonTaskBucketCalls = []string{
	"$JS.API.INFO",
	"$JS.API.STREAM.INFO.KV_legion_tasks",
	"$JS.API.STREAM.CREATE.KV_legion_tasks",
	"$JS.API.STREAM.UPDATE.KV_legion_tasks",
	"$JS.API.DIRECT.GET.KV_legion_tasks.>",
	"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
	"$KV.legion_tasks.>",
	"$JS.API.STREAM.PURGE.KV_legion_tasks",
	"$JS.API.CONSUMER.CREATE.KV_legion_tasks.>",
	"$JS.API.CONSUMER.DELETE.KV_legion_tasks.>",
	"$JS.FC.KV_legion_tasks.>",
}

// Each call returns lists of its own, so a caller that narrows one, as the daemon's refusal test
// does, changes no later call's.
func TestDaemonGrantIsEachCallersToChange(t *testing.T) {
	first := natsgrant.Daemon()
	first.Publish[0] = "edited"
	first.Subscribe[0] = "edited"
	second := natsgrant.Daemon()
	require.Equal(t, "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", second.Publish[0],
		"a caller's edit reached a later grant's publish")
	require.Equal(t, "notifications.envoy.exceptions.notifications.role.>", second.Subscribe[0],
		"a caller's edit reached a later grant's subscribe")
}
