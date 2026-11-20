package sandbox

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// leakCheckPushedAnnotation records on an issue Sandbox's own metadata, never on its pod
// template, the manifest digest of the setting's check this daemon pushed into a pod whose template
// records none (leakCheckerAnnotation), with that pod's uid (pushedLeakCheck). Such a pod was built
// under no setting, and its launcher holds no install until a daemon running the setting keeps the
// pod and pushes one (serveLeakCheck), when its roles are told the setting's digest (relaunch). The
// launcher then holds that install for the rest of its life and says so in every later hello, so
// without the record a role started after the launcher reconnected — a daemon restart, or a
// dropped connection — would be told no digest, and its guard would run the pushed check unhashed.
// The Sandbox controller builds the pod from the template alone, so the record changes nothing of
// the pod: no shape drift, no new Sandbox generation, and no pod recreation. It names the pod, so
// the pod that replaces this one, whose own template says what it was built under, never reads it.
const leakCheckPushedAnnotation = "legion.dev/leak-check-pushed-manifest-sha256"

// pushedLeakCheck is the value of leakCheckPushedAnnotation: the pod the check was pushed into and
// the setting's manifest digest it was pushed under.
type pushedLeakCheck struct {
	Pod            string `json:"pod"`
	ManifestSHA256 string `json:"manifestSHA256"`
}

// recordedLeakChecker is the manifest digest the pod podUID records of its install, from its
// Sandbox's template annotations and the Sandbox's own: the digest the template was built under
// where it records one, and otherwise the one this daemon pushed into that very pod
// (leakCheckPushedAnnotation). A push record of another pod, and one that cannot be read, records
// nothing: the launch then reads the pod as one that records none, and records the push again
// where it pushes (relaunch).
func recordedLeakChecker(template, own map[string]string, podUID string) (string, bool) {
	if held, recorded := template[leakCheckerAnnotation]; recorded {
		return held, true
	}
	raw, ok := own[leakCheckPushedAnnotation]
	if !ok {
		return "", false
	}
	var pushed pushedLeakCheck
	if err := json.Unmarshal([]byte(raw), &pushed); err != nil || pushed.Pod == "" || pushed.Pod != podUID {
		return "", false
	}
	return pushed.ManifestSHA256, true
}

// recordLeakCheckPushed records on s, the issue Sandbox as read, that this daemon pushes the
// setting's check into its pod podUID (leakCheckPushedAnnotation): a merge patch of that one
// annotation of the Sandbox's own metadata, carrying s's uid as recordAddresses's does, so a
// Sandbox deleted and recreated in between is never written. It runs before the push, so no install
// this daemon makes in a pod that records no digest is ever without its record.
func (r *Runtime) recordLeakCheckPushed(ctx context.Context, s *sandbox, podUID string) error {
	record, err := json.Marshal(pushedLeakCheck{Pod: podUID, ManifestSHA256: r.leakCheck.ManifestSHA256})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid":         string(s.UID),
		"annotations": map[string]string{leakCheckPushedAnnotation: string(record)},
	}})
	if err != nil {
		return err
	}
	if _, err := r.sandboxClient().Patch(ctx, s.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("record the leak check pushed into pod %s on sandbox %s: %w", podUID, s.Name, err)
	}
	return nil
}

// leakCheckDrift is why a pod was built under, or pushed, another leak check than the runtime
// runs now: held is the manifest digest it records (recordedLeakChecker), and the reason names it
// and the setting's, "none" where the runtime runs no check now. A pod that records none says
// nothing, as a template with no shape compares by image alone (podShapeDrift): a pod built under
// no setting, or by a daemon before the record, that no daemon has pushed the setting's check
// into is no drift for it. An update round opens over a pod that has drifted so, and its workers
// are told the digest the pod records while they push (keptPod.checker), so a corrected digest, or
// a moved repository, reaches every pod.
func (r *Runtime) leakCheckDrift(held string, recorded bool) []string {
	if !recorded || held == r.leakCheck.ManifestSHA256 {
		return nil
	}
	now := r.leakCheck.ManifestSHA256
	if now == "" {
		now = "none"
	}
	return []string{"leak check manifest " + held + ", now " + now}
}
