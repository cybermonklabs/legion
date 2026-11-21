package writerhold

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	compute "google.golang.org/api/compute/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pdDriver is GKE's Compute Engine persistent disk CSI driver, whose volume handle names the disk
// (projects/<project>/zones/<zone>/disks/<name>, or regions/<region> for a regional disk).
const pdDriver = "pd.csi.storage.gke.io"

// DiskRef is the Compute Engine persistent disk a volume's CSI handle names: Zone for a zonal disk,
// Region for a regional one.
type DiskRef struct {
	Project, Zone, Region, Name string
}

func (d DiskRef) String() string {
	location := "zones/" + d.Zone
	if d.Region != "" {
		location = "regions/" + d.Region
	}
	return "projects/" + d.Project + "/" + location + "/disks/" + d.Name
}

// parseVolumeHandle is the disk a pd.csi.storage.gke.io volume handle names, or false for any
// other shape.
func parseVolumeHandle(handle string) (DiskRef, bool) {
	parts := strings.Split(handle, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[4] != "disks" || parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return DiskRef{}, false
	}
	d := DiskRef{Project: parts[1], Name: parts[5]}
	switch parts[2] {
	case "zones":
		d.Zone = parts[3]
	case "regions":
		d.Region = parts[3]
	default:
		return DiskRef{}, false
	}
	return d, true
}

// Disks reads a persistent disk's numeric id, which Compute Engine gives each disk at its creation
// and never reuses: a disk deleted and made again under its old name, a restored one among them,
// has a new id, so the hold compares disks by id and never by name or handle (round 26, R26-D).
type Disks interface {
	// DiskID is the disk's id as compute.disks.get (a zonal disk) or compute.regionDisks.get (a
	// regional one) answers it.
	DiskID(ctx context.Context, disk DiskRef) (uint64, error)
}

// ComputeDisks is Disks on the Compute Engine API, with the operator's Google identity's
// compute.disks.get (t-11, part 2).
type ComputeDisks struct {
	Service *compute.Service
}

func (c ComputeDisks) DiskID(ctx context.Context, disk DiskRef) (uint64, error) {
	if disk.Region != "" {
		d, err := c.Service.RegionDisks.Get(disk.Project, disk.Region, disk.Name).Context(ctx).Do()
		if err != nil {
			return 0, err
		}
		return d.Id, nil
	}
	d, err := c.Service.Disks.Get(disk.Project, disk.Zone, disk.Name).Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	return d.Id, nil
}

// ClaimBinding is one JetStream claim as the hold reads it: the volume it is bound to and that
// volume's disk with its numeric id. Step 1 records Volume as step1_volume.<Claim> and DiskID as
// step1_disk_id.<Claim> (holdrecord.Step1); the proof requires the same again.
type ClaimBinding struct {
	Claim  string
	Volume string
	Disk   DiskRef
	// DiskID is the disk's numeric id, in decimal, as the hold's journal stores it.
	DiskID string
}

// ReadClaims is step 1's reading of the declared servers' JetStream claims, for the step-1 map:
// each server's claim, the volume it is bound to (spec.volumeName), and that volume's disk with its
// id. It reads each claim and each volume by name, t-11's get on persistentvolumeclaims and on
// persistentvolumes, and each disk through disks. A claim the hold cannot read whole is UNKNOWN:
// `hold set` refuses a target it cannot read, so the map names every target (R28-H).
func ReadClaims(ctx context.Context, hub KubeHub, r *Render, disks Disks) (map[string]ClaimBinding, error) {
	out := map[string]ClaimBinding{}
	for _, pod := range r.Servers {
		b, unbound, err := readClaim(ctx, hub, r.Claim(pod), disks, StepPreflight)
		if err != nil {
			return nil, err
		}
		if unbound != "" {
			return nil, unknownf(StepPreflight, nil, "%s", unbound)
		}
		out[b.Claim] = b
	}
	return out, nil
}

// readClaim reads claim's binding through its volume to the disk's id. A read that fails is
// UNKNOWN of step. A claim bound to no volume, a volume bound to another claim, or one that is not a
// GKE persistent disk is returned as unbound, a finding the caller names: at step 1 it is UNKNOWN,
// at the proof a refusal.
func readClaim(ctx context.Context, hub KubeHub, claim string, disks Disks, step string) (ClaimBinding, string, error) {
	pvc, err := hub.Client.CoreV1().PersistentVolumeClaims(hub.Namespace).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return ClaimBinding{}, "", unknownf(step, err, "read PersistentVolumeClaim %s/%s", hub.Namespace, claim)
	}
	b := ClaimBinding{Claim: claim, Volume: pvc.Spec.VolumeName}
	if b.Volume == "" || pvc.Status.Phase != corev1.ClaimBound {
		return b, fmt.Sprintf("claim %s/%s is bound to no volume (phase %q)", hub.Namespace, claim, pvc.Status.Phase), nil
	}
	pv, err := hub.Client.CoreV1().PersistentVolumes().Get(ctx, b.Volume, metav1.GetOptions{})
	if err != nil {
		return ClaimBinding{}, "", unknownf(step, err, "read PersistentVolume %s", b.Volume)
	}
	if ref := pv.Spec.ClaimRef; ref == nil || ref.Namespace != hub.Namespace || ref.Name != claim {
		return b, fmt.Sprintf("volume %s of claim %s/%s is bound to another claim (%v)", b.Volume, hub.Namespace, claim, pv.Spec.ClaimRef), nil
	}
	csi := pv.Spec.CSI
	if csi == nil || csi.Driver != pdDriver {
		return b, fmt.Sprintf("volume %s of claim %s/%s is not a %s volume", b.Volume, hub.Namespace, claim, pdDriver), nil
	}
	disk, ok := parseVolumeHandle(csi.VolumeHandle)
	if !ok {
		return b, fmt.Sprintf("volume %s's handle %q names no persistent disk", b.Volume, csi.VolumeHandle), nil
	}
	id, err := disks.DiskID(ctx, disk)
	if err != nil {
		return ClaimBinding{}, "", unknownf(step, err, "read disk %s of volume %s", disk, b.Volume)
	}
	b.Disk, b.DiskID = disk, strconv.FormatUint(id, 10)
	return b, "", nil
}

// ProveBinding is step 4's "It runs the checked binary and configuration" for one restarted
// cluster-hub pod, read from the hub's own records with t-11's gets (the ConfigMap, the
// StatefulSet, the pod, its claim and volume) and the disk's id through disks. varz is the pod's
// /varz read after its restart, and want its claim's expected binding: step 1's, or, for a claim
// hub-restore restored, the one the prove expects (Volume "" compares the disk alone). Each is a
// finding it names:
//
//   - the ConfigMap's resourceVersion or content changed since step 1;
//   - the StatefulSet's status.updateRevision changed since step 1, or the pod's
//     controller-revision-hash is not it: the pod does not run the checked template (its args and
//     env sources);
//   - the pod's nats container's imageID is not the pinned digest;
//   - /varz version is not the pinned tag's;
//   - /varz config_digest is not the reader's digest of the render for that pod, where every
//     variable the render references resolves from the pod spec (a server in r.Unresolved skips
//     this one comparison, for the reason Unresolved names);
//   - the claim is bound to another volume, or its disk's id is not want's.
//
// A read that fails, or a pod with no running nats image yet, is UNKNOWN.
func (r *Render) ProveBinding(ctx context.Context, hub KubeHub, disks Disks, pod string, varz Varz, want ClaimBinding) error {
	var f []string
	cm, err := hub.Client.CoreV1().ConfigMaps(r.Namespace).Get(ctx, r.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		return unknownf(StepProof, err, "read ConfigMap %s/%s", r.Namespace, r.ConfigMapName)
	}
	if cm.ResourceVersion != r.ResourceVersion {
		f = append(f, fmt.Sprintf("ConfigMap %s/%s is at resourceVersion %s, not step 1's %s", r.Namespace, r.ConfigMapName, cm.ResourceVersion, r.ResourceVersion))
	}
	if !maps.Equal(cm.Data, r.Files) || len(cm.BinaryData) > 0 {
		f = append(f, fmt.Sprintf("ConfigMap %s/%s no longer holds the checked render", r.Namespace, r.ConfigMapName))
	}
	set, err := hub.Client.AppsV1().StatefulSets(r.Namespace).Get(ctx, r.StatefulSetName, metav1.GetOptions{})
	if err != nil {
		return unknownf(StepProof, err, "read StatefulSet %s/%s", r.Namespace, r.StatefulSetName)
	}
	if set.Status.UpdateRevision != r.UpdateRevision {
		f = append(f, fmt.Sprintf("StatefulSet %s/%s's update revision is %s, not step 1's %s", r.Namespace, r.StatefulSetName, set.Status.UpdateRevision, r.UpdateRevision))
	}
	p, err := hub.Client.CoreV1().Pods(r.Namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return unknownf(StepProof, err, "read pod %s/%s", r.Namespace, pod)
	}
	if revision := p.Labels[appsv1.ControllerRevisionHashLabelKey]; revision != set.Status.UpdateRevision {
		f = append(f, fmt.Sprintf("pod %s runs revision %q, not StatefulSet %s's update revision %s", pod, revision, r.StatefulSetName, set.Status.UpdateRevision))
	}
	digest, err := runningDigest(p)
	if err != nil {
		return err
	}
	if digest != r.Image.Digest {
		f = append(f, fmt.Sprintf("pod %s runs image %s, not the pinned %s", pod, digest, r.Image.Reference))
	}
	if varz.Version != r.Image.Version {
		f = append(f, fmt.Sprintf("pod %s's server reports version %q, not %s of the pinned tag %s", pod, varz.Version, r.Image.Version, r.Image.Tag))
	}
	if want, ok := r.Digests[pod]; ok && varz.ConfigDigest != want {
		f = append(f, fmt.Sprintf("pod %s's server reports config_digest %q, not %s, the checked render's as that pod reads it", pod, varz.ConfigDigest, want))
	}
	b, unbound, err := readClaim(ctx, hub, r.Claim(pod), disks, StepProof)
	if err != nil {
		return err
	}
	switch {
	case unbound != "":
		f = append(f, unbound)
	case want.Volume != "" && b.Volume != want.Volume:
		f = append(f, fmt.Sprintf("claim %s is bound to volume %s, not %s", b.Claim, b.Volume, want.Volume))
	case b.DiskID != want.DiskID:
		f = append(f, fmt.Sprintf("claim %s's disk %s has id %s, not %s", b.Claim, b.Disk, b.DiskID, want.DiskID))
	}
	if len(f) > 0 {
		return refusef(StepProof, "%s", strings.Join(f, "; "))
	}
	return nil
}

// runningDigest is the digest of the image pod's nats container runs, from its
// status.containerStatuses[].imageID (<repository>@sha256:<hex>, which a docker runtime prefixes
// with docker-pullable://). A pod whose nats container has not started, or whose imageID names no
// sha256 repository digest, is UNKNOWN.
func runningDigest(p *corev1.Pod) (string, error) {
	for _, s := range p.Status.ContainerStatuses {
		if s.Name != natsContainer {
			continue
		}
		at := strings.LastIndex(s.ImageID, "@")
		if at < 0 || !imageDigest.MatchString(s.ImageID[at+1:]) {
			return "", unknownf(StepProof, nil, "pod %s's nats container reports imageID %q, which names no sha256 repository digest", p.Name, s.ImageID)
		}
		return s.ImageID[at+1:], nil
	}
	return "", unknownf(StepProof, nil, "pod %s reports no status of its nats container", p.Name)
}

// CheckReload stops the hold, at step, when pod's server read config_digest at after other than
// at before while it kept its server_id: the same process took another configuration, as a reload
// leaves it, and the configuration the hold proved is no longer the one it runs. A new server_id
// is a new process, which the proof reads afresh, so it passes here.
func CheckReload(step, pod string, before, after Varz) error {
	if before.ServerID == after.ServerID && before.ConfigDigest != after.ConfigDigest {
		return refusef(step, "pod %s's server %s reloaded its configuration: config_digest %s became %s with the same server_id", pod, after.ServerID, before.ConfigDigest, after.ConfigDigest)
	}
	return nil
}
