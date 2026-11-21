package gcp_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// nestedMounts is every pair of mount paths in pod, a decoded golden or any value inside one, where
// one container mounts a path inside another of its own mount paths, as "inner inside outer, in
// container name", and the count of containers it read. Kubernetes mounts the outer volume over
// the inner path's directory or the inner volume inside the outer one, by the order it picks, so
// one of the two files is hidden or the mount fails, and nothing in a render says which.
func nestedMounts(value any) (nested []string, containers int) {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"containers", "initContainers"} {
			list, _ := v[key].([]any)
			for _, item := range list {
				container, ok := item.(map[string]any)
				if !ok {
					continue
				}
				containers++
				name, _ := container["name"].(string)
				mounts, _ := container["volumeMounts"].([]any)
				var paths []string
				for _, mount := range mounts {
					if m, ok := mount.(map[string]any); ok {
						if path, ok := m["mountPath"].(string); ok {
							paths = append(paths, path)
						}
					}
				}
				for _, outer := range paths {
					for _, inner := range paths {
						if strings.HasPrefix(inner, strings.TrimSuffix(outer, "/")+"/") {
							nested = append(nested, fmt.Sprintf("%s inside %s, in container %s", inner, outer, name))
						}
					}
				}
			}
		}
		for _, child := range v {
			found, read := nestedMounts(child)
			nested, containers = append(nested, found...), containers+read
		}
	case []any:
		for _, child := range v {
			found, read := nestedMounts(child)
			nested, containers = append(nested, found...), containers+read
		}
	}
	return nested, containers
}

// No container of any pod the components render mounts a path inside another of its own mount
// paths: every golden under testdata is read, gcp:Dispatch's and gcp:ClusteredNatsHub's pods and
// the production render among them, and every container in it. A read that finds no container,
// or misses a pod golden it is known to hold, fails rather than passing on nothing. The control is
// a fixture pod with the readiness token at /var/run/secrets/envoy/readiness-token beside the
// credential mount /var/run/secrets/envoy, which the check names, both paths.
func TestNoMountNestsInAnother(t *testing.T) {
	control := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(`{"spec":{"containers":[{"name":"envoy-listener","volumeMounts":[
		{"name":"nats-token","mountPath":"/var/run/secrets/envoy"},
		{"name":"readiness-token","mountPath":"/var/run/secrets/envoy/readiness-token"}]}]}}`), &control))
	found, read := nestedMounts(control)
	require.Equal(t, 1, read, "the control's one container is read")
	require.Equal(t, []string{"/var/run/secrets/envoy/readiness-token inside /var/run/secrets/envoy, in container envoy-listener"}, found,
		"the control's nested mount is found, naming both paths")

	goldens, err := filepath.Glob("testdata/*.golden")
	require.NoError(t, err)
	var withContainers []string
	containers := 0
	for _, golden := range goldens {
		data, err := os.ReadFile(golden)
		require.NoError(t, err)
		var decoded any
		if json.Unmarshal(data, &decoded) != nil {
			continue
		}
		nested, read := nestedMounts(decoded)
		require.Empty(t, nested, "%s mounts a path inside another", golden)
		if read > 0 {
			withContainers = append(withContainers, filepath.Base(golden))
			containers += read
		}
	}
	for _, pod := range []string{
		"dispatch-pod.golden", "dispatch-listener-pod.golden", "dispatch-tailnet-pod.golden",
		"dispatch-pod-with-service-identity.golden", "dispatch-listener-pod-with-service-identity.golden",
		"clustered-hub-listener-pod.golden", "clustered-hub-listener-pod-service-identity.golden",
		"clustered-hub-tailnet-pod.golden", "production-render.golden",
	} {
		require.True(t, slices.Contains(withContainers, pod), "%s was read and holds containers; read: %v", pod, withContainers)
	}
	t.Logf("read %d containers in %d goldens", containers, len(withContainers))
}
