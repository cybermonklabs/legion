package daemon

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// projectLocked writes pod's Secret volumes under a.volumes, as the kubelet projects them into
// the pod: for each mount of a Secret volume by a container of the pod, each item of the volume
// (every key of the Secret where the volume lists no items) at
// <volumes>/<pod uid>/<mount path>/<item path>, with the item's mode or the volume's default
// mode. Each file is written whole and renamed into place, so a reader never reads half a token.
// An item whose key the Secret does not hold is removed. A mount with a subPath is left out: the
// kubelet never updates one, and no process the rig runs reads one. only, where set, is the one
// Secret whose volumes are written. A file that cannot be written is a fault (requireProjected).
func (a *issueSandboxAPI) projectLocked(pod map[string]any, only string) {
	meta, _ := pod["metadata"].(map[string]any)
	uid, _ := meta["uid"].(string)
	spec, _ := pod["spec"].(map[string]any)
	sources := map[string]map[string]any{}
	volumes, _ := spec["volumes"].([]any)
	for _, v := range volumes {
		volume, _ := v.(map[string]any)
		name, _ := volume["name"].(string)
		if source, _ := volume["secret"].(map[string]any); source != nil {
			sources[name] = source
		}
	}
	containers, _ := spec["containers"].([]any)
	for _, c := range containers {
		container, _ := c.(map[string]any)
		mounts, _ := container["volumeMounts"].([]any)
		for _, m := range mounts {
			mount, _ := m.(map[string]any)
			name, _ := mount["name"].(string)
			path, _ := mount["mountPath"].(string)
			subPath, _ := mount["subPath"].(string)
			source := sources[name]
			secretName, _ := source["secretName"].(string)
			if source == nil || subPath != "" || (only != "" && secretName != only) {
				continue
			}
			a.projectSecretLocked(filepath.Join(a.volumes, uid, path), source, a.objects["secrets"][secretName])
		}
	}
}

// projectSecretLocked writes the items of source, a Secret volume, out of secret (nil where the
// API holds no such Secret) under dir, the volume's mount (projectLocked).
func (a *issueSandboxAPI) projectSecretLocked(dir string, source, secret map[string]any) {
	data, _ := secret["data"].(map[string]any)
	mode := kubeFileMode(source["defaultMode"], 0o644)
	type item struct {
		key, path string
		mode      fs.FileMode
	}
	var items []item
	if listed, _ := source["items"].([]any); listed != nil {
		for _, l := range listed {
			entry, _ := l.(map[string]any)
			key, _ := entry["key"].(string)
			path, _ := entry["path"].(string)
			items = append(items, item{key: key, path: path, mode: kubeFileMode(entry["mode"], mode)})
		}
	} else {
		for key := range data {
			items = append(items, item{key: key, path: key, mode: mode})
		}
	}
	for _, it := range items {
		target := filepath.Join(dir, filepath.FromSlash(it.path))
		encoded, held := data[it.key].(string)
		if !held {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				a.kubeletFaults = append(a.kubeletFaults, fmt.Sprintf("remove %s: %v", target, err))
			}
			continue
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil {
			err = writeProjected(target, value, it.mode)
		}
		if err != nil {
			a.kubeletFaults = append(a.kubeletFaults, fmt.Sprintf("project %s: %v", target, err))
		}
	}
}

// kubeFileMode is a Secret volume's mode as JSON decodes it (a number), or otherwise where it
// names none.
func kubeFileMode(mode any, otherwise fs.FileMode) fs.FileMode {
	if number, ok := mode.(float64); ok {
		return fs.FileMode(number)
	}
	return otherwise
}

// writeProjected puts value at target with mode, through a file of its own renamed over target.
func writeProjected(target string, value []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".projecting-")
	if err != nil {
		return err
	}
	_, err = file.Write(value)
	if err == nil {
		err = file.Chmod(mode)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), target)
	}
	if err != nil {
		_ = os.Remove(file.Name())
	}
	return err
}

// mounted is the directory the stand-in's kubelet projects the pod uid's volume mounted at
// mountPath into (projectLocked).
func (a *issueSandboxAPI) mounted(uid, mountPath string) string {
	return filepath.Join(a.volumes, uid, mountPath)
}

// requireProjected fails the test naming every file of a pod's Secret volumes the stand-in's
// kubelet could not write or remove.
func (a *issueSandboxAPI) requireProjected(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.kubeletFaults) > 0 {
		t.Errorf("the rig's kubelet did not project %v", a.kubeletFaults)
	}
}
