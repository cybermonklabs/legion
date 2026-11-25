package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A role process's addresses are fixed when it is launched, and a daemon restarted with its key
// changed hands other ones (handedAddresses): the worker stream is fixed for the issue pod's life,
// in the pod's one container's launcher command, and the other five for one generation's life, in the
// start command the launcher keeps to itself. So the stream is read from the pod spec, and each
// role's five are recorded on the issue Sandbox when the daemon starts a generation, one annotation
// per role, which a later daemon reads from its informer cache (evaluate).
//
// A record names each address without its credentials — its scheme, host and port, `xxxxx@` when it
// carries userinfo, never its path, query or fragment — and holds the sha256 of its whole value,
// which is what is compared. The raw values travel only in the start command, so a URL's
// credentials never land in a cluster object.

// addressesAnnotation is the Sandbox annotation that records the addresses role's running
// generation was started with.
func addressesAnnotation(role claim.Role) string {
	return "legion.dev/addresses-" + string(role)
}

// addressRecord is one role generation's recorded addresses, and whether its start carried a
// Dispatch token.
//
// DispatchToken is whether the generation's start command carried DISPATCH_TOKEN in its Files
// (the shared bearer, or the transition token under dispatch_token_broker), which the launcher
// writes into the generation's private directory and deletes when the generation ends: what a
// restarted daemon counts its live Dispatch tokens from (Runtime.DispatchTokens). No comparison
// reads it — movedEnvironment compares Addresses alone — so it moves no generation to
// StaleAddress. Nil in a record a daemon before the field wrote (recordedDispatchToken).
type addressRecord struct {
	Generation    uint64            `json:"generation"`
	Addresses     []recordedAddress `json:"addresses"`
	DispatchToken *bool             `json:"dispatchToken,omitempty"`
}

// recordedAddress is one address of a record: the variable that carries it, its name as
// namedURL (or namedNATSURLs) prints it, and the sha256 of its whole value, hex-encoded.
type recordedAddress struct {
	Variable string `json:"variable"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
}

// environmentAddresses are what a generation of role carries in its environment that a daemon
// restarted under another configuration would hand otherwise: handedAddresses but the stream its
// launcher dials, and, for the controller, the project's Slack channels (slackChannelsVariable),
// so a changed list relaunches it, resuming its session, as a moved address does. enrolled is the
// one enrollment decision for the generation (Runtime.enrollsIn), which decides its
// AGENT_SECRETS_URL exactly as it decides the two variables mainEnvironment sets.
func (r *Runtime) environmentAddresses(role claim.Role, enrolled bool) []handedAddress {
	addresses := slices.DeleteFunc(r.handedAddresses(enrolled), func(a handedAddress) bool { return a.name == connectFlag })
	if role == claim.RoleController {
		addresses = append(addresses, handedAddress{slackChannelsVariable, r.slackChannelsValue()})
	}
	return addresses
}

// slackChannelsValue is LEGION_SLACK_CHANNELS as the controller is told it: the project's channels,
// comma-joined, "" for none.
func (r *Runtime) slackChannelsValue() string {
	return strings.Join(r.controllerSlackChannels, ",")
}

// addressName is how an address handed now is named in a record and an observation's detail. The
// channel list is no URL and no credential, so it is named as it is, "(none)" when empty.
func (r *Runtime) addressName(a handedAddress) string {
	switch a.name {
	case "ENVOY_NATS_URL":
		return namedNATSURLs(r.natsURLs)
	case slackChannelsVariable:
		if a.value == "" {
			return "(none)"
		}
		return a.value
	}
	return namedURL(a.value)
}

func addressDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// recordFor is the record of the addresses a generation of role started now is handed, encoded as
// its annotation holds it. enrolled is the launch's own enrollment decision for the pod that
// generation runs in (launch.enrolls, Runtime.enrollsIn), so a generation started in a kept pod
// that carries no enrollment is recorded as carrying no broker URL — which is what it was handed.
// dispatchToken is whether the generation's start carries DISPATCH_TOKEN, the launch's own
// Dispatch decision (prepare's bearer, dispatchCredential's transition token).
func (r *Runtime) recordFor(role claim.Role, generation uint64, enrolled, dispatchToken bool) (string, error) {
	record := addressRecord{Generation: generation, DispatchToken: &dispatchToken}
	for _, a := range r.environmentAddresses(role, enrolled) {
		record.Addresses = append(record.Addresses, recordedAddress{Variable: a.name, Name: r.addressName(a), SHA256: addressDigest(a.value)})
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode the address record of generation %d: %w", generation, err)
	}
	return string(encoded), nil
}

// recordAddresses records on s, the issue Sandbox as read, the addresses a generation of role
// started now with the enrollment decision enrolled is handed, and whether its start carries
// DISPATCH_TOKEN (dispatchToken): a merge patch of role's one annotation, so the other roles'
// records stand.
// The patch carries s's uid, which the API server refuses to change on a live object, so a Sandbox
// deleted and recreated in between is never written. It runs before the generation's start command
// is sent, so a generation that runs has its record.
func (r *Runtime) recordAddresses(ctx context.Context, s *sandbox, role claim.Role, generation uint64, enrolled, dispatchToken bool) error {
	record, err := r.recordFor(role, generation, enrolled, dispatchToken)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid":         string(s.UID),
		"annotations": map[string]string{addressesAnnotation(role): record},
	}})
	if err != nil {
		return err
	}
	if _, err := r.sandboxClient().Patch(ctx, s.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("record the addresses of %s generation %d on sandbox %s: %w", role, generation, s.Name, err)
	}
	return nil
}

// recordedDispatchToken is whether a generation carried DISPATCH_TOKEN as its record says, nil
// for a generation with no record of its own. A record without the field, and a generation with
// no record at all, were written by a daemon before the field, which gave every launch the token
// whenever Dispatch was configured, so they read as carrying it exactly when this runtime has a
// Dispatch URL. Read as carrying, a generation is counted until it ends, which keeps a deployment
// waiting for it rather than removing the transition token under a role still holding one.
func (r *Runtime) recordedDispatchToken(record *addressRecord) bool {
	if record == nil || record.DispatchToken == nil {
		return r.dispatchURL != ""
	}
	return *record.DispatchToken
}

// movedAddress is one thing a role process holds that one launched now is not handed: where it
// carries it (connectFlag, one of the variables mainEnvironment sets, or the agentSecretsTokenVolume
// volume), what it holds there and what a process launched now is handed, each named so it carries
// no credential (namedURL, enrollmentName).
type movedAddress struct{ where, held, handed string }

// String is the one way an observation's detail names a moved address: `<where> <held>, now
// <handed>`.
func (m movedAddress) String() string { return m.where + " " + m.held + ", now " + m.handed }

// describeMoved is moved as an observation's detail names them, one after another.
func describeMoved(moved []movedAddress) string {
	named := make([]string, len(moved))
	for i, m := range moved {
		named[i] = m.String()
	}
	return strings.Join(named, "; ")
}

// movedEnvironment compares role's recorded generation with the addresses a generation launched
// now in pod, the live pod that generation runs in, is handed, one movedAddress for each that
// moved, its held name the record's. The pod is what decides the broker: a generation in a pod
// that carries no enrollment is handed no broker URL now, as it was handed none when it started
// (Runtime.enrollsIn), so moving the broker's URL leaves such a generation alive and its pod to
// its update round. A Sandbox with no record of role, or a record of another generation, has
// nothing to compare: a generation started before records existed is never read as stale for want
// of one. A record that cannot be read is an error, which proves nothing about the generation's
// addresses: evaluate reads it as stale, and the relaunch that follows writes a fresh one.
//
// dispatchToken is, from the same read, whether the generation's start carried DISPATCH_TOKEN
// (recordedDispatchToken), which no comparison here reads: a generation with no record of its
// own, or an unreadable one, reads as one whose record lacks the field.
func (r *Runtime) movedEnvironment(s *sandbox, pod *corev1.Pod, role claim.Role, generation uint64) (moved []movedAddress, dispatchToken bool, err error) {
	raw, ok := s.Annotations[addressesAnnotation(role)]
	if !ok {
		return nil, r.recordedDispatchToken(nil), nil
	}
	var record addressRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, r.recordedDispatchToken(nil), fmt.Errorf("the address record %s is unreadable (%v)", addressesAnnotation(role), err)
	}
	if record.Generation != generation {
		return nil, r.recordedDispatchToken(nil), nil
	}
	for _, a := range r.environmentAddresses(role, r.enrollsIn(&pod.Spec, role)) {
		i := slices.IndexFunc(record.Addresses, func(held recordedAddress) bool { return held.Variable == a.name })
		switch {
		case i < 0:
			moved = append(moved, movedAddress{a.name, "(unrecorded)", r.addressName(a)})
		case record.Addresses[i].SHA256 != addressDigest(a.value):
			moved = append(moved, movedAddress{a.name, record.Addresses[i].Name, r.addressName(a)})
		}
	}
	return moved, r.recordedDispatchToken(&record), nil
}

// movedStream compares the stream the launcher in pod's container named container dials with the
// one a pod created now is handed; ok is whether it moved. A container that is absent or runs no
// connectFlag (never one this runtime built) has nothing to compare. The handed value is compared
// as the pod spec carries it, escaped against the kubelet's expansion (kubeletLiteral).
func (r *Runtime) movedStream(pod *corev1.Pod, container string) (movedAddress, bool) {
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == container })
	if i < 0 {
		return movedAddress{}, false
	}
	command := pod.Spec.Containers[i].Command
	held, ok := flagValue(command, connectFlag)
	if !ok {
		return movedAddress{}, false
	}
	if handed := kubeletEscape(r.streamURL); held != handed {
		return movedAddress{connectFlag, namedURL(held), namedURL(handed)}, true
	}
	return movedAddress{}, false
}

// earlierLayout is whether names, the containers of a pod Legion built, are an issue pod's of the
// layout before one container: exactly one container per workflow role, each named for its role.
// No other code reads that layout: the boot census passes such a Sandbox (legacyIssueSandbox) and
// movedLayout replaces its pod.
func earlierLayout(names []string) bool {
	return len(names) == len(claim.Roles) &&
		!slices.ContainsFunc(claim.Roles, func(role claim.Role) bool { return !slices.Contains(names, string(role)) })
}

// legionContainers are the names among names Legion gives a container: a launcher role's (the
// earlier layout's) or the one container of either pod this runtime builds. The rest were added to
// the running pod by another controller (a mesh's injected sidecar, say), which never writes the
// Sandbox's template, so only a pod's own containers are read through it. The launcher roles here
// are the earlier layout's fixed set, Legion's six and the controller (earlierLayout), never a
// role of the deployment: no role of the deployment ever had a container of that layout.
func legionContainers(names []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		role := claim.Role(name)
		return name != runtime.IssuePodContainer && role != claim.RoleController && !slices.Contains(claim.Roles, role)
	})
}

// movedLayout is a running issue pod of the layout before one container (earlierLayout, read over
// the pod's own containers, legionContainers, since an injected sidecar is in the running pod and
// never in the Sandbox's template the census reads); ok is whether pod is one. Its launchers run
// one role each and read the per-role paths a pod of that layout mounted, so no role of it can run
// as one launched now: every role of it is StaleAddress, and the next launch of any of them
// replaces the pod, an installing launch included.
func movedLayout(pod *corev1.Pod) (movedAddress, bool) {
	names := make([]string, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		names[i] = c.Name
	}
	names = legionContainers(names)
	if !earlierLayout(names) {
		return movedAddress{}, false
	}
	return movedAddress{"the pod's containers", strings.Join(names, ", "), runtime.IssuePodContainer}, true
}

// flagValue is the value command gives flag, the argument that follows it; ok is whether command
// carries the flag with a value at all.
func flagValue(command []string, flag string) (string, bool) {
	i := slices.Index(command, flag)
	if i < 0 || i+1 >= len(command) {
		return "", false
	}
	return command[i+1], true
}

// movedEnrollment compares the agent-secrets volumes a generation of role started now runs against
// (agentSecretsVolumes) with what pod carries; ok is whether pod lacks them. Such a role is started
// with the shim's agent-secrets flags (launcherCommand), which name its key directory and the
// broker's token file, and the shim refuses to start without its token file; a pod's volumes are
// fixed when it is created. So a pod made before the runtime enrolled, or under another audience or
// expiry, cannot run a role that enrolls now, and must be replaced, as a pod whose launcher dials a
// moved stream must. The role's key directory is compared by name, and the token's projection by
// the three fields the shim's token file depends on: its audience, expiry and path. Nothing else of
// a stored volume is compared: a field the API server's defaulting or an admission webhook adds, or
// another projection source beside the token's, changes nothing the shim reads, and reading it as a
// lack would rebuild the pod and re-provision its workspace at every relaunch. A runtime that
// enrolls no pod lacks nothing.
func (r *Runtime) movedEnrollment(pod *corev1.Pod, role claim.Role) (movedAddress, bool) {
	broker := r.agentSecrets
	if broker == nil {
		return movedAddress{}, false
	}
	want, keyDir := brokerTokenProjection(broker), roleVolume(agentSecretsKeyVolume, role)
	var held *corev1.ServiceAccountTokenProjection
	hasKeyDir := false
	for _, volume := range pod.Spec.Volumes {
		switch {
		case volume.Name == agentSecretsTokenVolume && volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if token := source.ServiceAccountToken; token != nil && (held == nil || sameTokenProjection(token, want)) {
					held = token
				}
			}
		case volume.Name == keyDir:
			hasKeyDir = true
		}
	}
	if sameTokenProjection(held, want) && hasKeyDir {
		return movedAddress{}, false
	}
	heldName := enrollmentName(held)
	if held != nil && !hasKeyDir {
		heldName += " without " + keyDir
	}
	return movedAddress{agentSecretsTokenVolume, heldName, enrollmentName(want)}, true
}

// enrollmentName names a broker token projection by the fields movedEnrollment compares, "(unset)"
// for none; none of them is a credential.
func enrollmentName(token *corev1.ServiceAccountTokenProjection) string {
	if token == nil {
		return "(unset)"
	}
	expiry := "(unset)"
	if token.ExpirationSeconds != nil {
		expiry = strconv.FormatInt(*token.ExpirationSeconds, 10) + "s"
	}
	return "audience=" + token.Audience + " expiry=" + expiry + " path=" + token.Path
}

// sameTokenProjection is whether held projects the token want does: the same audience, expiry and
// path.
func sameTokenProjection(held, want *corev1.ServiceAccountTokenProjection) bool {
	return held != nil && held.Audience == want.Audience && held.Path == want.Path &&
		held.ExpirationSeconds != nil && *held.ExpirationSeconds == *want.ExpirationSeconds
}

// movedExchangeIdentity compares the exchange token a pod created now carries
// (exchangeTokenProjection, for AgentSecrets.ExchangeAudience) with what pod carries; ok is whether
// pod lacks it. A role started now in a pod that lacks it is told no identity (tradesIn), and so
// trades nothing for as long as it runs there; a pod's volumes are fixed when it is created, so
// turning the exchange on, or moving its audience, leaves every enrolled pod lacking it until the
// pod is replaced, which is its update round's to do once its workers have pushed, as the
// enrollment's is (movedEnrollment). It compares the three fields the token file depends on, as
// movedEnrollment does. A runtime that names no exchange audience, or enrolls no pod, lacks
// nothing: a pod that still carries a token is no pod to replace for a generation told of none.
func (r *Runtime) movedExchangeIdentity(pod *corev1.Pod) (movedAddress, bool) {
	broker := r.agentSecrets
	if broker == nil || broker.ExchangeAudience == "" {
		return movedAddress{}, false
	}
	want := exchangeTokenProjection(broker)
	held := heldExchangeToken(pod.Spec, want)
	if sameTokenProjection(held, want) {
		return movedAddress{}, false
	}
	return movedAddress{agentSecretsExchangeVolume, enrollmentName(held), enrollmentName(want)}, true
}

// heldExchangeToken is the token pod's exchange volume projects, the one matching want where it
// projects several, nil where it carries no such volume.
func heldExchangeToken(pod corev1.PodSpec, want *corev1.ServiceAccountTokenProjection) *corev1.ServiceAccountTokenProjection {
	var held *corev1.ServiceAccountTokenProjection
	for _, volume := range pod.Volumes {
		if volume.Name != agentSecretsExchangeVolume || volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if token := source.ServiceAccountToken; token != nil && (held == nil || sameTokenProjection(token, want)) {
				held = token
			}
		}
	}
	return held
}

// carriesExchangeIdentity is whether the pod spec carries the exchange token a role is told of for
// broker: the exchange volume projecting a token for broker's exchange audience at
// AgentSecretsExchangeFile, the file AGENT_SECRETS_MACHINE_TOKEN_FILE names. Its lifetime does not
// decide it: a token of another lifetime still trades. It is the pod's half of the exchange
// decision and is read through that decision alone (Runtime.tradesIn).
func carriesExchangeIdentity(pod corev1.PodSpec, broker *AgentSecrets) bool {
	held := heldExchangeToken(pod, exchangeTokenProjection(broker))
	return held != nil && held.Audience == broker.ExchangeAudience && held.Path == AgentSecretsExchangeFile
}

// movedInPod is what pod holds, for role, that a pod created now is not handed and that no worker
// of the pod can be asked about first: the layout before one container (movedLayout), whose pod's
// workers predate the update round's contract and have no op to signal with, and the stream its
// launcher dials (movedStream), which such a launcher never redials, so it never reaches this
// daemon to be tasked at all. Both are fixed for the pod's life, so any moved means the pod must
// be replaced where it stands before role can run as one launched now: evaluate reads a role whose
// pod holds one as StaleAddress, and relaunch replaces a pod that holds one for any of its roles,
// ending every process in it.
//
// Where a round covers the pod (roundCovers), everything else a running pod cannot be changed to
// carry is drift, not a moved address (driftedInPod, podShapeDrift): its worker can be asked to
// do the steps of an update message first (supervise.UpdateTask), and an update round asks it
// before the pod is replaced.
//
// A pod no round covers holds every facet here, driftedInPod's five included. No round is ever
// opened over such a pod — PodDrift answers nothing for it — so a facet that was neither a moved
// address nor a round's would leave its role in the pod it was created in for ever, with nothing
// saying so: today that is the project controller's pod, which has nobody in it to task and no
// issue disk to protect, and so keeps the replacement it has always had, at the next launch into
// it. The rest of such a pod's drift, the shape its template was built from, is its launch's too
// (relaunch, shapeDrift), so a round covers every drift of a pod or a launch covers every drift of
// it, and no drifted pod is left to neither.
func (r *Runtime) movedInPod(pod *corev1.Pod, role claim.Role) []movedAddress {
	var moved []movedAddress
	if layout, ok := movedLayout(pod); ok {
		moved = append(moved, layout)
	}
	if stream, ok := r.movedStream(pod, podContainer(role)); ok {
		moved = append(moved, stream)
	}
	if !roundCovers(podKindFor(role)) {
		moved = append(moved, r.driftedInPod(pod, role)...)
	}
	return moved
}

// driftedInPod is what pod holds, for role, that a pod created now is not handed and whose
// replacement is an update round's business rather than a launch's: the operator's volume and
// mount holding every named role's overlays and the overlays of every arm the project declares
// in role's container
// (movedRoleOverlays), the secrets broker's enrollment its next generation is started against
// (movedEnrollment), the exchange identity it is told of (movedExchangeIdentity), the whole session
// store it was created under (movedSessionStore), and the gh directories its gh volume projects
// (movedGhDirs). Each is fixed for the
// pod's life, so the pod must be replaced for the role to run as one launched now — but each
// leaves the workers already in the pod running and able to reach this daemon, so each is a
// reason to open a round (PodDrift) and ask them to do the steps of its update message
// (supervise.UpdateTask), where replacing the pod under them would end every turn in it and take
// the issue's disk with it. The two facets whose worker cannot be reached at all are movedInPod's
// and are never a round's — and for a pod no round covers (roundCovers), the project controller's
// today, so are these five: no round is opened over such a pod, so movedInPod holds every facet
// there and its launch replaces it, as it always did.
func (r *Runtime) driftedInPod(pod *corev1.Pod, role claim.Role) []movedAddress {
	var moved []movedAddress
	if overlays, ok := r.movedRoleOverlays(pod.Spec, role); ok {
		moved = append(moved, overlays)
	}
	if enrollment, ok := r.movedEnrollment(pod, role); ok {
		moved = append(moved, enrollment)
	}
	if exchange, ok := r.movedExchangeIdentity(pod); ok {
		moved = append(moved, exchange)
	}
	if store, ok := r.movedSessionStore(pod); ok {
		moved = append(moved, store)
	}
	if dirs, ok := r.movedGhDirs(pod, role); ok {
		moved = append(moved, dirs)
	}
	return moved
}

// movedGhDirs is the owner directories a pod created now would project (r.gitHubDirs,
// appauth.GitHubDirs) that role's gh volume of the pod projects none of; ok is whether it lacks
// any. That volume is one Secret projection whose items are fixed when the pod is created
// (volumes), one pair of keys per directory of the launch, while the role Secret those keys come
// from gains a directory's pair the moment the daemon serves it (refreshGitHubCredentials). So a
// Secret that holds a new owner's files reaches no pod made before it: the pod's
// /var/run/legion/gh has no directory of that name, every `gh` call of its roles that would read
// the token there runs unauthenticated, and only a new pod projects it.
//
// What a pod created now would project is what a mint really produces, not what the Apps are
// installed on: an owner whose every repository GitHub refuses a lease for is left out of the
// bundle and recorded as left out (appauth's mintBundle and LeftOut), so appauth.GitHubDirs leaves
// it out of this answer too, and a pod without it is no pod to replace. Were it named here, the
// pod would be replaced at every sweep and its replacement's bundle would lack the directory
// again, for ever (INFRA-513). A mint that answers for it again clears the record, and the pod
// is then replaced once, which is right.
//
// That record is read for the Apps this pod runs and no others: the roles of the kind the role
// belongs to (podKindFor), mapped through appauth.AppRoleFor. An issue pod projects what both
// Apps' bundles hold, so a directory either App is refused is in no projection of it; the
// project controller's pod holds the review App's bundle alone, and an owner the implement
// App's installation does not reach is one its own `gh` reaches all the same, so a controller
// pod without that owner's directory has drifted and is replaced by its next launch.
//
// Owner directories alone are compared, on both sides (ghOwnerDirs). A bundle this daemon
// builds holds none but owner directories: a lease reaches every repository its installation
// holds, so there is nothing narrower for a directory to stand for (appauth.GitHubCredential,
// INFRA-2543). A pod holding a `<owner>/<repo>` directory was launched before that change and
// has drifted on that alone — the names ghOwnerDirs drops are the second reason below, which
// names the directory — since its gh holds a token that reaches one repository where the
// bundle of a pod created now holds its owner's, which reaches every repository of that
// installation.
// An owner's own directory is the first reason: a pod without it holds no token of that owner
// at all.
//
// The bundle's pairs file is the third reason, and the one read by its presence alone
// (GitHubPairsKey, ghconfig.PairsFile): every bundle of this daemon holds it, empty where the
// project has no configured rename that resolves, and `legion gh` and `legion git-credential`
// route a moved repository by it. A pod created before it existed projects no item for it, and
// a projection's items are fixed for the pod's life, so that pod's wrappers would route by
// owner alone for as long as it runs and no Update could mend it. Its content is never
// compared: the kubelet rewrites a projected file in place when the Secret changes, so a pair
// configured since the launch reaches a kept pod (refreshGitHubCredential), and a pod is
// replaced for what a refresh cannot reach and for nothing else.
//
// The pod's own repository is still read from its workspace fetch (fetchRepository), and a pod
// that fetches none — the controller's — asked for with the zero Repository, since the owner
// directories a bundle holds depend on it: among them is the owner the pod writes to, which is
// the role's own and not the project's.
//
// The two pods without a Secret projection are not the same. One with no gh volume at all is no
// pod of this runtime's making and stays unmoved: this runtime does not know what it holds. One
// whose gh volume is an EmptyDir was launched with no directories at all (volumes: a Secret
// projection with no items would project every key of the Secret, the launcher token among
// them), so it holds the empty set — a controller whose project had no owner yet reads every
// owner the daemon serves now as lacking, and the first owner its project gains remakes it.
// The comparison runs one way only: a directory the pod holds that the daemon no longer serves
// costs the pod a stale directory and nothing more, and must never be worth remaking a pod and
// deleting its disk for. The daemon's set comes sorted (Options.GitHubDirs), and the pod's is
// sorted here, so each is searched and named in one order.
func (r *Runtime) movedGhDirs(pod *corev1.Pod, role claim.Role) (movedAddress, bool) {
	name := roleVolume(ghVolume, role)
	projected, found := ghProjectionOf(pod, role)
	if !found {
		return movedAddress{}, false
	}
	held := projected.dirs
	slices.Sort(held)
	owned := ghOwnerDirs(held)
	// A `<owner>/<repo>` directory is no bundle of this daemon's: the pod was launched by one
	// that still cut a directory per repository, and its token reaches that repository where
	// the owner's own directory of a bundle built now reaches every repository of the
	// installation. The names ghOwnerDirs drops are exactly those.
	if repositories := slices.DeleteFunc(slices.Clone(held), func(dir string) bool { return !strings.Contains(dir, "/") }); len(repositories) > 0 {
		return movedAddress{where: name + " holds a repository directory", held: ghDirsName(repositories),
			handed: "the owner directories alone"}, true
	}
	// A pod launched with no directory at all projects no Secret and so no pairs file either,
	// and a launch now would hand it the same empty directory: it is compared for its owner
	// directories alone, below.
	if projected.secret && !projected.pairs {
		return movedAddress{where: name + " holds no " + ghconfig.PairsFile, held: ghDirsName(owned),
			handed: "the same directories beside the configured renames the wrappers route by"}, true
	}
	repo, _ := fetchRepository(pod)
	apps, err := podAppRoles(r, podKindFor(role))
	if err != nil {
		// A pod holding a role that acts as no App cannot be told what it lacks: it is read as
		// moved, naming why, rather than kept on a set of directories nobody can compute.
		return movedAddress{where: name, held: ghDirsName(held), handed: err.Error()}, true
	}
	served := ghOwnerDirs(r.gitHubDirs(repo, apps))
	lacking := slices.ContainsFunc(served, func(dir string) bool {
		_, found := slices.BinarySearch(owned, dir)
		return !found
	})
	if !lacking {
		return movedAddress{}, false
	}
	return movedAddress{where: name, held: ghDirsName(owned), handed: ghDirsName(served)}, true
}

// ghProjection is what one role's gh volume of a pod projects, read off its items, which are
// fixed for the pod's life: dirs, the bundle directories they name (GitHubHostsDir), in the order
// they come, pairs, whether they hold the bundle's pairs file (GitHubPairsKey), and secret,
// whether the volume is a Secret projection at all — not one of a pod launched with no directory,
// whose gh volume is an empty directory projecting nothing. The drift check reads it (movedGhDirs)
// and so does the refresher, which serves a pod the layout it names (servedLayout).
type ghProjection struct {
	dirs          []string
	pairs, secret bool
}

// ghProjectionOf is what role's gh volume of pod projects, and whether pod has such a volume at
// all: one with none is no pod of this runtime's making. It reads the volume, never its mount, so
// it reads the controller's pod in either of its layouts: the single-role form mounts the volume
// at GHConfigDir and the roles-dir form under the controller's directory of RolesDir.
func ghProjectionOf(pod *corev1.Pod, role claim.Role) (ghProjection, bool) {
	name := roleVolume(ghVolume, role)
	at := slices.IndexFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
	if at < 0 {
		return ghProjection{}, false
	}
	source := pod.Spec.Volumes[at].Secret
	if source == nil {
		return ghProjection{}, true
	}
	projected := ghProjection{secret: true}
	for _, item := range source.Items {
		if item.Key == GitHubPairsKey {
			projected.pairs = true
		}
		if dir, ok := GitHubHostsDir(item.Key); ok {
			projected.dirs = append(projected.dirs, dir)
		}
	}
	return projected, true
}

// podAppRoles are the GitHub Apps a pod of kind holds a bundle of under r, each named once
// (appauth.AppRoleFor over the runtime's deploymentApps, its kind's roles under r): both of them
// for an issue pod, whose roles run as one or the other, and the review App alone for the project
// controller's. It is what the record of the directories a mint found GitHub refuses is read for
// (movedGhDirs, appauth.LeftOut). A role of the pod that acts as no App is AppRoleFor's error.
func podAppRoles(r *Runtime, kind podKind) ([]appauth.AppRole, error) {
	apps := make([]appauth.AppRole, 0, len(appauth.Roles))
	for _, role := range kind.roles(r) {
		app, err := appauth.AppRoleFor(role, r.deploymentApps)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	return apps, nil
}

// PodAppRoles is podAppRoles of the pod role runs in (podKindFor) under a deployment that defines
// deployed: both Apps for a workflow role, whose issue pod projects the directories every role's
// bundle holds and no more (gitHubBundles, volumes), and the review App alone for the project
// controller; an issue pod's roles are Legion's and each role of deployed (issueRolesOf), each
// acting as its App. A role's container holds only the directories of that projection, so a
// caller that says which owners a role's token reaches in a pod of this runtime reads them for
// these Apps together (internal/daemon's plan.appOwners), as the drift check does (movedGhDirs).
// A role of the pod that acts as no App is AppRoleFor's error.
func PodAppRoles(role claim.Role, deployed appauth.DeploymentApps) ([]appauth.AppRole, error) {
	return podAppRoles(&Runtime{issueRoles: issueRolesOf(deployed), deploymentApps: deployed}, podKindFor(role))
}

// ghOwnerDirs is the owner directories of a gh directory set, in the order they come: the names
// holding no slash, where a repository's directory a pod launched before INFRA-2543 projects is
// `<owner>/<repo>`. It is what movedGhDirs compares, a repository directory a pod lacks being no
// address of its own.
func ghOwnerDirs(dirs []string) []string {
	owners := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !strings.Contains(dir, "/") {
			owners = append(owners, dir)
		}
	}
	return owners
}

// ghDirsName names a set of gh directories as one string, each name in order; "(unset)" for none,
// and no credential in any of it.
func ghDirsName(dirs []string) string {
	if len(dirs) == 0 {
		return "(unset)"
	}
	return strings.Join(dirs, " ")
}

// movedRoleOverlays is the first overlay of any role role_config_files names
// (Options.RoleConfigFiles) that runs in role's container of the pod spec — every issue role
// shares an issue pod's one container, the controller has its own pod — or of any arm of any
// experiment the project declares (Options.ExperimentVariants), which every pod of the
// deployment is read for, since any unit may be drawn into any arm, whose holder in a pod created
// now, the deepest
// operator mount at or above the file (runtime.kubernetes.pod volume_mounts) with its volume, is
// not the deepest mount at or above the file in that container as a role sees it (an issue pod's
// HomeMountsDir view mapped back to the image's home): another volume, sub_path or mount path, a
// mount that hides it there or none at all, or a volume of that name filled from another ConfigMap
// or Secret, or projecting other items (movedOverlay). Every role of the pod reports the same
// move, as for the secrets broker's enrollment, so the whole pod is replaced at the boot's
// re-adoption of any role running in it, or, with none running, at the first launch into it; a
// named role's next generation is started with the file in its PI_CONFIG_FILES (roleOperatorEnv),
// on which its Oh My Pi would refuse to start in the old pod — unless the launch keeps that pod,
// which starts the role with the overlays the pod does hold (heldRoleOverlays).
//
// It is asked of a running pod's spec and of a Sandbox's stored pod template alike: the template
// is the pod's own build, which is what a launch that keeps the pod reads.
func (r *Runtime) movedRoleOverlays(pod corev1.PodSpec, role claim.Role) (movedAddress, bool) {
	if len(r.roleConfigFiles) == 0 && len(r.variantFiles) == 0 {
		return movedAddress{}, false
	}
	seen, ok := roleMounts(pod, role)
	if !ok {
		return movedAddress{}, false
	}
	for _, named := range r.roleConfigRoles() {
		if podContainer(named) != podContainer(role) {
			continue
		}
		for _, file := range r.roleConfigFiles[named] {
			if held, want, moved := r.movedOverlay(seen, pod.Volumes, file); moved {
				return movedAddress{where: "role_config_files." + string(named) + " " + file, held: held, handed: want}, true
			}
		}
	}
	// The controller runs in no tree, so it is in no experiment and no arm ever names it these
	// files; a pod of its own that lacks them is no drift of its: its own overlays alone are
	// read above.
	if role != claim.RoleController {
		for _, variant := range r.experimentVariants {
			for _, file := range variant.ConfigFiles {
				if held, want, moved := r.movedOverlay(seen, pod.Volumes, file); moved {
					// The key the operator wrote these overlays under, carried on the arm
					// (runtime.Variant.ConfigFilesField): the experiment's own
					// `…experiments.<name>.variants.<arm>.config_files` for an arm of the
					// `experiments` key, so a drift names a key an operator can find in the
					// file. The `memory` block's own arm (runtime.Variant.MemoryBlock) is named
					// as it was before any experiment was declared, memoryOverlayField.
					field := variant.ConfigFilesField
					if variant.MemoryBlock {
						field = memoryOverlayField
					}
					return movedAddress{where: field + " " + file, held: held, handed: want}, true
				}
			}
		}
	}
	return movedAddress{}, false
}

// memoryOverlayField is the project's `memory.config_files`, as a drift of one of the `memory`
// block's own overlays names it (movedRoleOverlays): the key an operator reads in the daemon's
// configuration, which names no project, since a daemon runs one.
const memoryOverlayField = "memory.config_files"

// heldRoleOverlays are the overlays role_config_files names for role (Options.RoleConfigFiles) and
// the overlays of every arm the project declares (Options.ExperimentVariants), each in that
// order, that the pod
// spec's own container for role holds: each file the pod carries where a pod created now would
// (movedOverlay), none of the ones it was built too early to carry. ok is false when the spec has
// no container for role at all, where nothing can be said of what the pod holds.
//
// It is what a launch that keeps a live pod builds that role's PI_CONFIG_FILES from, read from the
// pod's stored template (relaunch's keptBy, roleOperatorEnv): Oh My Pi refuses to start on an
// entry that is not there, so a role started in a drifted pod against the configuration as it now
// reads would never register and never answer the round that is to replace that pod. A pod built
// before an arm's overlays were mounted holds none of them, and a role of a unit recorded in that
// arm is told none of them — it runs in neither arm until the round replaces the pod, which
// launch.overlays logs.
func (r *Runtime) heldRoleOverlays(pod corev1.PodSpec, role claim.Role) (own, variants []string, ok bool) {
	seen, ok := roleMounts(pod, role)
	if !ok {
		return nil, nil, false
	}
	held := func(files []string) []string {
		var kept []string
		for _, file := range files {
			if _, _, moved := r.movedOverlay(seen, pod.Volumes, file); !moved {
				kept = append(kept, file)
			}
		}
		return kept
	}
	return held(r.roleConfigFiles[role]), held(r.variantFiles), true
}

// carriesEnrollment is whether the pod spec carries the secrets broker's enrollment for role: the
// projected token volume, which holds the broker's audience token, and role's own key volume,
// which its agent-secrets client keeps its key in (agentSecretsVolumes). They are the two the
// shim's agent-secrets flags name, and it stats both and exits 1 when either is absent
// (launcherCommand, shim.ReadAgentSecretsFlags), so a launch that keeps a pod carrying neither —
// one created before the runtime enrolled — starts its role without those flags, and without the
// two variables that would tell it it holds a broker. A pod's volumes are fixed when it is
// created; the update round replaces such a pod. It reads the volumes alone, never where they are
// mounted, so it reads the controller's pod in either of its layouts, which carry the same two
// (controllerPod.launcherContainer).
//
// It is the pod's half of the one enrollment decision, and is read through that decision alone
// (Runtime.enrollsIn), never on its own: a caller that read it beside r.agentSecrets would be
// free to disagree with the generation's argv, which is the split INFRA-484 closed.
func carriesEnrollment(pod corev1.PodSpec, role claim.Role) bool {
	token, key := false, false
	for _, volume := range pod.Volumes {
		switch volume.Name {
		case agentSecretsTokenVolume:
			token = volume.Projected != nil
		case roleVolume(agentSecretsKeyVolume, role):
			key = true
		}
	}
	return token && key
}

// roleMounts are the mounts of the pod spec's container for role as a role sees them
// (roleViewPath): every issue role shares an issue pod's one container, the controller has its
// own pod. It reads the controller's container in either of its layouts: the single-role form
// mounts what belongs under the home at its own path, and the roles-dir form under HomeMountsDir
// (controllerPod.launcherContainer), which roleViewPath maps back. ok is false when the spec holds
// no such container — a template of another kind of pod, or one this daemon cannot read at all.
func roleMounts(pod corev1.PodSpec, role claim.Role) ([]corev1.VolumeMount, bool) {
	container := slices.IndexFunc(pod.Containers, func(c corev1.Container) bool { return c.Name == podContainer(role) })
	if container < 0 {
		return nil, false
	}
	seen := make([]corev1.VolumeMount, len(pod.Containers[container].VolumeMounts))
	for i, m := range pod.Containers[container].VolumeMounts {
		m.MountPath = roleViewPath(m.MountPath)
		seen[i] = m
	}
	return seen, true
}

// movedOverlay compares how a container whose mounts are seen, in a pod whose volumes are volumes,
// holds the overlay file with how a pod created now would: the file's holder is the deepest
// operator mount at or above it (holderMount over runtime.kubernetes.pod volume_mounts), and the
// volume that mount names must be filled from the same source (volumeFillName). held is what that
// pod fills it from, "(unset)" where the container does not hold the file under that mount at all,
// and want what a pod created now is handed. Only what decides whether the file is there is
// compared, so the fields the API server defaults on a stored volume never read as a move. A file
// no operator mount holds is never moved: the daemon has no mount to compare it by, and a
// configuration naming one is refused before the runtime is built (config's checkRoleConfigMount).
func (r *Runtime) movedOverlay(seen []corev1.VolumeMount, volumes []corev1.Volume, file string) (held, want string, moved bool) {
	holder := holderMount(r.pod.VolumeMounts, file)
	if holder == nil {
		return "", "", false
	}
	want = volumeFillName(r.pod.Volumes, holder.Name)
	held = "(unset)"
	if in := holderMount(seen, file); in != nil && in.Name == holder.Name && in.SubPath == holder.SubPath && in.MountPath == holder.MountPath {
		held = volumeFillName(volumes, holder.Name)
	}
	return held, want, held != want
}

// roleViewPath is path, a mount path of a pod's container, as a role sees it: the HomeMountsDir
// view of the image's home (containerHomePath) of a container that runs the roles-dir form, an
// issue pod's or the controller's under a writer audience, mapped back to the home, and any other
// path as it is. The controller's single-role container mounts nothing under HomeMountsDir, so
// its paths are the role's view already.
func roleViewPath(path string) string {
	if path == HomeMountsDir {
		return podHome
	}
	if rest, ok := strings.CutPrefix(path, HomeMountsDir+"/"); ok {
		return podHome + "/" + rest
	}
	return path
}

// holderMount is the deepest of mounts at or above file, as the kernel resolves the path, nil for
// none (config's checkRoleConfigMount refuses a role overlay no mount holds).
func holderMount(mounts []corev1.VolumeMount, file string) *corev1.VolumeMount {
	var holder *corev1.VolumeMount
	for i, mount := range mounts {
		if file != mount.MountPath && !strings.HasPrefix(file, mount.MountPath+"/") {
			continue
		}
		if holder == nil || len(mount.MountPath) > len(holder.MountPath) {
			holder = &mounts[i]
		}
	}
	return holder
}

// volumeFillName names what the volume called name among volumes is filled from, by the fields
// that decide which files it holds: its ConfigMap or Secret and their items, a projected volume's
// sources, its claim or NFS export; "(unset)" for no such volume. None of them is a credential.
func volumeFillName(volumes []corev1.Volume, name string) string {
	at := slices.IndexFunc(volumes, func(v corev1.Volume) bool { return v.Name == name })
	if at < 0 {
		return "(unset)"
	}
	items := func(list []corev1.KeyToPath) string {
		var kept []string
		for _, item := range list {
			kept = append(kept, item.Key+"="+item.Path)
		}
		return "[" + strings.Join(kept, ",") + "]"
	}
	v := volumes[at]
	switch {
	case v.ConfigMap != nil:
		return "volume " + name + " from ConfigMap " + v.ConfigMap.Name + " items " + items(v.ConfigMap.Items)
	case v.Secret != nil:
		return "volume " + name + " from Secret " + v.Secret.SecretName + " items " + items(v.Secret.Items)
	case v.Projected != nil:
		var sources []string
		for _, source := range v.Projected.Sources {
			switch {
			case source.ConfigMap != nil:
				sources = append(sources, "ConfigMap "+source.ConfigMap.Name+" items "+items(source.ConfigMap.Items))
			case source.Secret != nil:
				sources = append(sources, "Secret "+source.Secret.Name+" items "+items(source.Secret.Items))
			}
		}
		return "volume " + name + " projecting " + strings.Join(sources, " and ")
	case v.PersistentVolumeClaim != nil:
		return "volume " + name + " from claim " + v.PersistentVolumeClaim.ClaimName
	case v.NFS != nil:
		return "volume " + name + " from NFS " + v.NFS.Server + ":" + v.NFS.Path
	}
	return "volume " + name
}

// movedSessionStore compares the whole session store pod carries with the one a pod created now
// would carry, so no pod outlives a change of store; ok is whether they differ. A generation
// started under a database store names a URL file no pod created under another store has: the
// providers volume's projection of the Secret key under postgres, the proxy the pod's launcher
// serves under cloudsql-iam. A pod created under another store or key is drift (driftedInPod): an
// issue pod is replaced by its update round, and a launch into it before that keeps the pod and
// tells its generation the store the pod itself carries (keptPod), so no generation starts against
// a URL file its Oh My Pi cannot read; the controller's pod, which no round covers (roundCovers),
// is replaced at the next launch into it. Under
// cloudsql-iam the instance, the database, the login, the service account the launcher
// impersonates and the writer audience it impersonates that account with are all compared — an
// account or an audience named where the pod has none, none where it has one, or another one —
// since the pod's launcher was told them once, at its creation, and signs in to Cloud SQL as that
// identity, with that credential, for the pod's life. A writer audience added or dropped changes
// the controller's pod's layout too (controllerPod.launcherContainer), and this is what replaces
// it. Each side is named from what a pod spec holds, the kubelet escaping included
// (kubeletLiteral), as movedStream names the stream.
func (r *Runtime) movedSessionStore(pod *corev1.Pod) (movedAddress, bool) {
	proxy := r.sessionProxyFlags()
	for i, value := range proxy {
		proxy[i] = kubeletEscape(value)
	}
	held := sessionStoreInPod(&pod.Spec)
	handed := sessionStoreName(r.sessionStore.Kind, r.sessionStore.DSNSecretKey(), proxy)
	if held == handed {
		return movedAddress{}, false
	}
	return movedAddress{where: "the session store", held: held, handed: handed}, true
}

// sessionStoreInPod names the session store a pod specification was created under, read back from
// the specification itself (heldSessionStore, sessionStoreName). The Sandbox pod template and the
// created Pod carry the same markers.
func sessionStoreInPod(spec *corev1.PodSpec) string {
	return sessionStoreName(heldSessionStore(*spec))
}

// heldSessionStore is the session store the pod spec was created under, and the one field that
// kind carries in a pod: the providers volume's session item under postgres (providers), the
// session proxy's flags a launcher container carries under cloudsql-iam (sessionProxyFlagsIn), and
// neither under the volume store, which is what a pod created before any store was configured
// reads as. Under cloudsql-iam the kind comes from the three required flags alone, so a pod whose
// launcher impersonates a service account, with its own credentials or with the writer token, is a
// cloudsql-iam pod as one that signs in as itself is; the flags it answers name the account and
// the writer audience too where the pod carries them, so each still names a store of its own
// (sessionStoreName).
//
// It is asked of a running pod's spec and of a Sandbox's stored pod template alike, as
// movedRoleOverlays is: the template is the pod's own build, which is what a launch that keeps
// the pod reads to tell its role the URL file that pod can actually give it (keptBy, keptPod).
func heldSessionStore(pod corev1.PodSpec) (ompsessions.StoreKind, string, []string) {
	for _, volume := range pod.Volumes {
		if volume.Name != providersVolume || volume.Secret == nil {
			continue
		}
		for _, item := range volume.Secret.Items {
			if item.Path == ompsessions.DSNFileName {
				return ompsessions.StorePostgres, item.Key, nil
			}
		}
	}
	for _, container := range pod.Containers {
		if proxy := sessionProxyFlagsIn(container.Command); proxy != nil {
			return ompsessions.StoreCloudSQLIAM, "", proxy
		}
	}
	return ompsessions.StoreVolume, "", nil
}

// sessionProxyFlagsIn are the session proxy's flags command carries, in the order sessionProxyFlags
// writes them, nil unless it carries all three of sessionProxyFlagNames with a value each: a pod
// created under any other store, and the containers of every pod that run no launcher, carry none.
// The service account the launcher impersonates (sessionProxyAccountFlag) follows the three when
// command carries it with a value, and then the writer audience it impersonates that account with
// (sessionWriterAudienceFlag) when command carries that; each is left out when command does not
// carry it. Both are optional, so neither decides the kind, and a command that carries them
// without the three is no cloudsql-iam launcher.
func sessionProxyFlagsIn(command []string) []string {
	proxy := make([]string, 0, 2*len(sessionProxyFlagNames)+4)
	for _, flag := range sessionProxyFlagNames {
		value, ok := flagValue(command, flag)
		if !ok {
			return nil
		}
		proxy = append(proxy, flag, value)
	}
	for _, flag := range [...]string{sessionProxyAccountFlag, sessionWriterAudienceFlag} {
		if value, ok := flagValue(command, flag); ok {
			proxy = append(proxy, flag, value)
		}
	}
	return proxy
}

// sessionStoreName names a session store by everything a pod carries of it, which is all that
// decides a move: the Secret key under postgres, the proxy's flags under cloudsql-iam, and the
// kind alone under the volume store, which an unset store is. None of them is a credential — the
// postgres store's URL is the Secret's value, never its key.
func sessionStoreName(kind ompsessions.StoreKind, dsnSecret string, proxy []string) string {
	switch kind {
	case ompsessions.StorePostgres:
		return string(kind) + " key=" + dsnSecret
	case ompsessions.StoreCloudSQLIAM:
		return string(kind) + " " + strings.Join(proxy, " ")
	default:
		return string(ompsessions.StoreVolume)
	}
}

// namedURL is the log boundary for one endpoint value: an existing pod or record can hold a value a
// previous daemon accepted, or a direct Options caller supplied, even when this daemon's current
// loader no longer accepts it. It names "(unset)" for none; otherwise it constructs the name from
// exactly the scheme, host and port it accepts to show, adding xxxxx@ when the URL carries userinfo.
// Path, query and fragment never enter it. An endpoint that cannot yield a scheme and host is xxxxx
// whole, so unexpected input cannot make the name a credential reader. api.endpointOf is the same
// mask as the state renders it, which keeps the path: there the value is a setting its reader acts
// on, where this one is only a name.
func namedURL(address string) string {
	if address == "" {
		return "(unset)"
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "xxxxx"
	}
	name := u.Scheme + "://"
	if u.User != nil {
		name += "xxxxx@"
	}
	return name + u.Host
}

// namedNATSURLs names ENVOY_NATS_URL from the distinct values r.natsURLs holds, the entries
// readNatsURLs accepted, before mainEnvironment comma-joined them: each is one endpoint, so namedURL
// parses it whole. The joined value is never split, since raw commas are valid in userinfo.
func namedNATSURLs(urls []string) string {
	if len(urls) == 0 {
		return "(unset)"
	}
	named := make([]string, len(urls))
	for i, raw := range urls {
		named[i] = namedURL(raw)
	}
	return strings.Join(named, ",")
}
