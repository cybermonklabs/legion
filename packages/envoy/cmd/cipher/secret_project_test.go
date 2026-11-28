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

// TestCreateWritesTheProjectTag: create --project writes the secret's dispatch-project tag, the
// key as named, beside its owner and tier, on each store; a create naming no project writes no
// such tag, so the secret belongs to no project as before. The broker's answer to the reread
// that follows reaches the person at once: a broker that cannot check projects refuses the
// secret project-unchecked, which the create prints and exits 1 for.
func TestCreateWritesTheProjectTag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, "stored-value")
		if _, stderr, code := runSecret("create", "PRIV_KEY", "--owner", "me", "--tier", "agent", "--project", "PRIV"); code != 0 {
			t.Fatalf("create --project PRIV: exit %d, stderr %q", code, stderr)
		}
		tags := store.tags(t, "PRIV_KEY")
		if tags[policy.TagProject] != "PRIV" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
			t.Fatalf("PRIV_KEY's tags = %v, want %s=PRIV beside its owner and tier", tags, policy.TagProject)
		}
		useStdin(t, "stored-value")
		if _, stderr, code := runSecret("create", "PLAIN_KEY", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("create with no --project: exit %d, stderr %q", code, stderr)
		}
		if tags := store.tags(t, "PLAIN_KEY"); tags[policy.TagProject] != "" || len(tags) != 2 {
			t.Fatalf("PLAIN_KEY's tags = %v, want its owner and tier alone", tags)
		}
	})
	t.Run("the broker's refusal", func(t *testing.T) {
		local := secrets.NewLocal()
		startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Reason: policy.ReasonProjectUnchecked} })
		useAWS(t, local, testAccount, adaSignIn)
		useStdin(t, "stored-value")
		stdout, stderr, code := runSecret("create", "PRIV_KEY", "--owner", "me", "--tier", "agent", "--project", "PRIV")
		if code != 1 || !strings.Contains(stdout, "reason="+policy.ReasonProjectUnchecked) || !strings.Contains(stderr, policy.ReasonProjectUnchecked) {
			t.Fatalf("create under a broker that cannot check projects: exit %d stdout %q stderr %q; want exit 1 naming %s", code, stdout, stderr, policy.ReasonProjectUnchecked)
		}
		if got := secrets.TagMap(describe(t, local, "PRIV_KEY").Tags)[policy.TagProject]; got != "PRIV" {
			t.Fatalf("PRIV_KEY's project tag = %q, want PRIV: the write stands and the broker's answer says why it is not served", got)
		}
	})
}

// TestRetagNoneRemovesTheProjectTag: retag --project writes the tag and keeps every other tag,
// --project none removes it as every other none removes its own, and a removal beside another
// change is refused before anything is written, since it is a retag of its own.
func TestRetagNoneRemovesTheProjectTag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.onEachStore, main.useAWS, awsClients set at secret_test.go:375).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, "v1")
		if _, stderr, code := runSecret("retag", "API_KEY", "--project", "PRIV"); code != 0 {
			t.Fatalf("retag --project PRIV: exit %d, stderr %q", code, stderr)
		}
		tags := store.tags(t, "API_KEY")
		if tags[policy.TagProject] != "PRIV" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
			t.Fatalf("tags after --project PRIV = %v, want the project beside the kept owner and tier", tags)
		}
		_, stderr, code := runSecret("retag", "API_KEY", "--project", "none", "--tier", "human")
		if code != exitUsageError || !strings.Contains(stderr, "a retag of its own") {
			t.Fatalf("retag --project none --tier human: exit %d stderr %q; want a usage error saying a removal is a retag of its own", code, stderr)
		}
		if got := store.tags(t, "API_KEY"); got[policy.TagProject] != "PRIV" || got[policy.TagTier] != policy.TierAgent {
			t.Fatalf("a refused retag changed the tags to %v", got)
		}
		if _, stderr, code := runSecret("retag", "API_KEY", "--project", "none"); code != 0 {
			t.Fatalf("retag --project none: exit %d, stderr %q", code, stderr)
		}
		tags = store.tags(t, "API_KEY")
		if _, ok := tags[policy.TagProject]; ok {
			t.Fatalf("tags after --project none = %v, want no project", tags)
		}
		if tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierAgent {
			t.Fatalf("tags after --project none = %v, want owner and tier unchanged", tags)
		}
	})
}

// TestShowPrintsTheProject: show prints a project line holding the tag the store holds, and "-"
// for a secret carrying none; list prints it in a PROJECT column. Each form's --json carries the
// tag as the store holds it under project, and null for a secret carrying none, so a census of
// the store's tagged secrets counts the rows whose project is not null.
func TestShowPrintsTheProject(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	tagged := policytest.Secret("PRIV_KEY", "ada@example.com", policy.TierAgent, "v1")
	tagged.Tags[policy.TagProject] = "PRIV"
	plain := policytest.Secret("PLAIN_KEY", "ada@example.com", policy.TierAgent, "v1")
	local := secrets.NewLocal(tagged, plain)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)

	for _, tc := range []struct {
		name, printed string
		stored        *string
	}{
		{"PRIV_KEY", "PRIV", new("PRIV")},
		{"PLAIN_KEY", "-", nil},
	} {
		stdout, stderr, code := runSecret("show", tc.name)
		if code != 0 {
			t.Fatalf("show %s: exit %d, stderr %q", tc.name, code, stderr)
		}
		if want := "project: " + tc.printed + "\n"; !strings.Contains(stdout, want) {
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
		project, present := view["project"]
		if !present {
			t.Fatalf("show %s --json carries no project field: %s", tc.name, jsonOut)
		}
		switch {
		case tc.stored == nil && project != nil:
			t.Fatalf("show %s --json project = %v, want null for a secret carrying no tag", tc.name, project)
		case tc.stored != nil && project != *tc.stored:
			t.Fatalf("show %s --json project = %v, want %q", tc.name, project, *tc.stored)
		}
	}

	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	column := -1
	for i, name := range strings.Fields(lines[0]) {
		if name == "PROJECT" {
			column = i
		}
	}
	if column < 0 {
		t.Fatalf("list header = %q, want a PROJECT column", lines[0])
	}
	projects := map[string]string{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		projects[fields[0]] = fields[column]
	}
	if projects["PRIV_KEY"] != "PRIV" || projects["PLAIN_KEY"] != "-" || len(projects) != 2 {
		t.Fatalf("list PROJECT column = %v, want PRIV_KEY PRIV and PLAIN_KEY -", projects)
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
		if v["project"] != nil {
			taggedRows++
			if v["name"] != "PRIV_KEY" || v["project"] != "PRIV" {
				t.Fatalf("list --json row %v carries a project; only PRIV_KEY is tagged, with PRIV", v)
			}
		}
	}
	if len(views) != 2 || taggedRows != 1 {
		t.Fatalf("list --json = %s, want two rows and one whose project is not null", stdout)
	}
}

// TestAMalformedProjectIsRefusedBeforeAnyWrite: --project takes one Dispatch project key as
// written (policy.ParseProject), so a lowercase key, two keys, a key too short or too long and,
// on create, the word none are each a usage error naming --project and the reason the broker
// would refuse the secret for, refused before the sign-in is read or the store is called at all.
// The refusal does not print the value it refused (valueNotPrinted), as no tag flag's refusal does.
func TestAMalformedProjectIsRefusedBeforeAnyWrite(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1"))
	writes := &countingTagWrites{Local: local}
	startSecretBroker(t, servedBy(local))
	useAWS(t, writes, testAccount, adaSignIn)
	for _, args := range [][]string{
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project", "priv"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project", "PRIV PUB"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project", "P"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project", "ABCDEFGHIJK"},
		{"create", "NEW_KEY", "--owner", "me", "--tier", "agent", "--project", "none"},
		{"retag", "API_KEY", "--project", "priv"},
		{"retag", "API_KEY", "--project", " PRIV"},
		{"retag", "API_KEY", "--project", "PRIV,PUB"},
	} {
		useStdin(t, "stored-value")
		_, stderr, code := runSecret(args...)
		if code != exitUsageError || !strings.Contains(stderr, "--project") || !strings.Contains(stderr, policy.ReasonProjectMalformed) {
			t.Errorf("%q: exit %d stderr %q; want %d naming --project and %s", args, code, stderr, exitUsageError, policy.ReasonProjectMalformed)
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
}
