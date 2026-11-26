package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// gitHubRefreshInterval is how often refreshGitHubCredentials walks the pod store. The daemon's
// function (Options.GitHubCredential) holds each App's lease and re-mints it as the lease nears its
// expiry, so most ticks render the hosts.yml the Secret already holds and write nothing; a minute
// keeps the time an agent's gh runs on a token the daemon has already replaced short against an
// installation token's hour, at one Secret read per live slot. A runtime reads it once, as it starts
// (start); a test that must see a tick inside its run shortens it before starting one.
var gitHubRefreshInterval = time.Minute

// refreshGitHubCredentialsEvery runs refreshGitHubCredentials, then refreshCacheTokens, then
// readCacheHealth, every interval until ctx ends: the informers' lifetime (start), so the walk never
// reads a store nothing feeds.
func (r *Runtime) refreshGitHubCredentialsEvery(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.refreshGitHubCredentials(ctx)
			r.refreshCacheTokens(ctx)
			r.readCacheHealth(ctx)
		}
	}
}

// refreshGitHubCredentials brings the slot Secrets of every live pod to the current render of the
// GitHub credential each slot holds — the App of the role its process was launched as, which that
// launch recorded on the Secret, else of the stored claim's role, and its own name's App where no
// stored claim runs in it and that name is a role's (slotAppRole) — so the gh files its container
// projects (GHConfigDir) carry the tokens the daemon holds now: the kubelet rewrites the projection
// in place once the Secret changes.
// It walks the pod store for the pods of this project (every launcher slot holds
// an App's gh bundle, the controller's included; a repository cache's pod names no kind and holds
// none, so it is passed over, its token refreshCacheTokens') that are Pending or Running and not
// being deleted —
// a pod waiting to be scheduled starts its launchers from the Secret as it is then — and, for each
// slot the pod was built with, reads the slot's Secret, skips one another Sandbox owns (the pod's
// Sandbox replaced since the pod was listed), renders the credential for the pod's own repository, and
// rewrites only the directories whose hosts.yml is not the render. A Secret that records no
// default (one an older runtime wrote, whose pod the next launch replaces),
// a render that fails, or one that holds no directory for the default the Secret records is
// logged and the slot keeps its last tokens until the next tick, and a Secret whose default is
// empty (a controller whose project
// had no GitHub owner when its pod started, whose gh directory is an empty one) has nothing to
// refresh until its next pod; a write that conflicts is retried once on a fresh read. Nothing
// of the Secret but the rewritten directories' gh keys changes: the launcher token, whose hash
// binds the launcher (credentialFromSecret), the default, the annotations, and the keys of a
// directory the render no longer holds, which the pod's projection may still name, stay as they
// are, so no key a running pod projects is ever deleted. Every live issue pod's provisioning
// Secret is brought to the implement App's current lease for the pod's own repository too
// (refreshProvisioningToken), whatever its
// workspace-fetch has already done: the kubelet runs a pod's init containers afresh whenever it
// rebuilds the pod's sandbox, at any point in the pod's life (a relaunch that replaces the pod
// provisions it anew instead), and the fetch re-reads the token's file before every transfer, so
// a pod left on its boot token, aged past the hour GitHub gives an installation token, could not
// fetch its repository again. The lease is minted once per repository a tick, however many of
// that repository's pods the walk holds, and a repository whose mint fails is logged once and
// left to the next tick, where its pods are tried again. The gh bundles are built once per App
// role and repository of the tick in the same way (appauth.Bundles), so a tick over an issue pod
// reads its Apps' installations and not one per slot.
func (r *Runtime) refreshGitHubCredentials(ctx context.Context) {
	// mints memoizes this tick's provisioning leases by lower-cased repository — nothing here
	// knows of a configured repository rename, which the daemon's adapter maps, so Acme/legion
	// and acme/legion are one call — and records a mint that failed as well as one that worked,
	// so a repository is asked at most once a tick whatever the answer. A lease names the one
	// repository its pod clones, so two repositories of one owner are two mints. The walk is one
	// goroutine, so the map needs no lock.
	type mint struct {
		token string
		ok    bool
	}
	bundles := appauth.NewBundles(r.gitHubCredential)
	mints := map[string]mint{}
	provisioning := func(repo ghrepo.Repository) (string, bool) {
		key := strings.ToLower(repo.String())
		if m, asked := mints[key]; asked {
			return m.token, m.ok
		}
		minting, cancel := call(ctx)
		token, err := r.tokens.Token(minting, repo)
		cancel()
		if err != nil {
			r.log.Warn("sandbox runtime: provisioning token mint failed", "repo", repo.String(), "err", err)
		}
		mints[key] = mint{token: token, ok: err == nil}
		return token, err == nil
	}
	// Each slot holds the App of the role its process was launched as, which that launch recorded
	// on the slot's Secret, else of the step its claim does now, read from the stored claims once a
	// tick (slotAppRole); the role is judged on the Secret each write reads (refreshGitHubCredential).
	// A tick that cannot read the stored claims refreshes no slot of an issue pod, each of which
	// keeps its last tokens until the next tick, rather than mint a slot for its name.
	held, heldErr := r.slotHolders(ctx)
	if heldErr != nil {
		r.log.Warn("sandbox runtime: github credential refresh cannot read the stored claims; issue pods keep their tokens until the next tick", "err", heldErr)
	}
	for _, obj := range r.pods.GetStore().List() {
		pod := obj.(*corev1.Pod)
		kind, err := podKindOf(pod.Labels)
		if err != nil {
			continue
		}
		if phase := pod.Status.Phase; (phase != corev1.PodPending && phase != corev1.PodRunning) || pod.DeletionTimestamp != nil {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			continue
		}
		// The pod's own repository is read once here, for every role of it and for its
		// provisioning token: it is the pod's, not the role's, and a bundle is rendered for it
		// (refreshGitHubCredential). The roles are the ones the pod was built with (builtRoles), and
		// of those a slot in which no stored claim runs is refreshed only where its role is still
		// configured: a newly added role has no Secret in an old pod, and a dropped role's GitHub
		// lease should expire without another mint. A slot of an agent of the plan whose name is no
		// role of the runtime, and in which no stored claim runs yet, names no App to render it for
		// (slotAppRole refuses it), and runs nothing that reads it: its claim's first launch writes
		// it for that claim's role before its child starts (writeLaunchCredential). The walk passes
		// over such a slot here, which saves the warning a refused refresh of it would log at every
		// tick. A slot in which a stored claim runs is refreshed whatever its name.
		repo, named := fetchRepository(pod)
		configuredRoles := kind.roles(r)
		for _, role := range builtRoles(pod.Spec) {
			if heldErr != nil && role != claim.RoleController {
				continue
			}
			// The pod's launcher of each role serves the slot of that role's name.
			slot := claim.Agent(role)
			if _, runs := held[slotKey{sandbox: pod.Name, slot: slot}]; !runs &&
				(!slices.Contains(configuredRoles, role) || !slices.Contains(r.launcherRoles(), role)) {
				continue
			}
			r.refreshGitHubCredential(ctx, pod, owner.UID, slot, held, repo, bundles)
		}
		if _, issue := kind.(issuePod); !issue || !named {
			continue
		}
		token, ok := provisioning(repo)
		if !ok {
			continue
		}
		r.refreshProvisioningToken(ctx, pod, owner.UID, repo, token)
	}
}

// refreshProvisioningToken writes token — the implement App's current lease for repo, the
// repository the fetch of pod names (fetchRepository), which the walk minted once for that
// repository this tick — into the provisioning Secret of pod, owned by the Sandbox with uid.
// The fetch reads the token's file anew before every transfer from GitHub (workspace.Fetch),
// and the kubelet rewrites the projected file within its sync period, so a fetch that starts
// long after the pod did, one the kubelet runs again after it rebuilds the pod's sandbox
// included, never presents an expired token. Only the token is written; the cache's token
// beside it stays. A Secret another Sandbox owns, or one that already holds the lease, is left;
// a write that fails is logged and tried at the next tick, and a write that conflicts is
// retried once on a fresh read.
func (r *Runtime) refreshProvisioningToken(ctx context.Context, pod *corev1.Pod, uid types.UID, repo ghrepo.Repository, token string) {
	name := secretName(pod.Name)
	failed := func(err error) {
		r.log.Warn("sandbox runtime: provisioning token refresh failed", "sandbox", pod.Name, "repo", repo.String(), "err", err)
	}
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for retried := false; ; retried = true {
		secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			failed(err)
			return
		}
		if !ownedBySandbox(secret.OwnerReferences, uid) || string(secret.Data[provisionTokenKey]) == token {
			return
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[provisionTokenKey] = []byte(token)
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err == nil {
			r.log.Info("sandbox runtime: provisioning token refreshed", "sandbox", pod.Name, "repo", repo.String())
			return
		}
		if !apierrors.IsConflict(err) || retried {
			failed(err)
			return
		}
	}
}

// refreshGitHubCredential is one slot's refresh (refreshGitHubCredentials): the slot's Secret in
// the pod, owned by the Sandbox with uid, minted for the role whose App the slot holds, and
// rendered for repo, the pod's own repository its caller read off its workspace fetch once for
// every slot of the pod.
//
// That role is judged on the Secret the write reads, under its resourceVersion (slotAppRole):
// the role the slot's process was launched as, which the launch's own write of the Secret
// recorded (launchedRoleAnnotation), and, on a Secret no launch into this pod wrote, the stored
// role the tick read of the claim in the slot, held. A launch of a re-roled claim into the pod
// writes the slot for its new role's App at once and waits for that token (writeLaunchCredential),
// so a refresh that read the stored claims before the re-role must not write the old role's App
// over it: the launch's write lands before this one reads the Secret, and records the role, or
// between its read and its write, and the write is refused as a conflict and judged again on a
// fresh read (writeSlotCredential).
//
// A refresh writes nothing into a slot while a launch into the pod is in flight in it, from just
// before the launch writes the slot to the end of its start (markSlotLaunch, refreshUpdate): the
// start names the digest of the token the launch left in the slot, which the launcher waits for
// the files to hold, and a newer token of the same App written over it would leave them never
// holding it. The refresher's next tick writes the slot as usual.
//
// A workflow role's credential is rendered for that repository, and not for the Secret's
// recorded default, which names an owner and so could never name a repository: the repository
// is what the bundle's default is routed by (ghconfig.RouteDir). A pod the previous daemon
// created carries no repository in its Secret and needs none, since its fetch argument names it,
// so its refresh keeps working and no live issue is stopped. An owner directory the pod lacks is
// what opens an update round (movedGhDirs), and this refresh writes the tokens of the directories
// the pod does project (servedLayout), whatever else a bundle built now would hold.
//
// The controller's is fetched by owner instead (Options.ControllerCredential), the one its
// Secret records: it works no repository, so a bundle asked for by repository would take its
// default from the first owner the project serves, sorted, which a project that gained an owner
// sorting before the recorded one moves away from the directory the controller's GH_CONFIG_DIR
// names. Asked for by the recorded owner, the bundle's default is that directory, the one a
// mint may not leave out (appauth's mintBundle), so a lease GitHub refuses for it fails the
// refresh naming the owner and the reason. Its launch asks for no owner, so the default the
// Secret records is the launch's own.
//
// The Secret's recorded default names the directory the pod's GH_CONFIG_DIR was fixed at, and
// the render must hold that directory: one that does not is refused rather than written, since
// the slot would go on reading a directory nothing refreshes. The render's own default is not
// compared with it. A workflow role's default is the directory its repository routes to by the
// pairs that resolve now (appauth's bundleDirs), and a pair starts or stops resolving when the
// daemon starts or stops serving one of its two names (GitHub's answer of whether both role Apps
// are installed on it), so a change of that answer moves the default of every bundle built after
// it: once GitHub no longer answers that both Apps are installed on the old name, the default
// moves from the `from` owner's directory to the `to` owner's. The directories a bundle holds do
// not move with it and the pod projects the pairs file, so nothing opens an update round
// (movedGhDirs), and a refresh that refused the moved default would stop for the rest of the pod's
// life, every token in it expiring within the hour.
//
// What the pod keeps by this is routing by repository: `legion gh` and `legion git-credential`
// route each request by the repository it names, or by the one the working directory's git
// remotes name, over the pairs this refresh writes beside the leases. What it does not gain is
// the new default. A pod launched before that change keeps the directory its launch
// recorded as its GH_CONFIG_DIR for as long as it lives, a later launch into the same pod
// included, since that launch takes the default from this Secret (launcherBound), and only the
// pod a launch replaces it with is fixed at the directory the repository routes to now. So a
// command that names no repository and runs outside the worktree, where no git remote names
// one, is answered from the old default's directory (cmd/legion's runGh), with the lease that
// directory holds: the old owner's own where the App is installed on that owner, which does not
// reach the repository the pair moved.
//
// What it writes is the layout the pod's own gh projection can read (servedLayout), which a pod
// is served until its update round replaces it: the projection is fixed for the pod's life, and
// a layout the pod cannot read would cost it more than any drift does. A pod projecting the
// pairs file reads the bundle as it is built now. One projecting none was launched before the
// bundle carried one, and its wrappers route by owner alone, so it is served the layout it was
// launched with: each configured pair's `from` owner's directory holding the `to` owner's lease.
// Its Secret records the repository's own owner, whose directory that layout holds, so it is
// refreshed rather than refused, and neither loses nor gains what it reaches. A
// pod projecting a `<owner>/<repo>` directory, which a daemon before INFRA-2543 cut one of for
// each repository of an owner, has each of those keys filled at every refresh with the lease that
// repository routes to (ghconfig.RouteDir): no bundle of this daemon holds such a directory, yet
// that pod's wrappers still read it, and a key left as its launch wrote it would expire within
// the hour while the pod lives.
//
// Beside the leases it rewrites the bundle's configured renames where they differ
// (GitHubPairsKey, ghconfig.PairsFile): a pod the launch keeps projects the file of its
// creation, and the kubelet rewrites that file in place when the Secret changes, so a pair
// configured after the pod started reaches the wrappers in it without the pod being replaced.
// What a pod created before the file existed lacks is the projection item itself, which no
// Update can add and the drift check replaces the pod for (movedGhDirs).
//
// bundles is the walk's own memo of the bundles it has built (appauth.Bundles): the slots of a pod
// that run as one App and for one repository are one bundle, built by the first of them. The
// controller's is asked for by owner and memoised by nothing, a controller pod holding the one
// role.
func (r *Runtime) refreshGitHubCredential(ctx context.Context, pod *corev1.Pod, uid types.UID, slot claim.Agent, held map[slotKey]slotHolder, repo ghrepo.Repository, bundles *appauth.Bundles) {
	var role claim.Role
	judge := func(ctx context.Context, secret *corev1.Secret) (claim.Role, error) {
		var err error
		role, err = r.slotAppRole(ctx, held, pod.Name, slot, secret.Annotations[launchedRoleAnnotation])
		return role, err
	}
	_, err := r.writeSlotCredential(ctx, pod, uid, slot, repo, bundles, slotWrite{role: judge})
	if err != nil && !errors.Is(err, errSecretNotOwned) && !errors.Is(err, errSlotLaunching) {
		r.log.Warn("sandbox runtime: github credential refresh failed", "sandbox", pod.Name, "slot", slot, "role", role, "err", err)
	}
}

// errSecretNotOwned is a slot Secret another Sandbox owns: the pod's Sandbox was replaced since the
// pod was read, so the write is the new Sandbox's to make.
var errSecretNotOwned = errors.New("the slot's Secret is another Sandbox's")

// errSlotLaunching is a refresh's write of a slot that a launch into its pod is in flight in
// (refreshUpdate): the slot is the launch's to hold until its start ends, and the refresher's next
// tick writes it.
var errSlotLaunching = errors.New("a launch into the slot is in flight")

// markSlotLaunch marks key's slot as launched into until the answered func is called
// (Runtime.launching), so no refresh writes it meanwhile (refreshUpdate). A refresh's write of the
// slot already on its way to the API server is waited out first, so the launch reads the Secret
// that write leaves; ctx ending first unmarks the slot and answers ctx's error. A launch into a pod
// it keeps marks its slot just before it writes the slot's credential (writeLaunchCredential) and
// unmarks it once its start is answered (relaunch).
func (r *Runtime) markSlotLaunch(ctx context.Context, key slotKey) (func(), error) {
	r.launchedMu.Lock()
	r.launching[key]++
	updating := r.updating[key]
	r.launchedMu.Unlock()
	unmark := func() {
		r.launchedMu.Lock()
		defer r.launchedMu.Unlock()
		if r.launching[key]--; r.launching[key] <= 0 {
			delete(r.launching, key)
		}
	}
	if updating != nil {
		select {
		case <-updating:
		case <-ctx.Done():
			unmark()
			return nil, ctx.Err()
		}
	}
	return unmark, nil
}

// refreshUpdate makes update, the refresher's write of key's slot, unless a launch into the slot
// is in flight (markSlotLaunch), which answers errSlotLaunching and writes nothing. The check and
// the record of the write on its way are made under one lock, and a launch that marks the slot
// while the write is on its way waits it out, so no refresh's write lands in a slot after a launch
// into it has read the Secret, whether or not that launch wrote it: a launch that found its token
// in the Secret already writes nothing that a resourceVersion could refuse a refresh's write over.
func (r *Runtime) refreshUpdate(key slotKey, update func() error) error {
	r.launchedMu.Lock()
	if r.launching[key] > 0 {
		r.launchedMu.Unlock()
		return errSlotLaunching
	}
	done := make(chan struct{})
	r.updating[key] = done
	r.launchedMu.Unlock()
	defer func() {
		r.launchedMu.Lock()
		if r.updating[key] == done {
			delete(r.updating, key)
		}
		r.launchedMu.Unlock()
		close(done)
	}()
	return update()
}

// slotCredential is what a slot's Secret holds after its write (writeSlotCredential): its default
// owner, the token that owner's hosts.yml holds, and the App that minted it. An empty owner is a
// slot whose gh acts as nobody, which holds no token.
type slotCredential struct {
	owner, token, app string
}

// slotWrite is what one write of a slot's Secret is made for (writeSlotCredential). role judges,
// on the Secret as the write read it, the role whose App the slot is written for: a refresh's
// judges it on what the Secret records (refreshGitHubCredential), a launch's is the role of the
// step its claim does now. launched is the record of a launch's process, which the write puts on
// the Secret (launchedRecord), and "" for a refresh, which records nothing.
type slotWrite struct {
	role     func(ctx context.Context, secret *corev1.Secret) (claim.Role, error)
	launched string
}

// launchedRoleAnnotation records, on a slot's Secret, the role the slot's process runs as and the
// generation of its claim whose launch started the claim in that role (launchedRecord). A process
// runs its role for its whole life, since a claim takes another role only while it runs nothing
// (supervise.Machine.Rerole). A launch into a pod it keeps writes the record with the slot's
// credential, in the one update, where it starts the claim in another role than the record names
// (writeLaunchCredential, writeSlotCredential), and the build of a pod writes it for the launching
// claim's slot and clears it on every other, whose process the build ends (writeLauncherSecrets).
// The refresher renders a slot for it while it is of the stored claim's latest generation or a
// later one (slotAppRole).
const launchedRoleAnnotation = "legion.dev/launched-role"

// launchedRecord is the record a launch of generation, as role, leaves on its slot's Secret
// (launchedRoleAnnotation): `<generation>/<role>`.
func launchedRecord(generation uint64, role claim.Role) string {
	return strconv.FormatUint(generation, 10) + "/" + string(role)
}

// readLaunched is the generation and the role a slot's record names (launchedRecord), and false
// for a Secret that holds none or holds one of another shape.
func readLaunched(record string) (uint64, claim.Role, bool) {
	at, role, ok := strings.Cut(record, "/")
	if !ok || role == "" {
		return 0, "", false
	}
	generation, err := strconv.ParseUint(at, 10, 64)
	return generation, claim.Role(role), err == nil
}

// writeSlotCredential is the one-slot write, shared by the refresher (refreshGitHubCredential) and
// a launch into a pod it keeps (writeLaunchCredential): slot's Secret in pod, owned by the Sandbox
// with uid, minted for the role write judges on that Secret (built through bundles, the caller's
// memo) and rendered for repo in the layout the slot's projection reads (servedLayout), rewritten
// only in the directories whose hosts.yml is not the render, in the configured renames where they
// differ, and in the record of the launch's role where write names one the Secret does not hold.
// It answers what the Secret holds then, whether this call wrote it or found it already so, as
// its doc above (refreshGitHubCredential) says of each step. The role is judged on the Secret the
// update then writes, under its resourceVersion: a write refused as a conflict, because something
// wrote the Secret since it was read — a launch binding it to its new pod (bindLauncherSecrets),
// or a launch into the pod writing the slot for its claim's role — is judged, rendered and made
// once more on a fresh read, so that write is kept and its record read. A refresh's update is
// made only while no launch into the slot is in flight (refreshUpdate), and answers
// errSlotLaunching otherwise.
func (r *Runtime) writeSlotCredential(ctx context.Context, pod *corev1.Pod, uid types.UID, slot claim.Agent, repo ghrepo.Repository, bundles *appauth.Bundles, write slotWrite) (slotCredential, error) {
	sandbox := pod.Name
	name := roleSecretName(sandbox, launcherOf(slot))
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	for retried := false; ; retried = true {
		secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return slotCredential{}, err
		}
		if !ownedBySandbox(secret.OwnerReferences, uid) {
			return slotCredential{}, errSecretNotOwned
		}
		role, err := write.role(ctx, secret)
		if err != nil {
			return slotCredential{}, err
		}
		recorded, ok := secret.Data[GitHubDefaultKey]
		if !ok {
			return slotCredential{}, fmt.Errorf("Secret %s records no %s", name, GitHubDefaultKey)
		}
		owner := string(recorded)
		if owner == "" {
			return slotCredential{}, nil
		}
		minting, cancel := call(ctx)
		var bundle ghconfig.Bundle
		if slot == claim.Agent(claim.RoleController) {
			bundle, err = r.controllerCredential(minting, owner)
		} else {
			bundle, err = bundles.Bundle(minting, role, repo)
		}
		cancel()
		if err != nil {
			return slotCredential{}, err
		}
		served := servedLayout(pod, slot, repo, bundle)
		written, ok := served[owner]
		if !ok {
			return slotCredential{}, fmt.Errorf("the credential holds no directory for the Secret's default owner %s", owner)
		}
		token, err := ghconfig.TokenFromHosts([]byte(written.Hosts))
		if err != nil {
			return slotCredential{}, fmt.Errorf("the credential's %s: %w", owner, err)
		}
		held := slotCredential{owner: owner, token: token, app: written.App}
		var changed []string
		for _, dir := range slices.Sorted(maps.Keys(served)) {
			if string(secret.Data[GitHubHostsKey(dir)]) != served[dir].Hosts {
				changed = append(changed, dir)
			}
		}
		// The pairs turn over with the deployment's configuration and GitHub's answers for their
		// names rather than with a lease, so they are compared on their own: a pod the launch keeps
		// projects the file it was created with, and the kubelet rewrites its content in place when
		// the Secret changes, which is how a pair configured since the pod started reaches the
		// wrappers in it.
		pairs := ghconfig.RenderPairs(bundle.Pairs)
		stalePairs := string(secret.Data[GitHubPairsKey]) != pairs
		// The record is rewritten where the launch starts the claim in another role than it names,
		// or the Secret holds none: a record of the same role stays with the generation that took
		// it, which every later generation of that role reads past to the stored role, the same one.
		record := false
		if write.launched != "" {
			_, was, ok := readLaunched(secret.Annotations[launchedRoleAnnotation])
			_, now, _ := readLaunched(write.launched)
			record = !ok || was != now
		}
		if len(changed) == 0 && !stalePairs && !record {
			return held, nil
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for _, dir := range changed {
			rendered := served[dir]
			secret.Data[GitHubHostsKey(dir)], secret.Data[GitHubConfigKey(dir)] = []byte(rendered.Hosts), []byte(rendered.Config)
		}
		secret.Data[GitHubPairsKey] = []byte(pairs)
		if record {
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations[launchedRoleAnnotation] = write.launched
		}
		update := func() error {
			_, err := secrets.Update(ctx, secret, metav1.UpdateOptions{})
			return err
		}
		if write.launched == "" {
			err = r.refreshUpdate(slotKey{sandbox: sandbox, slot: slot}, update)
		} else {
			err = update()
		}
		if err == nil {
			if len(changed) > 0 || stalePairs {
				r.log.Info("sandbox runtime: github credential refreshed", "sandbox", sandbox, "slot", slot, "role", role, "app", written.App,
					"expiresAt", written.ExpiresAt, "dirs", changed, "renames", stalePairs)
			}
			return held, nil
		}
		if !apierrors.IsConflict(err) || retried {
			return slotCredential{}, err
		}
	}
}

// writeLaunchCredential writes the slot of spec's claim in pod, a pod the launch keeps, for the App
// of the step the claim does now (runtime.SlotRole), at once and with the one-slot write
// (writeSlotCredential), records on the Secret in that write the role the launch's process runs
// as (launchedRoleAnnotation), which the refresher renders the slot for from then on, and answers
// what the slot's gh files must hold before the child starts: the default owner's hosts.yml and
// the digest of its token, with no bound on the launcher's wait for it, which relaunch sets from
// its own deadline. A slot whose gh acts as nobody needs nothing. The kubelet projects the
// Secret anew within its sync period, and the launcher waits for it; so that no wait is silent,
// the start logs the wait whenever the slot's files may not hold its credential yet: at the first
// start this runtime makes in the slot, and at each start of another App than the slot's last,
// whether this write or the refresher's changed the Secret.
func (r *Runtime) writeLaunchCredential(ctx context.Context, pod *corev1.Pod, spec runtime.SpawnSpec) (*shimwire.LauncherGitHubCredential, error) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return nil, fmt.Errorf("pod %s names no Sandbox that owns it", pod.Name)
	}
	slot, _ := spec.Claim.Slot()
	role, err := runtime.SlotRole(ctx, r.actsAs, spec.Claim, spec.Role)
	if err != nil {
		return nil, err
	}
	repo, _ := fetchRepository(pod)
	held, err := r.writeSlotCredential(ctx, pod, owner.UID, slot, repo, appauth.NewBundles(r.gitHubCredential), slotWrite{
		role:     func(context.Context, *corev1.Secret) (claim.Role, error) { return role, nil },
		launched: launchedRecord(spec.Generation, spec.Role),
	})
	if err != nil {
		return nil, err
	}
	if held.owner == "" {
		return nil, nil
	}
	key := slotKey{sandbox: pod.Name, slot: slot}
	r.launchedMu.Lock()
	last, started := r.launchedApp[key]
	r.launchedApp[key] = held.app
	r.launchedMu.Unlock()
	if !started || last != held.app {
		r.log.Info("sandbox runtime: the start waits for its slot's gh files to hold its credential",
			"claim", spec.Claim, "slot", slot, "app", held.app, "generation", spec.Generation)
	}
	return &shimwire.LauncherGitHubCredential{HostsFile: held.owner + "/" + ghconfig.HostsFile, TokenDigest: shimwire.GitHubTokenDigest(held.token)}, nil
}

// slotAppChanges is whether a launch of spec into pod, a pod it keeps, would start its claim's slot
// as another App than the slot's gh files may hold: the App of the role the launch's process runs
// as (runtime.SlotRoles) against that of the slot's own name, the role a slot of a pod the release
// before built ran as, and that of the role the slot's last launch recorded on its Secret
// (launchedRoleAnnotation), where it records one. A role that acts as no App the deployment
// defines is another App. It reads the slot's Secret and writes nothing; relaunch asks it only of a
// launcher that does not wait for a start's credential.
//
// runtime.SlotRoles asks the runtime's ActsAs once, for the claim, and answers the rule each role
// read here is judged by, which is the rule one role is judged by at a launch or a refresh
// (runtime.SlotRole): an ActsAs that gives the claim a role gives it for every role read here. A
// failed ask, or a failed read of the Secret, is an error the launch retries, never a slot read as
// starting another App: that answer holds the launch for the pod's update round
// (runtime.AwaitsPodUpdate), which a read that failed for a moment must not cost.
func (r *Runtime) slotAppChanges(ctx context.Context, pod *corev1.Pod, spec runtime.SpawnSpec) (bool, error) {
	slotRole, err := runtime.SlotRoles(ctx, r.actsAs, spec.Claim)
	if err != nil {
		return false, err
	}
	appOf := func(role claim.Role) (appauth.AppRole, error) {
		return appauth.AppRoleFor(slotRole(role), r.deploymentApps)
	}
	app, err := appOf(spec.Role)
	if err != nil {
		return false, err
	}
	slot, _ := spec.Claim.Slot()
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, roleSecretName(pod.Name, launcherOf(slot)), metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	held := []claim.Role{launcherOf(slot)}
	if _, launched, ok := readLaunched(secret.Annotations[launchedRoleAnnotation]); ok {
		held = append(held, launched)
	}
	for _, role := range held {
		if was, err := appOf(role); err != nil || was != app {
			return true, nil
		}
	}
	return false, nil
}

// slotKey names one slot of one pod: the Sandbox the pod is named for, and the slot.
type slotKey struct {
	sandbox string
	slot    claim.Agent
}

// slotHolder is the stored claim that runs in a slot: its token, its stored role, the role of the
// step its agent does now, and the generation of it the store holds.
type slotHolder struct {
	token      claim.Token
	role       claim.Role
	generation uint64
}

// slotHolders is every stored claim by the Sandbox its issue names (SandboxName) and the slot it
// runs in (claim.Token.Slot), read in one call of the store.
func (r *Runtime) slotHolders(ctx context.Context) (map[slotKey]slotHolder, error) {
	claims, err := r.store.Claims(ctx)
	if err != nil {
		return nil, err
	}
	held := make(map[slotKey]slotHolder, len(claims))
	for _, c := range claims {
		// A claim the store holds no role for (a row written before claims recorded one) names no
		// slot's App, so its slot keeps its own.
		if slot, ok := c.Token.Slot(); ok && c.Role != "" {
			held[slotKey{sandbox: SandboxName(c.Token), slot: slot}] = slotHolder{token: c.Token, role: c.Role, generation: c.Generation}
		}
	}
	return held, nil
}

// slotAppRole is the role whose App slot of the pod of sandbox holds: runtime.SlotRole of the
// stored claim that runs in it, for the role its process was launched as where launched, the
// record a launch's write of the slot's Secret left (launchedRoleAnnotation), is of the stored
// claim's generation or a later one, and otherwise for that claim's stored role in held. A record
// of a later generation than held's is a launch held was read before, whose role is the one its
// process runs; a record of an earlier one is of a process a later launch replaced, by a release
// that leaves no record, whose role the stored claim holds. SlotRole gives the claim's role unless
// the runtime's ActsAs gives another. A slot is named by its agent, not by the role its claim
// holds: two claims of one issue can hold one role (record.WorkerClaim).
//
// Where no stored claim runs in the slot, it is the slot's own name, and only where that name is a
// role the runtime writes a slot for with no claim (launcherRoles: its issue roles and the
// controller), as the refresh walk passes over every other slot that runs no claim. The slot of an
// agent of the plan whose name is no role (`implementer-2`) names no App, so it is
// refused: the pod's build writes it for the App of its agent's first step (gitHubBundles), and
// its claim's launch for that claim's role (writeLaunchCredential).
func (r *Runtime) slotAppRole(ctx context.Context, held map[slotKey]slotHolder, sandbox string, slot claim.Agent, launched string) (claim.Role, error) {
	h, ok := held[slotKey{sandbox: sandbox, slot: slot}]
	if !ok {
		own := launcherOf(slot)
		if !slices.Contains(r.launcherRoles(), own) {
			return "", fmt.Errorf("the slot %s of %s runs no stored claim, and its name is no role, so it names no App", slot, sandbox)
		}
		return own, nil
	}
	if generation, role, ok := readLaunched(launched); ok && generation >= h.generation {
		h.role = role
	}
	return runtime.SlotRole(ctx, r.actsAs, h.token, h.role)
}

// servedLayout is what the refresh writes into slot's Secret of pod out of bundle, rendered for
// repo, the pod's own repository: the gh files by the directory each is written under — the
// layout that slot's gh projection of the pod can read (ghProjectionOf), which a pod is served
// until its update round replaces it (movedGhDirs).
//
// A projection holding the pairs file reads bundle as it is built. One holding none routes by
// owner alone, so it is handed bundle routed by owner (ghconfig.Bundle.RoutedByOwner): each
// configured pair's `from` owner's directory holding the `to` owner's lease, as the daemon that
// launched it chose. Every `<owner>/<repo>` directory a
// projection names, which a daemon before INFRA-2543 cut, is filled with the lease that
// repository routes to (ghconfig.RouteDir, the rule `legion gh` and `legion git-credential`
// route by): the owner's own for every repository but one a configured pair moved, which is
// filled with the lease of the owner it moved to, the installation that holds it now. That lease
// reaches at least the repository the directory was cut for, and is refreshed as the pod's
// other directories are, where the key left as its launch wrote it would expire within the hour
// while the pod still reads it. A directory whose lease the bundle does not hold is not written,
// and keeps what it holds, as any directory the render no longer holds does.
func servedLayout(pod *corev1.Pod, slot claim.Agent, repo ghrepo.Repository, bundle ghconfig.Bundle) map[string]ghconfig.Rendered {
	projected, _ := ghProjectionOf(pod, launcherOf(slot))
	layout := bundle
	if projected.secret && !projected.pairs {
		layout = bundle.RoutedByOwner(repo)
	}
	served := maps.Clone(layout.Dirs)
	for _, dir := range projected.dirs {
		owner, name, nested := strings.Cut(dir, "/")
		if !nested {
			continue
		}
		routed, _, _ := ghconfig.RouteDir(bundle.Pairs, owner, name)
		if lease, held := bundle.Dirs[routed]; held {
			served[dir] = lease
		}
	}
	return served
}
