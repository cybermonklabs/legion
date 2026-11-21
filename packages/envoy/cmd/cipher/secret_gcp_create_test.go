// packages/envoy/cmd/cipher/secret_gcp_create_test.go
//
// secret create on GCP, against the fake Secret Manager of secret_gcp_test.go: the secret is made,
// empty and with its tags, before the value is read, so the store's refusal of the create asks for
// no value; a create that then stores no value deletes the secret it made, and names that secret
// and the command that clears it when the store refuses the delete too.
package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// newTestID is the store's whole name of NEW_TEST, the secret these tests create.
const newTestID = testGCPPrefix + "new-test"

// clearEmptySecret is the command a refused delete names for clearing NEW_TEST's empty secret.
const clearEmptySecret = "gcloud secrets delete agent-secrets-new-test --project " + testGCPProject

// onFirstRead answers r's bytes to the form's read of standard input, and runs first once, before
// the first of them reaches the form: at the moment the form reads the value.
type onFirstRead struct {
	r     io.Reader
	first func()
	ran   bool
}

func (o *onFirstRead) Read(p []byte) (int, error) {
	if !o.ran {
		o.ran = true
		o.first()
	}
	return o.r.Read(p)
}

// useStdinThen makes value the secret forms' standard input for the rest of t, running first at
// the form's first read of it.
func useStdinThen(t *testing.T, value string, first func()) {
	t.Helper()
	restore := secretStdin
	secretStdin = &onFirstRead{r: strings.NewReader(value), first: first}
	t.Cleanup(func() { secretStdin = restore })
}

// count is how many times calls names method.
func count(calls []string, method string) int {
	n := 0
	for _, call := range calls {
		if call == method {
			n++
		}
	}
	return n
}

// TestGCPSecretCreateTheStoreRefusesAsksForNoValue: where the store refuses the create itself - IAM
// refusing secretmanager.secrets.create, as it refuses a dev box's own identity the broker's
// namespace - the create ends before it reads the value, piped or at the prompt, exiting 1 with the
// store's refusal and the sign-in it refused, having written nothing and asked for no reread.
func TestGCPSecretCreateTheStoreRefusesAsksForNoValue(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods = map[string]codes.Code{"CreateSecret": codes.PermissionDenied}
			startFakeSecretManager(t, fake)
			useNoGcloud(t)
			useGCPIdentity(t, "", machineEmail)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			if terminal {
				useTerminal(t) // types nothing: a read at the prompt fails t
			} else {
				refuseValueReads(t)
			}

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "human")
			if code != 1 || !strings.Contains(stderr, "PermissionDenied") || !strings.Contains(stderr, "the store refused the metadata server: "+machineEmail) {
				t.Fatalf("exit %d, stderr %q; want 1 naming the store's refusal of the metadata server's identity", code, stderr)
			}
			if strings.Contains(stderr, "Value for") {
				t.Fatalf("stderr %q: the create prompted for a value the store had already refused", stderr)
			}
			requireNothingWritten(t, fake, broker)
			if calls := fake.called(); !slices.Equal(calls, []string{"CreateSecret"}) {
				t.Fatalf("Secret Manager calls %v, want the refused CreateSecret alone", calls)
			}
		})
	}
}

// TestGCPSecretCreateThatStoresNoValueLeavesNoSecret: once the create has made the secret, a value
// it then does not store - one it reads empty, one the minter of its type refuses, one the store
// refuses outright to add - leaves no secret behind: the create verifies the creation label,
// lists versions in case another writer added one, and deletes the empty secret it made. It
// exits as that failure does, says the empty secret it made is deleted and nothing stays, and
// asks the broker to reread the name once.
func TestGCPSecretCreateThatStoresNoValueLeavesNoSecret(t *testing.T) {
	for _, row := range []struct {
		name   string
		stdin  string
		refuse string // the one method the store refuses, or none
		args   []string
		code   int
		want   string
	}{
		{"an empty value", "", "", nil, exitUsageError, "the value is read from standard input, which was empty"},
		{"a value its type refuses", "not an App key", "", []string{"--type", policy.TypeGitHubApp, "--github-owners", "acme-org"}, exitUsageError,
			"the broker refuses this value as a github-app secret's"},
		{"a value the store refuses", "shh", "AddSecretVersion", nil, 1, "PermissionDenied"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			if row.refuse != "" {
				fake.refuseMethods = map[string]codes.Code{row.refuse: codes.PermissionDenied}
			}
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, row.stdin)

			_, stderr, code := runSecret(append([]string{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}, row.args...)...)
			if code != row.code || !strings.Contains(stderr, row.want) || strings.Contains(stderr, "an empty secret named") {
				t.Fatalf("exit %d, stderr %q; want %d naming %q and no secret left in the store", code, stderr, row.code, row.want)
			}
			if deleted := "the empty secret " + newTestID + " is deleted, so nothing stays in the store"; !strings.Contains(stderr, deleted) {
				t.Fatalf("stderr %q; want it to say %q", stderr, deleted)
			}
			requireNothingLeft(t, fake, broker, "NEW_TEST")
			calls := fake.called()
			if made, deleted := slices.Index(calls, "CreateSecret"), slices.Index(calls, "DeleteSecret"); made < 0 || deleted < made || count(calls, "DeleteSecret") != 1 || count(calls, "GetSecret") != 1 || count(calls, "ListSecretVersions") != 1 {
				t.Fatalf("Secret Manager calls %v; want the secret made, its identity and versions checked, then deleted once", calls)
			}
		})
	}
}

// requireNothingLeft fails t unless Secret Manager holds no secret and the broker was asked to
// reread name once, answering it absent: what a create that discarded its empty secret leaves.
func requireNothingLeft(t *testing.T, fake *fakeSecretManager, broker *secretBroker, name string) {
	t.Helper()
	if n := fake.written(); n != 0 {
		t.Fatalf("Secret Manager holds %d secrets after a create that stored no value", n)
	}
	if rereads, answers := broker.recorded(); !slices.Equal(rereads, []string{name}) || answers[0].Served || answers[0].Reason != policy.ReasonAbsent {
		t.Fatalf("rereads %v answered %v, want %s once, answered absent", rereads, answers, name)
	}
}

// TestGCPSecretCreateWhoseStoreAnswerIsAmbiguousNeverDeletesTheValue: an AddSecretVersion answered
// with a failure that is no refusal - Unavailable, DeadlineExceeded - may have stored the value, or
// may still store it late, so the create never deletes the secret after it. It lists the secret's
// versions to say what it found: with a version there it says the value may have been stored and
// asks the broker to reread the name; with none it says the store showed no version when listed,
// that a late add may still land, and asks the broker to reread too; where the list is refused it
// says which state is unknown. Each names the command that settles it and exits 1.
func TestGCPSecretCreateWhoseStoreAnswerIsAmbiguousNeverDeletesTheValue(t *testing.T) {
	for _, row := range []struct {
		name          string
		refuse, after map[string]codes.Code
		versions      int // the versions the secret holds once the create ends
		want          []string
		rereads       []string
	}{
		{"the value stored and its answer lost", nil, map[string]codes.Code{"AddSecretVersion": codes.Unavailable}, 1,
			[]string{"Unavailable", "may have been stored", "`agent-secrets secret show NEW_TEST`", "`agent-secrets secret set NEW_TEST`"}, []string{"NEW_TEST"}},
		{"the value not stored yet, its answer lost", map[string]codes.Code{"AddSecretVersion": codes.DeadlineExceeded}, nil, 0,
			[]string{"DeadlineExceeded", "showed no version", "a late add may still", "`agent-secrets secret show NEW_TEST`", "`" + clearEmptySecret + "`"}, []string{"NEW_TEST"}},
		{"the versions not listed either", map[string]codes.Code{"AddSecretVersion": codes.Unavailable, "ListSecretVersions": codes.PermissionDenied}, nil, 0,
			[]string{"Unavailable", "is unknown", "`agent-secrets secret show NEW_TEST`", "`" + clearEmptySecret + "`"}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods, fake.answerAfter = row.refuse, row.after
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			for _, want := range row.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q; want it to name %q", stderr, want)
				}
			}
			if strings.Contains(stderr, "stays in the store:") || strings.Contains(stderr, "shh") {
				t.Fatalf("stderr %q names an empty secret for one that may hold the value, or prints the value", stderr)
			}
			calls := fake.called()
			if listed := slices.Index(calls, "ListSecretVersions"); listed < slices.Index(calls, "AddSecretVersion") || slices.Contains(calls, "DeleteSecret") {
				t.Fatalf("Secret Manager calls %v; want the versions listed after the add, and no delete after an add whose answer was lost", calls)
			}
			fake.mu.Lock()
			entry, held := fake.secrets[newTestID]
			versions := -1
			if held {
				versions = len(entry.versions)
			}
			fake.mu.Unlock()
			if versions != row.versions {
				t.Fatalf("NEW_TEST holds %d versions (-1: no secret), want %d", versions, row.versions)
			}
			if rereads, _ := broker.recorded(); !slices.Equal(rereads, row.rereads) {
				t.Fatalf("rereads %v, want %v", rereads, row.rereads)
			}
		})
	}
}

// TestGCPSecretCreateWhoseEmptySecretCannotBeDeletedNamesIt: where the store refuses the delete of
// the empty secret too, the create says what it left - an empty secret of that whole name stays in
// the store - and the command that clears it, and exits 1 even where the value's own refusal is a
// usage error, since the store now holds something it did not hold before.
func TestGCPSecretCreateWhoseEmptySecretCannotBeDeletedNamesIt(t *testing.T) {
	for _, row := range []struct {
		name, stdin string
		refuse      []string
		want        string
	}{
		{"a value the store refuses", "shh", []string{"AddSecretVersion", "DeleteSecret"}, "PermissionDenied"},
		{"an empty value", "", []string{"DeleteSecret"}, "the value is read from standard input, which was empty"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods = map[string]codes.Code{}
			for _, method := range row.refuse {
				fake.refuseMethods[method] = codes.PermissionDenied
			}
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, row.stdin)

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, row.want) {
				t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, stderr, row.want)
			}
			for _, want := range []string{"an empty secret named " + newTestID + " stays in the store", "`" + clearEmptySecret + "`"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q; want it to name %q", stderr, want)
				}
			}
			fake.mu.Lock()
			entry, held := fake.secrets[newTestID]
			versions := 0
			if held {
				versions = len(entry.versions)
			}
			fake.mu.Unlock()
			if !held || versions != 0 || fake.written() != 1 {
				t.Fatalf("the store holds NEW_TEST %t with %d versions (%d secrets); want the one empty secret the message names", held, versions, fake.written())
			}
			if rereads, _ := broker.recorded(); len(rereads) != 0 {
				t.Fatalf("rereads = %v, want none", rereads)
			}
		})
	}
}

// TestGCPSecretCreateMakesTheSecretBeforeItReadsTheValue: by the time a create reads the value, it
// has made the secret, on the broker's key, with every tag it writes as an annotation, its own
// agent-secrets-create label (a random value, which no annotation carries) and no version yet; it
// then stores that value as the secret's one version, deletes nothing, leaves the label, and asks
// the broker to reread the name, which the broker then serves. The store refuses every
// UpdateSecret, as IAM refuses a teammate without secretmanager.secrets.update, and the create
// still succeeds, making no such call: a create needs that permission nowhere.
func TestGCPSecretCreateMakesTheSecretBeforeItReadsTheValue(t *testing.T) {
	fake := newFakeSecretManager()
	fake.refuseMethods = map[string]codes.Code{"UpdateSecret": codes.PermissionDenied}
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	want := map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagMaxGrant: "1h"}
	var heldAtRead bool
	var tagsAtRead, labelsAtRead map[string]string
	var keyAtRead string
	versionsAtRead := -1
	useStdinThen(t, "line one\nline two\n", func() {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		entry, held := fake.secrets[newTestID]
		heldAtRead = held
		if held {
			tagsAtRead = maps.Clone(entry.secret.GetAnnotations())
			labelsAtRead = maps.Clone(entry.secret.GetLabels())
			keyAtRead = entry.secret.GetReplication().GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName()
			versionsAtRead = len(entry.versions)
		}
	})

	stdout, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "human", "--max-grant", "1h")
	if code != 0 || !strings.Contains(stdout, "created NEW_TEST in GCP Secret Manager project ") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0 and the created line", code, stdout, stderr)
	}
	if !heldAtRead || !maps.Equal(tagsAtRead, want) || keyAtRead != testKMSKeyName || versionsAtRead != 0 {
		t.Fatalf("when the value was read the store held NEW_TEST %t, tags %v, key %q, %d versions; want it held with tags %v on %q and no version",
			heldAtRead, tagsAtRead, keyAtRead, versionsAtRead, want, testKMSKeyName)
	}
	if len(labelsAtRead) != 1 || !createLabelValue.MatchString(labelsAtRead[createLabel]) {
		t.Fatalf("when the value was read NEW_TEST carried labels %v; want %s alone, a random value of the create's own", labelsAtRead, createLabel)
	}
	fake.mu.Lock()
	entry := fake.secrets[newTestID]
	tags, labels, payloads := maps.Clone(entry.secret.GetAnnotations()), maps.Clone(entry.secret.GetLabels()), slices.Clone(entry.payloads)
	fake.mu.Unlock()
	if !maps.Equal(tags, want) || !maps.Equal(labels, labelsAtRead) || len(payloads) != 1 || string(payloads[0]) != "line one\nline two" {
		t.Fatalf("NEW_TEST holds tags %v, labels %v and %d versions; want tags %v, the create's label left as it was, and the one value read", tags, labels, len(payloads), want)
	}
	if calls := fake.called(); slices.Contains(calls, "UpdateSecret") || slices.Contains(calls, "DeleteSecret") || slices.Index(calls, "CreateSecret") > slices.Index(calls, "AddSecretVersion") {
		t.Fatalf("Secret Manager calls %v, want CreateSecret before AddSecretVersion, and no UpdateSecret or DeleteSecret", calls)
	}
	if rereads, answers := broker.recorded(); !slices.Equal(rereads, []string{"NEW_TEST"}) || !answers[0].Served {
		t.Fatalf("rereads %v answered %v, want NEW_TEST served", rereads, answers)
	}
}

// TestGCPSecretCreateWhoseCreateAnswerIsAmbiguousSettlesWhatItMade: the create labels the secret
// it makes agent-secrets-create=<a random value of its own>, so a secret it finds later is its own
// only where that label holds its value. Where CreateSecret fails with an answer that is no
// refusal the store gives before it acts (Unavailable here), the store may or may not have made
// the secret, so the create reads it back: a read that finds none says the store did not show it
// then, that a late create may still land, and names `secret show`; one carrying this create's
// label and no version is deleted again; one carrying it and holding a version, which this create
// did not add, is left as it is; one without this create's label - there before the create, or
// made by another create of the name between, as a racing create does - is left as it is, never
// listed nor deleted. A read that fails names the secret, this create's label value and the
// commands that tell and clear it, and a delete the store refuses names the gcloud command. Each
// exits 1, never reads the value, and asks the broker to reread only a name whose secret it
// deleted or left holding a version.
func TestGCPSecretCreateWhoseCreateAnswerIsAmbiguousSettlesWhatItMade(t *testing.T) {
	otherCreate := map[string]string{createLabel: "0000000000000000ffffffffffffffff"}
	describe := "`gcloud secrets describe agent-secrets-new-test --project " + testGCPProject + "`"
	for _, row := range []struct {
		name           string
		refuse, after  map[string]codes.Code
		existed        bool              // NEW_TEST, empty and unlabelled, is in the store before the create
		raced          map[string]string // labels another create of NEW_TEST makes it with just before this one's CreateSecret
		addVersion     bool              // another writer adds a version before the create lists them
		lists, deletes bool              // the create lists the secret's versions; it asks to delete the secret
		held           int               // the secrets the store holds once the create ends
		facts          []string          // what the refusal must say
		rereads        []string
	}{
		{name: "made and empty, so deleted", after: map[string]codes.Code{"CreateSecret": codes.Unavailable},
			lists: true, deletes: true, held: 0, rereads: []string{"NEW_TEST"},
			facts: []string{"made by this create", "deleted"}},
		{name: "not shown", refuse: map[string]codes.Code{"CreateSecret": codes.Unavailable},
			facts: []string{"did not find it", "may still land", "`agent-secrets secret show NEW_TEST`"}},
		{name: "made, its read back refused", after: map[string]codes.Code{"CreateSecret": codes.Unavailable}, refuse: map[string]codes.Code{"GetSecret": codes.PermissionDenied},
			held: 1, facts: []string{"unknown", describe, "`" + clearEmptySecret + "`"}},
		{name: "made, its versions not listed", after: map[string]codes.Code{"CreateSecret": codes.Unavailable}, refuse: map[string]codes.Code{"ListSecretVersions": codes.PermissionDenied},
			lists: true, held: 1, facts: []string{"made by this create", "`agent-secrets secret show NEW_TEST`", "`" + clearEmptySecret + "`"}},
		{name: "made and empty, its delete refused", after: map[string]codes.Code{"CreateSecret": codes.Unavailable}, refuse: map[string]codes.Code{"DeleteSecret": codes.PermissionDenied},
			lists: true, deletes: true, held: 1, facts: []string{newTestID + " stays in the store", "`" + clearEmptySecret + "`"}},
		{name: "made, then given a version by another writer", after: map[string]codes.Code{"CreateSecret": codes.Unavailable}, addVersion: true,
			lists: true, held: 1, rereads: []string{"NEW_TEST"}, facts: []string{"1 version(s)", "stays", "`agent-secrets secret show NEW_TEST`"}},
		{name: "held before the create", existed: true, refuse: map[string]codes.Code{"CreateSecret": codes.Unavailable},
			held: 1, facts: []string{"did not make", "left as it is"}},
		{name: "made by another create racing this one", raced: otherCreate, refuse: map[string]codes.Code{"CreateSecret": codes.Unavailable},
			held: 1, facts: []string{"did not make", "left as it is"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods, fake.answerAfter = row.refuse, row.after
			theirs := &secretmanagerpb.Secret{Name: newTestID, Annotations: map[string]string{policy.TagOwner: "grace@example.com", policy.TagTier: policy.TierAgent}, Labels: row.raced}
			if row.existed {
				fake.secrets[newTestID] = &fakeSecret{secret: theirs}
			}
			fake.beforeCall = func(method string) {
				switch {
				case method == "CreateSecret" && row.raced != nil:
					fake.mu.Lock()
					fake.secrets[newTestID] = &fakeSecret{secret: theirs}
					fake.mu.Unlock()
				case method == "ListSecretVersions" && row.addVersion:
					if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: newTestID, Payload: &secretmanagerpb.SecretPayload{Data: []byte("theirs")}}); err != nil {
						t.Errorf("another writer's add: %v", err)
					}
				}
			}
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			refuseValueReads(t)

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, "Unavailable") || !strings.Contains(stderr, "leaves open whether it made "+newTestID) {
				t.Fatalf("exit %d, stderr %q; want 1 naming Unavailable and that the store's answer leaves the create open", code, stderr)
			}
			for _, fact := range row.facts {
				if !strings.Contains(stderr, fact) {
					t.Fatalf("stderr %q; want it to say %q", stderr, fact)
				}
			}
			calls := fake.called()
			if slices.Index(calls, "CreateSecret") != 0 || slices.Index(calls, "GetSecret") < 1 || slices.Contains(calls, "ListSecretVersions") != row.lists || slices.Contains(calls, "DeleteSecret") != row.deletes {
				t.Fatalf("Secret Manager calls %v; want CreateSecret first, the secret read back after it, its versions listed %t, a delete asked for %t", calls, row.lists, row.deletes)
			}
			fake.mu.Lock()
			held, entry := len(fake.secrets), fake.secrets[newTestID]
			fake.mu.Unlock()
			if held != row.held {
				t.Fatalf("the store holds %d secrets, want %d", held, row.held)
			}
			switch {
			case row.existed || row.raced != nil:
				if entry.secret != theirs || !maps.Equal(entry.secret.GetLabels(), row.raced) {
					t.Fatalf("NEW_TEST is %v; want the other write's secret left as it was", entry.secret)
				}
			case held == 1:
				// The secret is this create's: it carries a label of the create's own, which a
				// read that fails names, so a person can tell it from another's.
				label := entry.secret.GetLabels()[createLabel]
				if !createLabelValue.MatchString(label) || (row.refuse["GetSecret"] != codes.OK && !strings.Contains(stderr, label)) {
					t.Fatalf("NEW_TEST's %s label is %q, stderr %q; want a random value of the create's own, named by a read that failed", createLabel, label, stderr)
				}
			}
			if rereads, _ := broker.recorded(); !slices.Equal(rereads, row.rereads) {
				t.Fatalf("rereads %v, want %v", rereads, row.rereads)
			}
		})
	}
}

// createLabelValue is the shape of the value create labels its secret with: 32 lowercase hex
// digits, 128 random bits, which a Secret Manager label value admits.
var createLabelValue = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestGCPSecretCreateEndedBySignalWhileReadingDeletesTheEmptySecret: while a create reads the
// value, onSignalDeath is set, and a hang-up, an interrupt, a quit or a termination runs it before
// it ends the form by that signal (endBySignal; in a real process, secret_create_signal_linux_test.go
// runs that with the value piped in, and secret_job_linux_test.go at the prompt). Here the read
// runs it as that signal would: it deletes the empty secret the create made, once, so the form ends
// having stored nothing; where the store refuses that delete too, it says so on stderr, naming the
// secret that stays and the command that clears it. Once the create returns, nothing is left set.
func TestGCPSecretCreateEndedBySignalWhileReadingDeletesTheEmptySecret(t *testing.T) {
	for _, refuseDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete refused=%t", refuseDelete), func(t *testing.T) {
			fake := newFakeSecretManager()
			if refuseDelete {
				fake.refuseMethods = map[string]codes.Code{"DeleteSecret": codes.PermissionDenied}
			}
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			heldAtDeath := -1
			useStdinThen(t, "", func() {
				if onSignalDeath == nil {
					t.Fatal("the create reads its value with nothing set for a signal to run")
				}
				onSignalDeath()
				heldAtDeath = fake.written()
			})

			_, stderr, _ := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if onSignalDeath != nil {
				t.Fatal("the create left onSignalDeath set once it returned")
			}
			if calls := fake.called(); count(calls, "DeleteSecret") != 1 {
				t.Fatalf("Secret Manager calls %v, want one DeleteSecret", calls)
			}
			stays := strings.Contains(stderr, "an empty secret named "+newTestID+" stays in the store") && strings.Contains(stderr, clearEmptySecret)
			if refuseDelete {
				if heldAtDeath != 1 || !stays {
					t.Fatalf("the store held %d secrets at the signal, stderr %q; want the empty secret, named with the command that clears it", heldAtDeath, stderr)
				}
				return
			}
			if heldAtDeath != 0 || stays {
				t.Fatalf("the store held %d secrets at the signal, stderr %q; want none, and no secret named as staying", heldAtDeath, stderr)
			}
		})
	}
}

// Installing the watcher before begin must not let an interrupt delete a secret this create
// never made, even if the requested name already exists.
func TestCreateInterruptedBeforeBeginNeverDeletesAnExistingSecret(t *testing.T) {
	fake := newFakeSecretManager()
	fake.secrets[newTestID] = &fakeSecret{secret: &secretmanagerpb.Secret{Name: newTestID}}
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	s, err := openSession(context.Background(), io.Discard, "secret create")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &pendingCreate{s: s, form: "secret create", name: "NEW_TEST", stdout: io.Discard, stderr: io.Discard, created: s.backend.prepareCreate("new-test", nil)}
	p.interrupted()
	if fake.written() != 1 || len(fake.called()) != 0 {
		t.Fatalf("secrets %d, calls %v; want the existing secret unchanged and no store call", fake.written(), fake.called())
	}
	if err := p.begin(); err == nil || len(fake.called()) != 0 {
		t.Fatalf("begin after interruption = %v, calls %v; want no create after interruption", err, fake.called())
	}
}

// Refusing this create's input must not delete a value another writer added while the prompt
// was open, or a replacement secret that now holds the name.
func TestGCPSecretCreateDiscardPreservesConcurrentWriters(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdinThen(t, "", func() {
				if replace {
					fake.mu.Lock()
					fake.secrets[newTestID] = &fakeSecret{secret: &secretmanagerpb.Secret{Name: newTestID, Etag: "replacement", Labels: map[string]string{createLabel: "another-create"}}}
					fake.mu.Unlock()
					return
				}
				if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: newTestID, Payload: &secretmanagerpb.SecretPayload{Data: []byte("another-writers-value")}}); err != nil {
					t.Fatal(err)
				}
			})
			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 || fake.written() != 1 || count(fake.called(), "DeleteSecret") != 0 {
				t.Fatalf("exit %d, held %d, calls %v, stderr %q; want a cleanup refusal preserving the other writer's secret", code, fake.written(), fake.called(), stderr)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			entry := fake.secrets[newTestID]
			if replace {
				if entry.secret.GetLabels()[createLabel] != "another-create" || !strings.Contains(stderr, "another writer") {
					t.Fatalf("replacement %v, stderr %q; want the other create's secret unchanged and identified", entry.secret, stderr)
				}
			} else if len(entry.versions) != 1 || string(entry.payloads[0]) != "another-writers-value" || !strings.Contains(stderr, "holds 1 version(s)") {
				t.Fatalf("versions %d, stderr %q; want the other writer's value unchanged and identified", len(entry.versions), stderr)
			}
		})
	}
}

// Recovery from a lost initial answer can delete a container the broker saw while it waited.
// That deletion must invalidate the broker just like a discard after the value prompt does.
func TestGCPSecretCreateLostAnswerCleanupRereadsBroker(t *testing.T) {
	fake := newFakeSecretManager()
	fake.answerAfter = map[string]codes.Code{"CreateSecret": codes.Unavailable}
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	refuseValueReads(t)
	_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
	if code != 1 || !strings.Contains(stderr, "Unavailable") {
		t.Fatalf("exit %d, stderr %q; want the original lost-answer failure", code, stderr)
	}
	requireNothingLeft(t, fake, broker, "NEW_TEST")
}

func TestGCPSecretCreateDiscardRejectsReplacementDuringDelete(t *testing.T) {
	fake := newFakeSecretManager()
	fake.beforeCall = func(method string) {
		if method == "DeleteSecret" {
			fake.mu.Lock()
			fake.secrets[newTestID] = &fakeSecret{secret: &secretmanagerpb.Secret{Name: newTestID, Etag: "replacement", Labels: map[string]string{createLabel: "another-create"}}}
			fake.mu.Unlock()
		}
	}
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "")
	_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
	if code != 1 || fake.written() != 1 || !strings.Contains(stderr, "changed before cleanup") {
		t.Fatalf("exit %d, held %d, stderr %q; want the changed etag to preserve the replacement", code, fake.written(), stderr)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if label := fake.secrets[newTestID].secret.GetLabels()[createLabel]; label != "another-create" {
		t.Fatalf("label %q; want the replacement secret unchanged", label)
	}
}

// blockedStdin is a standard input that never answers a read, as a pipe whose writer never writes.
type blockedStdin struct{}

func (blockedStdin) Read([]byte) (int, error) { select {} }

// TestGCPSecretCreateValueReadEndsAtItsWindow: a create reads its value for valueReadWindow at
// most, policy.PendingValueWindow (ten minutes), the time the broker holds the empty secret as
// pending, and here shortened: a value that has not come by then ends the read, the empty secret
// is discarded through the common cleanup, the broker asked to reread the name, and the create
// exits 1 naming the window and the command to run again, with nothing stored.
func TestGCPSecretCreateValueReadEndsAtItsWindow(t *testing.T) {
	if valueReadWindow != policy.PendingValueWindow || policy.PendingValueWindow != 10*time.Minute {
		t.Fatalf("the create reads its value for %s and the broker holds it pending for %s; want both ten minutes", valueReadWindow, policy.PendingValueWindow)
	}
	restore := valueReadWindow
	valueReadWindow = 300 * time.Millisecond
	t.Cleanup(func() { valueReadWindow = restore })
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	restoreStdin := secretStdin
	secretStdin = blockedStdin{}
	t.Cleanup(func() { secretStdin = restoreStdin })

	// Nothing ever comes on the create's standard input, so the create ending at all says the
	// window ended its read (returns).
	type result struct {
		stderr string
		code   int
	}
	got := returns(t, "the create's end at its value window", func() result {
		_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
		return result{stderr, code}
	})
	if got.code != 1 || !strings.Contains(got.stderr, "300ms") || !strings.Contains(got.stderr, "agent-secrets secret create NEW_TEST") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the window and the command to run again", got.code, got.stderr)
	}
	requireNothingLeft(t, fake, broker, "NEW_TEST")
}

// TestGCPSecretCreateWithoutTheRightToAddAValueAsksForNone: a sign-in IAM lets create the secret
// but not add its value (no secretmanager.versions.add) is told so before it is asked for the
// value: the create asks IAM which of that permission it holds on the secret it made, finds it
// withheld, deletes the empty secret through the common cleanup, asks the broker to reread the
// name, and exits 1 naming the permission and the sign-in, never reading the value. Where IAM
// cannot be asked, the create goes on to the prompt, and the add itself is the check.
func TestGCPSecretCreateWithoutTheRightToAddAValueAsksForNone(t *testing.T) {
	t.Run("withheld", func(t *testing.T) {
		fake := newFakeSecretManager()
		fake.withheld = []string{"secretmanager.versions.add"}
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		refuseValueReads(t)

		_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
		if code != 1 || !strings.Contains(stderr, "secretmanager.versions.add") || !strings.Contains(stderr, "the well-known credentials file") {
			t.Fatalf("exit %d, stderr %q; want 1 naming the permission withheld and the sign-in", code, stderr)
		}
		requireNothingLeft(t, fake, broker, "NEW_TEST")
		calls := fake.called()
		if asked := slices.Index(calls, "TestIamPermissions"); asked < slices.Index(calls, "CreateSecret") || slices.Contains(calls, "AddSecretVersion") || slices.Index(calls, "DeleteSecret") < asked {
			t.Fatalf("Secret Manager calls %v; want IAM asked after the create, no add, and the empty secret deleted after", calls)
		}
	})
	t.Run("not answered", func(t *testing.T) {
		fake := newFakeSecretManager()
		fake.refuseMethods = map[string]codes.Code{"TestIamPermissions": codes.Unavailable}
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("exit %d, stderr %q; want the create to go on to the value and store it", code, stderr)
		}
		if calls := fake.called(); !slices.Contains(calls, "AddSecretVersion") {
			t.Fatalf("Secret Manager calls %v; want the value added", calls)
		}
	})
}

// TestGCPSecretCreatesLabelTheirSecretsDifferently: two creates label the secrets they make with
// values of their own, so neither can take the other's secret for its own when it settles a lost
// answer.
func TestGCPSecretCreatesLabelTheirSecretsDifferently(t *testing.T) {
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	labels := map[string]bool{}
	for _, name := range []string{"NEW_TEST", "OTHER_TEST"} {
		useStdin(t, "shh")
		if _, stderr, code := runSecret("create", name, "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("create %s: exit %d, stderr %q", name, code, stderr)
		}
	}
	fake.mu.Lock()
	for _, entry := range fake.secrets {
		label := entry.secret.GetLabels()[createLabel]
		if !createLabelValue.MatchString(label) {
			t.Errorf("label %q is not a create's random value", label)
		}
		labels[label] = true
	}
	fake.mu.Unlock()
	if len(labels) != 2 {
		t.Fatalf("two creates labelled their secrets %v; want two different values", labels)
	}
}

// TestGCPSecretCreateRecoveryCommandsSignInAsTheCreateDid: every command a create prints for
// settling what it left - `agent-secrets secret show` and `set`, and the gcloud command that
// clears an empty secret - signs in as the create did: the agent-secrets lines carry its sign-in
// prefix (a key file GOOGLE_APPLICATION_CREDENTIALS names, the gcloud login CLOUDSDK_CORE_ACCOUNT
// names, the gcloud configuration CLOUDSDK_ACTIVE_CONFIG_NAME names), and the gcloud line pins the
// same identity for gcloud (the key file as CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE, the login as
// CLOUDSDK_CORE_ACCOUNT, the configuration as CLOUDSDK_ACTIVE_CONFIG_NAME) and names it.
func TestGCPSecretCreateRecoveryCommandsSignInAsTheCreateDid(t *testing.T) {
	for _, mode := range []struct {
		name   string
		signIn func(t *testing.T)
		// pins is the sign-in prefix of the agent-secrets lines and the pin of the gcloud line,
		// read once signIn has run.
		pins func() (agentPrefix, gcloudPin string)
	}{
		{"a key file", func(t *testing.T) {
			useGCPIdentity(t, "service_account", "robot@acme.iam.gserviceaccount.com")
		}, func() (string, string) {
			path := shellWord(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
			return "GOOGLE_APPLICATION_CREDENTIALS=" + path + " ", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE=" + path + " "
		}},
		{"a named gcloud login", func(t *testing.T) {
			useNoADC(t)
			useGcloud(t, "ada@example.com", "grace@example.com")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}, func() (string, string) {
			return "CLOUDSDK_CORE_ACCOUNT=grace@example.com ", "CLOUDSDK_CORE_ACCOUNT=grace@example.com "
		}},
		{"a gcloud configuration CLOUDSDK_ACTIVE_CONFIG_NAME names", func(t *testing.T) {
			useGcloudConfigurations(t, "box", orderConfigurations)
			t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person")
		}, func() (string, string) {
			return "CLOUDSDK_ACTIVE_CONFIG_NAME=person ", "CLOUDSDK_ACTIVE_CONFIG_NAME=person "
		}},
	} {
		for _, scenario := range []struct {
			name  string
			setUp func(fake *fakeSecretManager)
			want  func(agentPrefix, gcloudPin string) []string
		}{
			{"another writer's version kept", func(fake *fakeSecretManager) {
				fake.beforeCall = func(method string) {
					if method == "ListSecretVersions" {
						if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: newTestID, Payload: &secretmanagerpb.SecretPayload{Data: []byte("theirs")}}); err != nil {
							panic(err)
						}
					}
				}
			}, func(agentPrefix, _ string) []string {
				return []string{"`" + agentPrefix + "agent-secrets secret show NEW_TEST`", "`" + agentPrefix + "agent-secrets secret set NEW_TEST`"}
			}},
			{"the empty secret's delete refused", func(fake *fakeSecretManager) {
				fake.refuseMethods = map[string]codes.Code{"DeleteSecret": codes.PermissionDenied}
			}, func(_, gcloudPin string) []string { return []string{"`" + gcloudPin + clearEmptySecret + "`"} }},
		} {
			t.Run(mode.name+", "+scenario.name, func(t *testing.T) {
				fake := newFakeSecretManager()
				scenario.setUp(fake)
				startFakeSecretManager(t, fake)
				mode.signIn(t)
				startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				useStdin(t, "")

				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "ada@example.com", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
				}
				agentPrefix, gcloudPin := mode.pins()
				for _, want := range scenario.want(agentPrefix, gcloudPin) {
					if !strings.Contains(stderr, want) {
						t.Fatalf("stderr %q; want it to name %s", stderr, want)
					}
				}
			})
		}
	}
}

// TestCreateSignalAfterAFailedBeginPrintsItsFailure: a begin that failed - here a lost create
// answer whose settle deleted the empty secret it made, which leaves nothing - ends the stretch
// before the form prints that failure, so a signal that comes first, whose cleanup the form then
// never outruns, is the one to print it: the failure, with what it left, prints once, by the
// signal's cleanup.
func TestCreateSignalAfterAFailedBeginPrintsItsFailure(t *testing.T) {
	fake := newFakeSecretManager()
	fake.answerAfter = map[string]codes.Code{"CreateSecret": codes.Unavailable}
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	s, err := openSession(context.Background(), io.Discard, "secret create")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var stderr strings.Builder
	p := &pendingCreate{s: s, form: "secret create", name: "NEW_TEST", stdout: io.Discard, stderr: &stderr, created: s.backend.prepareCreate("new-test", nil)}
	if err := p.begin(); err == nil || fake.written() != 0 {
		t.Fatalf("begin = %v with %d secrets held; want the lost answer's failure and the empty secret deleted", err, fake.written())
	}
	p.interrupted()
	if out := stderr.String(); strings.Count(out, "agent-secrets secret create: ") != 1 || !strings.Contains(out, "Unavailable") || !strings.Contains(out, "so it is deleted again and nothing stays") {
		t.Fatalf("a signal after the failed begin printed %q; want that failure, and what it left, once", out)
	}
}

// signalAtFirstWrite is a standard error that runs onSignalDeath, as a signal would, at the form's
// first write to it after the line naming the credential it signs in with (afterUsing), while that
// is set. That line comes before the first store call, when no cleanup can be set yet, so a signal
// at it would prove nothing about the writes the create's cleanup could race.
type signalAtFirstWrite struct {
	strings.Builder
	fired bool
}

func (w *signalAtFirstWrite) Write(p []byte) (int, error) {
	if !w.fired && !strings.HasPrefix(string(p), "agent-secrets secret create: using ") {
		w.fired = true
		if death := onSignalDeath; death != nil {
			death()
		}
	}
	return w.Builder.Write(p)
}

// TestCreateFailedBeginPrintsItsFailureOnce: the form prints a failed begin only once the stretch
// is over, so a signal that comes as it prints finds no cleanup set and prints nothing more: one
// line, whether the begin left nothing or left the empty secret it could not delete.
func TestCreateFailedBeginPrintsItsFailureOnce(t *testing.T) {
	for _, row := range []struct {
		name   string
		refuse map[string]codes.Code
		want   string
	}{
		{"its empty secret deleted", nil, "so it is deleted again and nothing stays"},
		{"its empty secret kept", map[string]codes.Code{"DeleteSecret": codes.PermissionDenied}, newTestID + " stays in the store"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.answerAfter, fake.refuseMethods = map[string]codes.Code{"CreateSecret": codes.Unavailable}, row.refuse
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			refuseValueReads(t)
			stderr := &signalAtFirstWrite{}
			code := cmdSecret([]string{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}, io.Discard, stderr)
			if out := stderr.String(); code != 1 || strings.Count(afterUsing(t, out, "secret create"), "agent-secrets secret create: ") != 1 || !strings.Contains(out, row.want) {
				t.Fatalf("exit %d, stderr %q; want 1 and the failure, naming %q, printed once", code, out, row.want)
			}
		})
	}
}

// TestGCPSecretCreateWhoseDeleteAnswerIsLostReadsTheSecretBack: a DeleteSecret answered with a
// failure that is no refusal the store gives before it acts - Unavailable here - may have deleted
// the empty secret, so the create reads it back rather than say it stays: none found is the delete
// done, said as such, with the broker asked to reread the name and the value's own refusal's exit
// code kept; the secret still found, its versions listed again and none found, stays, said with the
// command that clears it; one found holding a version another writer added between the cleanup's
// list and the delete may hold a value, so it stays with no command that deletes it, and the broker
// is asked to reread the name; one whose versions cannot be listed again names the delete only for
// `versions: none`; a read back that fails says the store may still hold it, with the commands that
// tell and clear it, and never that it stays.
func TestGCPSecretCreateWhoseDeleteAnswerIsLostReadsTheSecretBack(t *testing.T) {
	const (
		readBackFails   = "the read back refused"
		versionAdded    = "another writer's version added"
		versionsUnknown = "the versions not listed again"
	)
	for _, row := range []struct {
		name          string
		refuse, after map[string]codes.Code
		atDelete      string // what happens as the delete is asked for, or nothing
		code, held    int
		facts, not    []string
		rereads       []string
	}{
		{"the delete done, its answer lost", nil, map[string]codes.Code{"DeleteSecret": codes.Unavailable}, "", exitUsageError, 0,
			[]string{"the value is read from standard input, which was empty", "the empty secret " + newTestID + " is deleted, so nothing stays in the store"},
			[]string{"an empty secret named", "may still hold"}, []string{"NEW_TEST"}},
		{"the delete not done, its answer lost", map[string]codes.Code{"DeleteSecret": codes.Unavailable}, nil, "", 1, 1,
			[]string{"Unavailable", "leaves open whether it deleted " + newTestID, "reading it back still finds it", "`" + clearEmptySecret + "`"}, nil, nil},
		{"the delete's answer lost and its read back failing", map[string]codes.Code{"DeleteSecret": codes.Unavailable}, nil, readBackFails, 1, 1,
			[]string{"Unavailable", "leaves open whether it deleted " + newTestID, "may still hold it", "`agent-secrets secret show NEW_TEST`", "`" + clearEmptySecret + "`"},
			[]string{"stays in the store"}, nil},
		{"the delete's answer lost and the secret given a version meanwhile", map[string]codes.Code{"DeleteSecret": codes.Unavailable}, nil, versionAdded, 1, 1,
			[]string{"Unavailable", "leaves open whether it deleted " + newTestID, "holds 1 version(s)", "`agent-secrets secret show NEW_TEST`", "`agent-secrets secret set NEW_TEST`"},
			[]string{"empty secret stays", clearEmptySecret}, []string{"NEW_TEST"}},
		{"the delete's answer lost and the versions not listed again", map[string]codes.Code{"DeleteSecret": codes.Unavailable}, nil, versionsUnknown, 1, 1,
			[]string{"Unavailable", "leaves open whether it deleted " + newTestID, "listing its versions failed", "only with `versions: none`", "`" + clearEmptySecret + "`"},
			[]string{"empty secret stays"}, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods, fake.answerAfter = maps.Clone(row.refuse), row.after
			if fake.refuseMethods == nil {
				fake.refuseMethods = map[string]codes.Code{}
			}
			fake.beforeCall = func(method string) {
				if method != "DeleteSecret" {
					return
				}
				switch row.atDelete {
				case readBackFails, versionsUnknown:
					refused := map[string]string{readBackFails: "GetSecret", versionsUnknown: "ListSecretVersions"}[row.atDelete]
					fake.mu.Lock()
					fake.refuseMethods[refused] = codes.Unavailable
					fake.mu.Unlock()
				case versionAdded:
					if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: newTestID, Payload: &secretmanagerpb.SecretPayload{Data: []byte("theirs")}}); err != nil {
						t.Errorf("another writer's add: %v", err)
					}
				}
			}
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "")

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != row.code {
				t.Fatalf("exit %d, stderr %q; want %d", code, stderr, row.code)
			}
			for _, fact := range row.facts {
				if !strings.Contains(stderr, fact) {
					t.Fatalf("stderr %q; want it to say %q", stderr, fact)
				}
			}
			for _, not := range row.not {
				if strings.Contains(stderr, not) {
					t.Fatalf("stderr %q; want no %q", stderr, not)
				}
			}
			calls := fake.called()
			if deleted := slices.Index(calls, "DeleteSecret"); deleted < 0 || !slices.Contains(calls[deleted:], "GetSecret") {
				t.Fatalf("Secret Manager calls %v; want the secret read back after the delete", calls)
			}
			if held := fake.written(); held != row.held {
				t.Fatalf("the store holds %d secrets, want %d", held, row.held)
			}
			if rereads, _ := broker.recorded(); !slices.Equal(rereads, row.rereads) {
				t.Fatalf("rereads %v, want %v", rereads, row.rereads)
			}
		})
	}
}

// TestGCPSecretCreateDeadlineNoticeSaysTheEmptySecretStaysUntilEnter: where the value deadline
// passes with an entry under way, the prompt reads on until the entry ends, and the create deletes
// its empty secret only then. So the notice the prompt prints at the deadline says that the empty
// secret, named, stays in the store until the entry ends, and names only what ends it from there
// (valueDeadlineNotice): Enter, and the interrupt key, by its name, where it ends the process; with
// no such key, ending the process; inside an open paste, where Ctrl-C is pasted text, finishing the
// paste first. The prompt is stubbed to give that notice, then end as the entry's Enter does.
func TestGCPSecretCreateDeadlineNoticeSaysTheEmptySecretStaysUntilEnter(t *testing.T) {
	for _, row := range []struct {
		name      string
		notice    valueDeadlineNotice
		want, not []string
	}{
		{"the interrupt key live", valueDeadlineNotice{interrupt: 0x03},
			[]string{"until you press Enter to end this entry, and the create deletes it then; Ctrl-C deletes it now"}, nil},
		{"another interrupt key", valueDeadlineNotice{interrupt: 0x7f},
			[]string{"until you press Enter to end this entry, and the create deletes it then; Delete deletes it now"}, []string{"Ctrl-C"}},
		{"another control-byte interrupt key", valueDeadlineNotice{interrupt: 0x1c},
			[]string{"and the create deletes it then; Ctrl-\\ deletes it now"}, []string{"Ctrl-C"}},
		{"a printable interrupt key", valueDeadlineNotice{interrupt: 'Z'},
			[]string{"and the create deletes it then; the Z key deletes it now"}, []string{"Ctrl-"}},
		{"a non-ASCII interrupt key", valueDeadlineNotice{interrupt: 0xe9},
			[]string{"and the create deletes it then; the key that sends byte 0xe9 deletes it now"}, []string{"Ctrl-"}},
		{"no interrupt key", valueDeadlineNotice{},
			[]string{"until you press Enter to end this entry, or end this process (close this terminal, or send it SIGTERM), and the create deletes it then"}, []string{"Ctrl-C"}},
		{"a paste open", valueDeadlineNotice{inPaste: true, interrupt: 0x03},
			[]string{"until this entry ends: finish the paste, then press Enter, and the create deletes it then"}, []string{"Ctrl-C"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useTerminal(t)
			readHidden = func(_ int, deadline time.Time, prompt func(), onDiscard func(error)) ([]byte, error) {
				if deadline.IsZero() {
					t.Fatal("a GCP create read its value with no deadline")
				}
				prompt()
				onDiscard(row.notice)
				return nil, errValueTimedOut
			}

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			for _, want := range append([]string{"The value deadline passed with an entry under way", "The empty secret NEW_TEST stays in the store until"}, row.want...) {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q; want the notice to say %q", stderr, want)
				}
			}
			for _, not := range row.not {
				if strings.Contains(stderr, not) {
					t.Fatalf("stderr %q; want no %q", stderr, not)
				}
			}
			requireNothingLeft(t, fake, broker, "NEW_TEST")
		})
	}
}
