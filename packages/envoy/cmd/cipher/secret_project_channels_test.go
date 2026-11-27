package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestRetagWritesAndRemovesProjectChannels: create and retag --project-channels slack write the
// secret's project-channels tag beside its owner and tier, and the rollback guard in its requesters
// tag, on each store, and keep every other tag; --project-channels none removes it as every other
// none removes its own, and a removal beside another change is refused before anything is
// written, since it is a retag of its own. Any
// value but slack, and none on create, is a usage error naming --project-channels and the reason
// the broker would refuse the secret for, refused before the sign-in is read or the store is
// called at all. show prints a project_channels line holding the tag ("-" for none), list prints
// it in a PROJECT_CHANNELS column, and each form's --json carries it under project_channels, null
// for a secret carrying none, so a census of the tagged secrets counts the rows where it is not
// null.
func TestRetagWritesAndRemovesProjectChannels(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, "xoxb-stored")
		if _, stderr, code := runSecret("create", "SLACK_TOKEN", "--owner", "me", "--tier", "agent", "--project-channels", "slack"); code != 0 {
			t.Fatalf("create --project-channels slack: exit %d, stderr %q", code, stderr)
		}
		tags := store.tags(t, "SLACK_TOKEN")
		if tags[policy.TagProjectChannels] != policy.ProjectChannelsSlack || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent || tags[policy.TagRequesters] != policy.RequestersRollbackGuard || len(tags) != 4 {
			t.Fatalf("SLACK_TOKEN's tags = %v, want %s=%s beside its owner, its tier and the rollback guard alone", tags, policy.TagProjectChannels, policy.ProjectChannelsSlack)
		}

		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent, policy.TagProject: "PRIV"}, "v1")
		if _, stderr, code := runSecret("retag", "API_KEY", "--project-channels", "slack"); code != 0 {
			t.Fatalf("retag --project-channels slack: exit %d, stderr %q", code, stderr)
		}
		tags = store.tags(t, "API_KEY")
		if tags[policy.TagProjectChannels] != policy.ProjectChannelsSlack || tags[policy.TagProject] != "PRIV" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
			t.Fatalf("tags after --project-channels slack = %v, want the tag beside the kept project, owner and tier", tags)
		}

		_, stderr, code := runSecret("retag", "API_KEY", "--project-channels", "none", "--tier", "human")
		if code != exitUsageError || !strings.Contains(stderr, "a retag of its own") {
			t.Fatalf("retag --project-channels none --tier human: exit %d stderr %q; want a usage error saying a removal is a retag of its own", code, stderr)
		}
		if got := store.tags(t, "API_KEY"); got[policy.TagProjectChannels] != policy.ProjectChannelsSlack || got[policy.TagTier] != policy.TierAgent {
			t.Fatalf("a refused retag changed the tags to %v", got)
		}

		if _, stderr, code := runSecret("retag", "API_KEY", "--project-channels", "none"); code != 0 {
			t.Fatalf("retag --project-channels none: exit %d, stderr %q", code, stderr)
		}
		tags = store.tags(t, "API_KEY")
		if _, ok := tags[policy.TagProjectChannels]; ok {
			t.Fatalf("tags after --project-channels none = %v, want no project-channels tag", tags)
		}
		if tags[policy.TagProject] != "PRIV" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
			t.Fatalf("tags after --project-channels none = %v, want project, owner and tier unchanged", tags)
		}
	})

	t.Run("a malformed value is refused before any write", func(t *testing.T) {
		local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1"))
		writes := &countingTagWrites{Local: local}
		startSecretBroker(t, servedBy(local))
		useAWS(t, writes, testAccount, adaSignIn)
		for _, args := range [][]string{
			{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project-channels", "SLACK"},
			{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project-channels", "discord"},
			{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project-channels", "none"},
			{"retag", "API_KEY", "--project-channels", "Slack"},
			{"retag", "API_KEY", "--project-channels", " slack"},
			{"retag", "API_KEY", "--project-channels", "slack none"},
			{"retag", "API_KEY", "--project-channels", "C0123ABCD"},
		} {
			useStdin(t, "xoxb-stored")
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "--project-channels") || !strings.Contains(stderr, policy.ReasonProjectChannelsMalformed) {
				t.Errorf("%q: exit %d stderr %q; want %d naming --project-channels and %s", args, code, stderr, exitUsageError, policy.ReasonProjectChannelsMalformed)
			}
			if value := args[len(args)-1]; !strings.Contains(stderr, valueNotPrinted) || strings.Contains(stderr, strconv.Quote(value)) {
				t.Errorf("%q: stderr %q; want it to say %s and not to print %q", args, stderr, valueNotPrinted, value)
			}
		}
		if writes.describes != 0 || writes.tags != 0 || writes.untags != 0 {
			t.Fatalf("refused commands made %d DescribeSecret, %d TagResource and %d UntagResource calls; want none", writes.describes, writes.tags, writes.untags)
		}
		if _, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("NEW_KEY"))}); err == nil {
			t.Fatal("a refused create made NEW_KEY")
		}
	})

	t.Run("show, list and --json print it", func(t *testing.T) {
		tagged := policytest.Secret("SLACK_TOKEN", policy.OwnerShared, policy.TierAgent, "v1")
		tagged.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
		plain := policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "v1")
		local := secrets.NewLocal(tagged, plain)
		startSecretBroker(t, servedBy(local))
		useAWS(t, local, testAccount, adaSignIn)

		for _, tc := range []struct {
			name, printed string
			stored        *string
		}{
			{"SLACK_TOKEN", policy.ProjectChannelsSlack, new(policy.ProjectChannelsSlack)},
			{"PLAIN_KEY", "-", nil},
		} {
			stdout, stderr, code := runSecret("show", tc.name)
			if code != 0 {
				t.Fatalf("show %s: exit %d, stderr %q", tc.name, code, stderr)
			}
			if want := "\nproject_channels: " + tc.printed + "\n"; !strings.Contains(stdout, want) {
				t.Fatalf("show %s stdout %q must contain %q", tc.name, stdout, want)
			}
			jsonOut, stderr, code := runSecret("show", tc.name, "--json")
			if code != 0 {
				t.Fatalf("show %s --json: exit %d, stderr %q", tc.name, code, stderr)
			}
			var view map[string]any
			if err := json.Unmarshal([]byte(jsonOut), &view); err != nil {
				t.Fatalf("show %s --json is not a secret: %v: %s", tc.name, err, jsonOut)
			}
			channels, present := view["project_channels"]
			if !present {
				t.Fatalf("show %s --json carries no project_channels field: %s", tc.name, jsonOut)
			}
			switch {
			case tc.stored == nil && channels != nil:
				t.Fatalf("show %s --json project_channels = %v, want null for a secret carrying no tag", tc.name, channels)
			case tc.stored != nil && channels != *tc.stored:
				t.Fatalf("show %s --json project_channels = %v, want %q", tc.name, channels, *tc.stored)
			}
		}

		stdout, stderr, code := runSecret("list")
		if code != 0 {
			t.Fatalf("list: exit %d, stderr %q", code, stderr)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		column := -1
		for i, name := range strings.Fields(lines[0]) {
			if name == "PROJECT_CHANNELS" {
				column = i
			}
		}
		if column < 0 {
			t.Fatalf("list header = %q, want a PROJECT_CHANNELS column", lines[0])
		}
		printed := map[string]string{}
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			printed[fields[0]] = fields[column]
		}
		if printed["SLACK_TOKEN"] != policy.ProjectChannelsSlack || printed["PLAIN_KEY"] != "-" || len(printed) != 2 {
			t.Fatalf("list PROJECT_CHANNELS column = %v, want SLACK_TOKEN %s and PLAIN_KEY -", printed, policy.ProjectChannelsSlack)
		}

		stdout, stderr, code = runSecret("list", "--json")
		if code != 0 {
			t.Fatalf("list --json: exit %d, stderr %q", code, stderr)
		}
		var views []map[string]any
		if err := json.Unmarshal([]byte(stdout), &views); err != nil {
			t.Fatalf("list --json is not a JSON array of secrets: %v: %s", err, stdout)
		}
		taggedRows := 0
		for _, v := range views {
			if _, present := v["project_channels"]; !present {
				t.Fatalf("list --json row %v carries no project_channels field", v)
			}
			if v["project_channels"] != nil {
				taggedRows++
				if v["name"] != "SLACK_TOKEN" || v["project_channels"] != policy.ProjectChannelsSlack {
					t.Fatalf("list --json row %v carries project_channels; only SLACK_TOKEN is tagged, with %s", v, policy.ProjectChannelsSlack)
				}
			}
		}
		if len(views) != 2 || taggedRows != 1 {
			t.Fatalf("list --json = %s, want two rows and one whose project_channels is not null", stdout)
		}
	})
}
