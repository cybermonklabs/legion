package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// keptBrokerURL is the secrets broker every test's deployment enrols its pods with (testOptions),
// and movedBrokerURL where an operator moves that broker afterwards.
const (
	keptBrokerURL  = "https://secrets.internal.example"
	movedBrokerURL = "https://secrets-next.internal.example"
)

// beforeTheEnrollment is a tester in a pod an older daemon built before
// runtime.kubernetes.agent_secrets, which carries neither the broker's token volume, nor the role's
// key directory, nor the exchange identity (builtWithout, agentSecretsVolume), running the
// generation that daemon started there, whose record names no broker URL (recordedNoBroker).
func beforeTheEnrollment(t *testing.T) (*rig, runtime.SpawnSpec, runtime.Locator) {
	t.Helper()
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	g.builtWithout(loc.Sandbox.Name, agentSecretsVolume)
	recordedNoBroker(t, g, loc)
	return g, spec, loc
}

// recordedNoBroker rewrites loc's address record into the one an older daemon wrote for a
// generation it started in a pod built before the enrollment, which it handed no broker:
// AGENT_SECRETS_URL unset.
func recordedNoBroker(t *testing.T, g *rig, loc runtime.Locator) {
	t.Helper()
	rewriteRecord(t, g, loc, func(record map[string]any) {
		for _, address := range record["addresses"].([]any) {
			if entry := address.(map[string]any); entry["variable"] == "AGENT_SECRETS_URL" {
				entry["name"], entry["sha256"] = "(unset)", addressDigest("")
			}
		}
	})
}

// A launch that keeps a pod built before the secrets broker's enrollment waits for that pod's
// update round. Such a pod carries no exchange identity either, so its role would have no
// identity to trade, and the Envoy listener takes no shared bearer: listenerCredential refuses the
// launch with runtime.ErrAwaitingPodRound naming the pod, before any start reaches the pod — so no
// role is told of a broker its pod does not mount — and before anything of the pod is touched.
// PodDrift still names the broker's token volume the pod lacks, the drift its round replaces it
// for.
func TestAKeptPodWithoutTheEnrollmentWaitsForItsRound(t *testing.T) {
	g, spec, loc := beforeTheEnrollment(t)
	r2 := restartedOver(t, g, loc, func(*Options) {})
	before := starts(g)
	g.clearActions()
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	_, err := r2.Resume(g.ctx, &loc, spec)
	wantAwaitingRound(t, g, loc, err, before,
		"pod "+loc.Sandbox.Name+" carries no exchange identity", "the Envoy listener, which takes no shared bearer", "its update round replaces it")
	if drifted := driftOf(t, g.ctx, r2, loc); !slices.ContainsFunc(drifted, func(d string) bool { return strings.HasPrefix(d, agentSecretsTokenVolume+" ") }) {
		t.Errorf("PodDrift = %q, want the missing enrollment among it, so the update round replaces the pod", drifted)
	}
}

// The record of a generation says what that generation was handed, never what the configuration
// reads: the launch writes it on the issue Sandbox before the start (recordAddresses), and it is
// the only account of the running process a later daemon has (movedEnvironment). A launch into a
// kept pod built before the enrollment is refused by listenerCredential before that write (the
// Envoy listener takes no shared bearer, and the pod carries no exchange identity to trade), so it
// starts no generation and writes no record: the record of the generation still running there is
// left as it was.
func TestAKeptPodWithoutTheEnrollmentKeepsTheRecordOfItsRunningGeneration(t *testing.T) {
	g, spec, loc := beforeTheEnrollment(t)
	held := g.sandbox(loc.Sandbox.Name).Annotations[addressesAnnotation(claim.RoleTester)]
	r2 := restartedOver(t, g, loc, func(*Options) {})
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	if _, err := r2.Resume(g.ctx, &loc, spec); !strings.Contains(errString(err), "the Envoy listener, which takes no shared bearer") {
		t.Fatalf("Resume = %v, want listenerCredential's refusal", err)
	}
	if after := g.sandbox(loc.Sandbox.Name).Annotations[addressesAnnotation(claim.RoleTester)]; after != held {
		t.Errorf("the refused launch rewrote the tester's address record from %s to %s, want it left as the running generation's", held, after)
	}
	var record addressRecord
	if err := json.Unmarshal([]byte(held), &record); err != nil || record.Generation != loc.Sandbox.Generation {
		t.Errorf("the tester's address record %s is not generation %d's (%v)", held, loc.Sandbox.Generation, err)
	}
}

// errString is err's text, "" for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Moving a broker a generation never held moves nothing of that generation. An observation
// compares the record with what a generation started now in that same pod would be handed
// (movedEnvironment), and a generation an older daemon started in a pod built before the
// enrollment was handed no broker URL and recorded none, and the pod carries no enrollment, so
// both sides hold no broker URL and the role reads Alive. Reading it StaleAddress instead would
// have the supervisor repoint the role mid-turn, which the update round exists to prevent before
// its bound: the pod is replaced once every worker in it has answered the update message, no turn
// runs and no claim's activity report holds it (internal/daemon's roundEnd.holding), or at the
// round's bound.
func TestAKeptPodsGenerationIsNotStaleWhenTheBrokerItNeverHeldMoves(t *testing.T) {
	g, _, loc := beforeTheEnrollment(t)
	r2 := restartedOver(t, g, loc, func(o *Options) { o.AgentSecrets.URL = movedBrokerURL })
	if obs := readopt(t, g.ctx, r2, loc)[0]; obs.Kind != runtime.Alive {
		t.Errorf("%s (%s), want Alive: the operator moved the broker from %s to %s, and the tester's generation was handed neither",
			obs.Kind, obs.Detail, keptBrokerURL, movedBrokerURL)
	}
}
