// The Stage 4a harness's image probe checks: the worker image under test, unmodified, passes its
// probe under the operator's pod, and the probe refuses a pod the image cannot serve. The rig is
// live_test.go.

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/prompts"
)

// prober is a runtime of the run's Options on the worker image under test, unmodified
// (liveEnv.probeImage), which the image probe's checks probe: what they check is the image
// itself, never the copy the run's pods start, which adds the fake GitHub's authority. edit, when
// not nil, changes those Options first, never the shared runtime's. The prober shares the run's
// listener, which no probe uses, and ends with ctx.
func (r *liveRig) prober(ctx context.Context, edit func(*Options)) (*Runtime, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return nil, err
	}
	opts := r.runtimeOptions(r.streamAddress(), r.ln)
	opts.Image = r.env.probeImage
	if edit != nil {
		edit(&opts)
	}
	return New(ctx, r.rc, opts)
}

// probeSandbox is the name of the probe Sandbox the prober makes for the worker image under test.
func (r *liveRig) probeSandbox() string {
	_, digest, _ := strings.Cut(r.env.probeImage, "@sha256:")
	return probeName(r.env.project, digest, "")
}

// awaitProbeGone waits for the probe Sandbox to be deleted after an attempt.
func (r *liveRig) awaitProbeGone() error {
	probe := r.probeSandbox()
	if err := r.poll(liveGoneLimit, "probe Sandbox "+probe+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(probe)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", probe)
	return nil
}

// image-probe: the probe Sandbox, carrying the operator's pod, passes on the worker image under
// test, confirms the daemon API contract, resolved the model of every task agent the prompts
// dispatch, and checked the capability list, every image-site capability present; the report
// carries the model-fallback mark the pod read. Its pod is read while the probe runs: the one
// container carries the probe's own reservation as both request and limit (imageProbe), so the
// probe pod is Guaranteed whatever the roles request, and it asks nothing of its placement. The
// negative control is the same pod with its container's cpu limit raised past its request, which
// the reservation rule refuses naming the container.
func (r *liveRig) checkImageProbe() error {
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := r.prober(ctx, nil)
	if err != nil {
		return err
	}
	name := r.probeSandbox()
	p := r.imageProbe()
	// The probe pod lives from the controller's create until ProbeImage deletes the Sandbox, so a
	// poll started before the probe sees it in whatever phase it is in; its spec and qosClass are
	// set at creation.
	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	probePod := make(chan *corev1.Pod, 1)
	go func() {
		defer close(probePod)
		for {
			if pod, err := r.getPod(name); err == nil {
				probePod <- pod
				return
			}
			select {
			case <-watching.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	report, err := rt.ProbeImage(ctx, p)
	if err != nil {
		return err
	}
	stopWatching()
	passed, ok := r.logs.find("sandbox runtime: the worker image passed its probe")
	if !ok {
		return errors.New("ProbeImage returned nil without logging a pass")
	}
	contract, ok := bootprobe.ConfirmedContract(passed["log"])
	if !ok || contract != api.DaemonAPIVersion {
		return fmt.Errorf("the probe log confirms contract %d (found %t), want %d: %s", contract, ok, api.DaemonAPIVersion, passed["log"])
	}
	note("runtime", "probe Sandbox %s passed on %s: %s", passed["sandbox"], r.env.probeImage, lastLine(passed["log"], bootprobe.OKPrefix))
	note("runtime", "daemon-api-version=%d parsed, the daemon's contract", contract)
	if models := bootprobe.AgentModels(passed["log"]); models != bootprobe.AgentModelsResolved {
		return fmt.Errorf("the probe log says agent-models=%q, want %q: %s", models, bootprobe.AgentModelsResolved, passed["log"])
	}
	note("runtime", "agent-models=%s parsed: every task agent the prompts dispatch resolved its model under the operator's pod", bootprobe.AgentModelsResolved)
	if checked := bootprobe.Capabilities(passed["log"]); checked != bootprobe.CapabilitiesAll {
		return fmt.Errorf("the probe log says capabilities=%q, want %q: %s", checked, bootprobe.CapabilitiesAll, passed["log"])
	}
	note("runtime", "capabilities=checked parsed: every image-site capability present under the operator's pod")
	if report.ModelFallback != "on" && report.ModelFallback != "off" {
		return fmt.Errorf("the probe reports model-fallback=%q, want on or off: %s", report.ModelFallback, passed["log"])
	}
	note("runtime", "model-fallback=%s reported: the image's retry.modelFallback under the operator's pod", report.ModelFallback)
	pod, ok := <-probePod
	if !ok || pod == nil {
		return fmt.Errorf("the probe pod %s was never read while the probe ran, so its reservation and QoS are unproven", name)
	}
	if err := guaranteedPod(pod, map[string]corev1.ResourceRequirements{probeContainer: p.Resources}, nil, resource.Quantity{}, false, false); err != nil {
		return fmt.Errorf("probe pod %s (phase %s): %w", name, pod.Status.Phase, err)
	}
	container := pod.Spec.Containers[0]
	note("operator", "probe pod %s read in phase %s: container %s cpu %s, memory %s, request = limit, the probe's own reservation, ephemeral-storage %s under a limit of %s; qosClass %s; no affinity",
		name, pod.Status.Phase, container.Name, container.Resources.Limits.Cpu().String(), container.Resources.Limits.Memory().String(), container.Resources.Requests.StorageEphemeral().String(), container.Resources.Limits.StorageEphemeral().String(), pod.Status.QOSClass)
	bursting := pod.DeepCopy()
	doubled := bursting.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	doubled.Add(doubled)
	bursting.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = doubled
	err = guaranteedPod(bursting, map[string]corev1.ResourceRequirements{probeContainer: p.Resources}, nil, resource.Quantity{}, false, false)
	if err == nil || !strings.Contains(err.Error(), "container "+probeContainer) {
		return fmt.Errorf("negative control: the reservation rule accepted the probe pod with its cpu limit raised to %s, or did not name the container: %v", doubled.String(), err)
	}
	note("runtime", "negative control: the same pod with its container's cpu limit raised to %s is refused: %v", doubled.String(), err)
	return r.awaitProbeGone()
}

// image-probe-negative: with modelRoles.oracle gone from the run's ConfigMap, the probe on the same
// image refuses, naming the agent and the role no one configured, since the task tool would run
// every oracle consult on the session's model; the ConfigMap is restored before the check ends,
// and every later check boots on it.
func (r *liveRig) checkImageProbeRefusal() error {
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := r.prober(ctx, nil)
	if err != nil {
		return err
	}
	name := r.env.operatorConfigMap
	overlay, err := r.kubectl("get", "configmap", name, "-o", `jsonpath={.data.overlay\.yml}`)
	if err != nil {
		return err
	}
	var kept []string
	for line := range strings.Lines(overlay) {
		if !strings.HasPrefix(line, "  oracle: ") {
			kept = append(kept, line)
		}
	}
	if removed := strings.Count(overlay, "\n") - strings.Count(strings.Join(kept, ""), "\n"); removed != 1 {
		return fmt.Errorf("ConfigMap %s's overlay.yml holds %d modelRoles.oracle lines, want 1", name, removed)
	}
	patch := func(text string) error {
		body, err := json.Marshal(map[string]any{"data": map[string]string{"overlay.yml": text}})
		if err != nil {
			return err
		}
		_, err = r.kubectl("patch", "configmap", name, "--type", "merge", "-p", string(body))
		return err
	}
	if err := patch(strings.Join(kept, "")); err != nil {
		return err
	}
	note("operator", "ConfigMap %s: modelRoles.oracle removed from overlay.yml", name)
	p := r.imageProbe()
	_, refusal := rt.ProbeImage(ctx, p)
	if err := patch(overlay); err != nil {
		return fmt.Errorf("restore ConfigMap %s: %w", name, err)
	}
	note("operator", "ConfigMap %s: overlay.yml restored", name)
	if refusal == nil {
		return errors.New("ProbeImage passed with modelRoles.oracle removed from the operator's overlay")
	}
	for _, want := range []string{"task agent oracle (dispatched by ", "on its model @oracle: role oracle is not configured"} {
		if !strings.Contains(refusal.Error(), want) {
			return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
		}
	}
	note("runtime", "refused: %s", lineContaining(refusal.Error(), "role oracle is not configured"))
	return r.awaitProbeGone()
}

// image-probe-capability-negative: with PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium in the
// operator's pod env, the probe on the same image refuses naming the browser capability missing:
// the probe honours the variable as Oh My Pi does, so a wrong value in an operator's pod env is
// caught at boot rather than in a worker's first browser call. The probe runs on a prober whose
// Options add that variable to a copy of the pod env, so the shared runtime's pod is never
// changed.
func (r *liveRig) checkImageProbeCapabilityRefusal() error {
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := r.prober(ctx, func(opts *Options) {
		env := maps.Clone(r.pod.Env)
		if env == nil {
			env = map[string]string{}
		}
		env["PUPPETEER_EXECUTABLE_PATH"] = "/nonexistent/chromium"
		opts.Pod.Env = env
	})
	if err != nil {
		return fmt.Errorf("a runtime whose pod env sets PUPPETEER_EXECUTABLE_PATH: %w", err)
	}
	note("operator", "pod env for this probe: the fixture's %d variables and PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium", len(r.pod.Env))
	p := r.imageProbe()
	_, refusal := rt.ProbeImage(ctx, p)
	if refusal == nil {
		return errors.New("ProbeImage passed with PUPPETEER_EXECUTABLE_PATH naming nothing")
	}
	if want := "capability browser is missing"; !strings.Contains(refusal.Error(), want) {
		return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
	}
	note("runtime", "refused: %s", lineContaining(refusal.Error(), "capability browser is missing"))
	return r.awaitProbeGone()
}

// image-probe-missing-volume-negative: with a volume operator-missing, from a ConfigMap
// `<operator ConfigMap>-missing` that does not exist, added to the operator's pod volumes and
// mounted at /etc/legion-operator-missing (the kubelet skips a volume no container mounts), the
// probe on the same image refuses naming the volume, the ConfigMap runtime.kubernetes.pod.volumes
// gives it from, and the kubelet's own `configmap "<name>" not found`: a FailedMount for an operator
// volume whose object is missing is read on the first event read after the pod is scheduled
// (mountRecheck), never retried to the budget. The refusal is timed from the pod's PodScheduled
// transition, read from the prober's store while the probe runs. The prober's Options add the
// volume and its mount to copies of the pod's slices, so the shared runtime's pod is never changed.
func (r *liveRig) checkImageProbeMissingVolumeRefusal() error {
	missing := r.env.operatorConfigMap + "-missing"
	if out, err := r.kubectl("get", "configmap", missing, "-o", "name"); err == nil {
		return fmt.Errorf("ConfigMap %s exists (%s): the check relies on no ConfigMap of that name", missing, strings.TrimSpace(out))
	} else if !strings.Contains(err.Error(), "(NotFound)") {
		return err
	}
	ctx, stop := context.WithCancel(r.ctx)
	defer stop()
	rt, err := r.prober(ctx, func(opts *Options) {
		opts.Pod.Volumes = append(slices.Clone(r.pod.Volumes), corev1.Volume{Name: "operator-missing", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: missing}},
		}})
		opts.Pod.VolumeMounts = append(slices.Clone(r.pod.VolumeMounts), corev1.VolumeMount{Name: "operator-missing", MountPath: "/etc/legion-operator-missing", ReadOnly: true})
	})
	if err != nil {
		return fmt.Errorf("a runtime whose pod mounts ConfigMap %s: %w", missing, err)
	}
	note("operator", "pod volumes for this probe: the fixture's %d and operator-missing from ConfigMap %s, which does not exist, mounted read-only at /etc/legion-operator-missing beside the fixture's %d mounts",
		len(r.pod.Volumes), missing, len(r.pod.VolumeMounts))
	probe := r.probeSandbox()
	// The probe pod is scheduled, then read for its FailedMount, then deleted with its Sandbox,
	// all inside ProbeImage: the last pod the prober's store held is kept for its PodScheduled
	// transition.
	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	lastSeen := make(chan *corev1.Pod, 1)
	go func() {
		var last *corev1.Pod
		defer func() { lastSeen <- last }()
		for {
			if pod := rt.storedPod(probe); pod != nil {
				last = pod
			}
			select {
			case <-watching.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	p := r.imageProbe()
	started := time.Now()
	_, refusal := rt.ProbeImage(ctx, p)
	refusedAt := time.Now()
	stopWatching()
	pod := <-lastSeen
	// A refusal read on the first event read after scheduling can land inside one tick of the
	// watcher, so it may never have sampled the pod. The Sandbox's delete is background
	// propagation, so the store still holds the pod just after ProbeImage returns: read it once more.
	if pod == nil {
		pod = rt.storedPod(probe)
	}
	if refusal == nil {
		return fmt.Errorf("ProbeImage passed with volume operator-missing naming ConfigMap %s, which does not exist", missing)
	}
	for _, want := range []string{`cannot mount volume "operator-missing"`, `ConfigMap "` + missing + `"`} {
		if !strings.Contains(refusal.Error(), want) {
			return fmt.Errorf("the probe's refusal does not say %q: %v", want, refusal)
		}
	}
	kubelet := []string{`configmap "` + missing + `" not found`, `configmaps "` + missing + `" not found`}
	if !slices.ContainsFunc(kubelet, func(form string) bool { return strings.Contains(refusal.Error(), form) }) {
		return fmt.Errorf("the probe's refusal carries neither of the kubelet's forms %q: %v", kubelet, refusal)
	}
	if strings.Contains(refusal.Error(), "never completed within its retry budget") {
		return fmt.Errorf("the probe retried the mount to its budget instead of refusing it on the first read: %v", refusal)
	}
	if pod == nil {
		return fmt.Errorf("the probe pod %s was never read while the probe ran, so the refusal's bound from its scheduling is unproven", probe)
	}
	var scheduledAt time.Time
	if scheduled := podCondition(pod, corev1.PodScheduled); scheduled != nil && scheduled.Status == corev1.ConditionTrue {
		scheduledAt = scheduled.LastTransitionTime.Time
	}
	if scheduledAt.IsZero() {
		return fmt.Errorf("the probe pod %s as last read (phase %s) carries no PodScheduled=True condition, so the refusal's bound from its scheduling is unproven", probe, pod.Status.Phase)
	}
	sinceScheduled := refusedAt.Sub(scheduledAt)
	// The reads after scheduling fall at the first poll, then every mountRecheck; a kubelet whose
	// first mount attempt lands after the first recheck is found on the second, so the bound allows
	// two rechecks, with 15 s of slack for the poll's tick, the API server's second-truncated
	// transition time and the kubelet's first volume sync. The refusal's own claim is "within one
	// read of the kubelet's first FailedMount", which the noted durations show; this bound only says
	// the attempt was not waited out.
	if limit := 2*mountRecheck + 15*time.Second; sinceScheduled > limit {
		return fmt.Errorf("the refusal came %s after the probe pod was scheduled, past the %s two event reads every %s, the poll's tick and the kubelet's first volume sync allow", sinceScheduled.Round(time.Second), limit, mountRecheck)
	}
	note("runtime", "refused %s after ProbeImage was called, %s after the pod was scheduled: %s", refusedAt.Sub(started).Round(time.Second), sinceScheduled.Round(time.Second), lineContaining(refusal.Error(), "cannot mount volume"))
	return r.awaitProbeGone()
}

// imageProbe is the probe as the daemon asks for it: its contract, the references of the role
// prompts it hands every pod (prompts.RoleReferences), and the probe's own fixed reservation
// (internal/daemon/kubernetes.go imageProbe, probeReservation) — 500m and 1Gi, not a role's share,
// so a probe pod fits a pool with little room — with cpu and memory the request and the limit
// alike, so the probe pod is Guaranteed whatever the roles request. The probe command it sends, with
// --role-references and, the run having a provider key, --provider-env-dir, is noted.
func (r *liveRig) imageProbe() ImageProbe {
	p := ImageProbe{
		Contract: api.DaemonAPIVersion, Budget: 10 * time.Minute, RoleReferences: prompts.RoleReferences(),
		Retry:     bootprobe.Retry{Initial: 15 * time.Second, Max: time.Minute, Attempts: 3},
		Resources: probeReservation(),
	}
	command := r.rt.probeManifest("probe", p, time.Now()).Spec.PodTemplate.Spec.Containers[0].Command
	note("runtime", "the probe command: %s", strings.Join(command, " "))
	return p
}

func lastLine(text, prefix string) string {
	for _, line := range slices.Backward(strings.Split(text, "\n")) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
