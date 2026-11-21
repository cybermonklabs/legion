package writerhold

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nats-io/nats-server/v2/conf"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// natsContainer is the nats chart's name for the container that runs nats-server
// (files/stateful-set/nats-container.yaml of chart 2.15.1); its reloader and exporter sidecars
// run beside it.
const natsContainer = "nats"

// ImageRef is the image reference the StatefulSet pins for nats-server, parsed strictly as
// <repository>:<major>.<minor>.<patch>[-<suffix>]@sha256:<64 hex>, for example
// mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae5…: the tag says which release runs, and the
// digest which bytes. The binding compares each pod's running image with Digest and each server's
// /varz version with Version.
type ImageRef struct {
	// Reference is the whole reference as the StatefulSet writes it.
	Reference string
	// Repository is the part before the tag, registry and path included.
	Repository string
	// Tag is the tag, suffix included (2.15.1-alpine).
	Tag string
	// Version is the tag's <major>.<minor>.<patch> (2.15.1), which /varz version reports. The
	// suffix names the image's variant (alpine), not the release, so it is not part of it.
	Version string
	// Digest is sha256:<64 hex>.
	Digest string
}

var (
	imageTag    = regexp.MustCompile(`^((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))(-[A-Za-z0-9][A-Za-z0-9._-]*)?$`)
	imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// parseImageRef parses reference strictly, as ImageRef says; any other shape is UNKNOWN, naming
// the reference: without a digest the hold cannot say which bytes run, and without a release tag it
// cannot say which version /varz must report.
func parseImageRef(reference string) (ImageRef, error) {
	name, digest, found := strings.Cut(reference, "@")
	if !found || strings.Contains(digest, "@") || !imageDigest.MatchString(digest) {
		return ImageRef{}, unknownf(StepPreflight, nil, "the nats image %q is not pinned by a sha256 digest (<repository>:<major>.<minor>.<patch>[-<suffix>]@sha256:<64 hex>)", reference)
	}
	colon := strings.LastIndex(name, ":")
	if colon <= strings.LastIndex(name, "/") {
		return ImageRef{}, unknownf(StepPreflight, nil, "the nats image %q names no tag (<repository>:<major>.<minor>.<patch>[-<suffix>]@sha256:<64 hex>)", reference)
	}
	repository, tag := name[:colon], name[colon+1:]
	match := imageTag.FindStringSubmatch(tag)
	if repository == "" || strings.ContainsFunc(repository, isSpaceOrControl) || match == nil {
		return ImageRef{}, unknownf(StepPreflight, nil, "the nats image %q is not <repository>:<major>.<minor>.<patch>[-<suffix>]@sha256:<64 hex>", reference)
	}
	return ImageRef{Reference: reference, Repository: repository, Tag: tag, Version: match[1], Digest: digest}, nil
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// Render is the configuration the clustered hub's servers run, as the hold read it at step 1 from
// the hub's own records: the ConfigMap the nats container mounts, and the StatefulSet that says how
// each pod starts. The proof (ProveBinding) compares each restarted pod against it.
type Render struct {
	// Namespace, ConfigMapName and StatefulSetName name the two objects read.
	Namespace, ConfigMapName, StatefulSetName string
	// ResourceVersion is the ConfigMap's at the read; the proof requires it unchanged.
	ResourceVersion string
	// Files are the ConfigMap's data: each key is one file of the pods' configuration directory
	// (nats.conf and the auth.conf it includes, for the chart).
	Files map[string]string
	// ConfigFile is the key of Files the nats container starts with (its --config).
	ConfigFile string
	// Servers are the declared servers: the StatefulSet's pods, by name.
	Servers []string
	// Image is the pinned nats image.
	Image ImageRef
	// UpdateRevision is the StatefulSet's status.updateRevision: the revision of the pod template
	// read here, which every restarted pod must carry.
	UpdateRevision string
	// ClaimTemplate is the name of the StatefulSet's one volume claim template, the JetStream
	// store; pod p's claim is <ClaimTemplate>-<p>.
	ClaimTemplate string
	// Digests is, for each server whose every configuration variable resolves from the pod spec,
	// the digest nats-server computes over the render as that pod reads it (its /varz
	// config_digest). A server absent here has a variable the operator cannot read (Unresolved).
	Digests map[string]string
	// Unresolved names, for each server, each variable the render references that the reader could
	// not resolve from the pod spec, with its source (a secretKeyRef, for the route credentials of
	// t-13): the proof skips that server's digest equality, and the report says why.
	Unresolved map[string][]string
	// AuthUser is the auth service's own static user, the one user auth_callout's auth_users lists,
	// as nats-server's /connz?auth=1 shows its authorized_user: its public nkey for an nkey user,
	// which core renders, else its username (server/client.go getRawAuthUser, v2.15.1:6887-6900).
	// Precondition sets it once check passes, and every declared server's render names the same one.
	AuthUser string

	// parsed is each server's render as nats-server's conf package parses it, for check.
	parsed map[string]map[string]any
}

// Claim is the name of pod's JetStream claim, the StatefulSet's claim template for that pod.
func (r *Render) Claim(pod string) string { return r.ClaimTemplate + "-" + pod }

// natsSpec is what the reader takes from the StatefulSet: how its nats container starts.
type natsSpec struct {
	servers        []string
	image          ImageRef
	configFile     string
	env            []corev1.EnvVar
	envFrom        bool
	claimTemplate  string
	updateRevision string
}

// Precondition is step 1's reading of the render (§3a, "The precondition"): it reads the clustered
// hub's ConfigMap and StatefulSet through hub.Client, the two gets t-11's Role grants by name, and
// returns the render the hub's pods run once check finds nothing in it that admits a writer past
// the hold. A read that fails or a render that does not parse is UNKNOWN; a finding is a Refusal
// naming each finding.
func Precondition(ctx context.Context, hub KubeHub) (*Render, error) {
	r, err := readRender(ctx, hub)
	if err != nil {
		return nil, err
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return r, nil
}

// readRender reads the ConfigMap and the StatefulSet, and parses the render once for each declared
// server with the environment that pod's nats container declares (parsePod).
func readRender(ctx context.Context, hub KubeHub) (*Render, error) {
	cm, err := hub.Client.CoreV1().ConfigMaps(hub.Namespace).Get(ctx, hub.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, unknownf(StepPreflight, err, "read ConfigMap %s/%s", hub.Namespace, hub.ConfigMap)
	}
	set, err := hub.Client.AppsV1().StatefulSets(hub.Namespace).Get(ctx, hub.StatefulSet, metav1.GetOptions{})
	if err != nil {
		return nil, unknownf(StepPreflight, err, "read StatefulSet %s/%s", hub.Namespace, hub.StatefulSet)
	}
	spec, err := readNatsSpec(set, hub.ConfigMap)
	if err != nil {
		return nil, err
	}
	if len(cm.BinaryData) > 0 {
		return nil, unknownf(StepPreflight, nil, "ConfigMap %s/%s holds binaryData %v, which the reader does not read", hub.Namespace, hub.ConfigMap, sortedKeys(cm.BinaryData))
	}
	if _, ok := cm.Data[spec.configFile]; !ok {
		return nil, unknownf(StepPreflight, nil, "ConfigMap %s/%s holds no %s, the file the nats container starts with", hub.Namespace, hub.ConfigMap, spec.configFile)
	}
	r := &Render{
		Namespace: hub.Namespace, ConfigMapName: hub.ConfigMap, StatefulSetName: hub.StatefulSet,
		ResourceVersion: cm.ResourceVersion, Files: cm.Data, ConfigFile: spec.configFile,
		Servers: spec.servers, Image: spec.image, UpdateRevision: spec.updateRevision,
		ClaimTemplate: spec.claimTemplate,
		Digests:       map[string]string{}, Unresolved: map[string][]string{}, parsed: map[string]map[string]any{},
	}
	dir, err := os.MkdirTemp("", "writerhold-render-")
	if err != nil {
		return nil, unknownf(StepPreflight, err, "make a directory for the render's files")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, text := range cm.Data {
		// A ConfigMap key is [-._a-zA-Z0-9]+ (Kubernetes validation), so it is one file name; the
		// check stands in case an API server let another through.
		if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return nil, unknownf(StepPreflight, nil, "ConfigMap %s/%s holds the key %q, which is no file name", hub.Namespace, hub.ConfigMap, name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			return nil, unknownf(StepPreflight, err, "write the render's %s", name)
		}
	}
	for _, pod := range spec.servers {
		parsed, digest, unresolved, err := parsePod(dir, r, spec, pod)
		if err != nil {
			return nil, err
		}
		r.parsed[pod] = parsed
		if len(unresolved) > 0 {
			r.Unresolved[pod] = unresolved
		} else {
			r.Digests[pod] = digest
		}
	}
	return r, nil
}

// readNatsSpec reads how the StatefulSet starts nats-server: its pods, the pinned image, the one
// configuration file it starts with and the ConfigMap that file comes from, the container's
// environment, the one claim template and the update revision. Anything but the chart's shape is
// UNKNOWN, naming it: a command, another argument or a second configuration source would make the
// render read here something other than what the pods run.
func readNatsSpec(set *appsv1.StatefulSet, configMap string) (natsSpec, error) {
	where := set.Namespace + "/" + set.Name
	servers, err := statefulSetPods(set)
	if err != nil {
		return natsSpec{}, err
	}
	spec := natsSpec{updateRevision: set.Status.UpdateRevision, servers: servers}
	if spec.updateRevision == "" {
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s reports no status.updateRevision", where)
	}
	var c *corev1.Container
	for i := range set.Spec.Template.Spec.Containers {
		if set.Spec.Template.Spec.Containers[i].Name == natsContainer {
			c = &set.Spec.Template.Spec.Containers[i]
		}
	}
	if c == nil {
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s has no container %q", where, natsContainer)
	}
	image, err := parseImageRef(c.Image)
	if err != nil {
		return natsSpec{}, err
	}
	spec.image = image
	if len(c.Command) > 0 {
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s's nats container sets a command %q; the reader reads only the image's own entrypoint with --config", where, c.Command)
	}
	configPath, ok := configFlag(c.Args)
	if !ok || !path.IsAbs(configPath) || path.Clean(configPath) != configPath {
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s's nats container starts with arguments %q, not --config <absolute file> alone", where, c.Args)
	}
	dir, file := path.Split(configPath)
	dir = path.Clean(dir)
	var volume string
	for _, m := range c.VolumeMounts {
		if path.Clean(m.MountPath) == dir {
			if m.SubPath != "" || m.SubPathExpr != "" {
				return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s mounts %s from a subPath", where, dir)
			}
			volume = m.Name
		}
	}
	var source *corev1.ConfigMapVolumeSource
	for _, v := range set.Spec.Template.Spec.Volumes {
		if v.Name == volume {
			source = v.ConfigMap
		}
	}
	switch {
	case volume == "" || source == nil:
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s's nats container reads %s from no ConfigMap volume", where, configPath)
	case source.Name != configMap:
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s's nats container reads %s from ConfigMap %s, not %s", where, configPath, source.Name, configMap)
	case len(source.Items) > 0:
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s maps ConfigMap %s's keys to other paths (items)", where, configMap)
	}
	spec.configFile = file
	spec.env = c.Env
	spec.envFrom = len(c.EnvFrom) > 0
	if n := len(set.Spec.VolumeClaimTemplates); n != 1 {
		return natsSpec{}, unknownf(StepPreflight, nil, "StatefulSet %s has %d volume claim templates, not the one JetStream claim", where, n)
	}
	spec.claimTemplate = set.Spec.VolumeClaimTemplates[0].Name
	return spec, nil
}

// configFlag is the configuration file args name, when args are one configuration flag and its
// value alone, in a spelling nats-server's flag set takes (-c, -config, --config; server/opts.go
// ConfigureOptions).
func configFlag(args []string) (string, bool) {
	flags := []string{"-c", "-config", "--config"}
	switch len(args) {
	case 1:
		for _, f := range flags {
			if value, ok := strings.CutPrefix(args[0], f+"="); ok {
				return value, value != ""
			}
		}
	case 2:
		if slices.Contains(flags, args[0]) {
			return args[1], args[1] != ""
		}
	}
	return "", false
}

// envMu serialises parsePod: nats-server's conf package reads a configuration's variables from
// the process environment (conf/parse.go lookupVariable, os.LookupEnv), so a parse as one pod
// reads it sets that environment for the parse's length.
var envMu sync.Mutex

// missingVariable is the variable nats-server's conf parser could not find, from its error
// (v2.15.1 conf/parse.go:389-391); an included file's error wraps the same text.
var missingVariable = regexp.MustCompile(`variable reference for '([^']*)' on line [0-9]+ can not be found`)

// podEnv is one environment variable of the nats container as a pod sees it: its value when the
// reader can know it from the pod spec, else where it comes from.
type podEnv struct {
	value  string
	known  bool
	source string
}

// parsePod parses the render as pod's nats-server parses it: with conf.ParseFileWithChecks, the
// parse under nats-server's conf.ParseFileWithChecksDigest (server/opts.go:1077), over the files in
// dir, with each variable the render references set to the value pod's nats container gives it.
// It sets only the variables the parse asks for, each found from the parser's own "can not be
// found" error, and unsets every name the container declares for the parse's length, so no value of
// the operator's own process stands in for the pod's. A variable the container takes from a source
// the operator cannot read (a Secret) gets a placeholder and is returned in unresolved, with its
// source: the parse still checks the render around it, and its digest is then not the pod's. A
// variable the container does not declare, a parse that fails, or a value read from a file the
// ConfigMap does not hold is UNKNOWN.
func parsePod(dir string, r *Render, spec natsSpec, pod string) (map[string]any, string, []string, error) {
	env := resolvePodEnv(spec.env, pod, r.Namespace)
	envMu.Lock()
	defer envMu.Unlock()
	saved := map[string]*string{}
	for _, e := range spec.env {
		if _, done := saved[e.Name]; done {
			continue
		}
		saved[e.Name] = nil
		if value, ok := os.LookupEnv(e.Name); ok {
			saved[e.Name] = &value
		}
		_ = os.Unsetenv(e.Name)
	}
	defer func() {
		for name, value := range saved {
			if value == nil {
				_ = os.Unsetenv(name)
			} else {
				_ = os.Setenv(name, *value)
			}
		}
	}()
	file := filepath.Join(dir, r.ConfigFile)
	var unresolved []string
	set := map[string]bool{}
	for {
		m, err := conf.ParseFileWithChecks(file)
		if err == nil {
			parsed, err := plainRender(m, dir, r)
			if err != nil {
				return nil, "", nil, err
			}
			digest, err := configDigest(parsed)
			if err != nil {
				return nil, "", nil, unknownf(StepPreflight, err, "digest the render %s/%s as %s reads it", r.Namespace, r.ConfigMapName, pod)
			}
			sort.Strings(unresolved)
			return parsed, digest, unresolved, nil
		}
		match := missingVariable.FindStringSubmatch(err.Error())
		if match == nil {
			return nil, "", nil, unknownf(StepPreflight, err, "parse the render %s/%s as %s reads it", r.Namespace, r.ConfigMapName, pod)
		}
		name := match[1]
		value, declared := env[name]
		if !declared || set[name] {
			what := "which the nats container's env does not declare"
			if spec.envFrom {
				what += " (it also takes envFrom, whose names the reader does not list)"
			}
			return nil, "", nil, unknownf(StepPreflight, err, "the render %s/%s references $%s, %s", r.Namespace, r.ConfigMapName, name, what)
		}
		set[name] = true
		if value.known {
			_ = os.Setenv(name, value.value)
			continue
		}
		_ = os.Setenv(name, `"writerhold-unresolved-`+name+`"`)
		unresolved = append(unresolved, "$"+name+" ("+value.source+")")
	}
}

// configDigest is the digest nats-server 2.15.1 reports as /varz config_digest for a configuration
// whose parse is parsed: conf's configDigest (v2.15.1 conf/parse.go:117-125), the sha256 of
// json.NewEncoder's Encode of the parse, trailing newline included, formatted "sha256:%x". It is
// computed here because conf.ParseFileWithChecksDigest is 2.15.1's alone, and writerhold also builds
// against nats-server 2.10.29 (scripts/forced-order.sh -server 2.10.29), whose conf has no digest.
// conf's own encodes the pedantic parse, whose every value is a token, and a token encodes as its
// value (token.MarshalJSON, parse.go:148-150), so the same encoding of the unwrapped parse
// (plainValue) gives the same bytes. 2.10.29's token has no MarshalJSON, so encoding its tokens
// there would give {} for each value; the unwrapped parse gives the 2.15.1 digest under either
// version. The hub runs 2.15.1, and 2.10.29's /varz reports no config_digest.
// TestHoldTheConfigurationBinding measures it against a running server's /varz.
func configDigest(parsed map[string]any) (string, error) {
	digest := sha256.New()
	if err := json.NewEncoder(digest).Encode(parsed); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

// resolvePodEnv is the nats container's environment as pod sees it, in the container's order:
// a literal value with Kubernetes' $(NAME) references to earlier entries expanded, and the
// fieldRefs metadata.name and metadata.namespace, are known; every other source is named, and so
// is a value that expands an entry of such a source. A later entry of a name replaces an earlier,
// as the kubelet does.
func resolvePodEnv(env []corev1.EnvVar, pod, namespace string) map[string]podEnv {
	out := map[string]podEnv{}
	for _, e := range env {
		switch from := e.ValueFrom; {
		case from == nil:
			out[e.Name] = expandEnv(e.Value, out)
		case from.FieldRef != nil && from.FieldRef.FieldPath == "metadata.name":
			out[e.Name] = podEnv{value: pod, known: true}
		case from.FieldRef != nil && from.FieldRef.FieldPath == "metadata.namespace":
			out[e.Name] = podEnv{value: namespace, known: true}
		case from.FieldRef != nil:
			out[e.Name] = podEnv{source: "fieldRef " + from.FieldRef.FieldPath}
		case from.SecretKeyRef != nil:
			out[e.Name] = podEnv{source: "secretKeyRef " + from.SecretKeyRef.Name + "/" + from.SecretKeyRef.Key}
		case from.ConfigMapKeyRef != nil:
			out[e.Name] = podEnv{source: "configMapKeyRef " + from.ConfigMapKeyRef.Name + "/" + from.ConfigMapKeyRef.Key}
		case from.ResourceFieldRef != nil:
			out[e.Name] = podEnv{source: "resourceFieldRef " + from.ResourceFieldRef.Resource}
		default:
			out[e.Name] = podEnv{source: "a valueFrom the reader does not read"}
		}
	}
	return out
}

// expandEnv expands value as the kubelet expands an env value (Kubernetes'
// third_party/forked/golang/expansion): $$ is a literal $, $(NAME) is NAME's value when an earlier
// entry declares it and stays as written otherwise, and any other $ is literal. A reference to an
// entry whose value the reader cannot know makes this one unknown too, naming it.
func expandEnv(value string, earlier map[string]podEnv) podEnv {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '$' || i+1 == len(value) {
			b.WriteByte(value[i])
			continue
		}
		switch next := value[i+1]; {
		case next == '$':
			b.WriteByte('$')
			i++
		case next == '(':
			end := strings.IndexByte(value[i+2:], ')')
			if end < 0 {
				b.WriteString(value[i:])
				return podEnv{value: b.String(), known: true}
			}
			name := value[i+2 : i+2+end]
			ref, ok := earlier[name]
			switch {
			case !ok:
				b.WriteString("$(" + name + ")")
			case !ref.known:
				return podEnv{source: "$(" + name + ") of " + ref.source}
			default:
				b.WriteString(ref.value)
			}
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return podEnv{value: b.String(), known: true}
}

// confToken is a value of nats-server's pedantic parse (conf.token, unexported), which carries the
// file it was read from.
type confToken interface {
	Value() any
	SourceFile() string
}

// plainRender is the parse m with its tokens unwrapped, once every value it holds was read from a
// file of the ConfigMap: an include that reaches outside the configuration directory reads, in the
// pod, a file of the image the reader cannot see, so it is UNKNOWN, naming the file.
func plainRender(m map[string]any, dir string, r *Render) (map[string]any, error) {
	allowed := map[string]bool{}
	for name := range r.Files {
		allowed[filepath.Join(dir, name)] = true
	}
	read := map[string]bool{}
	out := plainValue(m, read).(map[string]any)
	for file := range read {
		if !allowed[file] {
			rel, err := filepath.Rel(dir, file)
			if err != nil {
				rel = file
			}
			return nil, unknownf(StepPreflight, nil, "the render %s/%s includes %s, which is not a file of the ConfigMap", r.Namespace, r.ConfigMapName, rel)
		}
	}
	return out, nil
}

func plainValue(v any, read map[string]bool) any {
	if tk, ok := v.(confToken); ok {
		if tk.SourceFile() != "" {
			read[tk.SourceFile()] = true
		}
		v = tk.Value()
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainValue(e, read)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainValue(e, read)
		}
		return out
	default:
		return x
	}
}

// check is the precondition on the render (§3a, "The precondition", and step 1's "No static
// envoy-move user" and "The render"): for each server's parse, the render is $G alone, with
// auth_callout and the auth service's own user, so the hold's callout is the one door. It refuses,
// naming each finding: an accounts block or an account source outside $G (an operator or a
// resolver), a subject mapping, an import or an export, a gateway block, a leafnodes block, a
// cluster block without route authorization, no auth_callout or more than one authorization
// block, the static envoy-move user, any other static user or credential beside the auth service's
// own (the one user auth_callout's auth_users lists), an auth service user granted more than
// subscribe $SYS.REQ.USER.AUTH and publish under $SYS._INBOX. (core.serviceGrantRefusal's rule),
// and declared servers whose renders name different auth service users. Once nothing is found it
// sets r.AuthUser to the one auth service user every server's render names.
func (r *Render) check() error {
	var findings []string
	seen := map[string]bool{}
	// services is each declared server's auth service user, in r.Servers' order.
	var services []string
	for _, pod := range r.Servers {
		found, service := renderFindings(r.parsed[pod], r.Files)
		for _, f := range found {
			if !seen[f] {
				seen[f] = true
				findings = append(findings, f)
			}
		}
		services = append(services, service)
	}
	if len(findings) == 0 {
		// Each server's render passed with one auth service user. A variable of the pod spec can
		// make it differ between servers, and then no one value is the auth service's.
		distinct := slices.Compact(slices.Sorted(slices.Values(services)))
		if len(distinct) == 1 {
			r.AuthUser = distinct[0]
			return nil
		}
		findings = append(findings, "the declared servers' renders name different auth service users: "+strings.Join(distinct, ", "))
	}
	return refusef(StepPreflight, "the render %s/%s: %s", r.Namespace, r.ConfigMapName, strings.Join(findings, "; "))
}

// The top-level keys nats-server reads as accounts from outside the configuration: operator mode's
// JWT accounts and their resolvers (server/opts.go processConfigFileLine, v2.15.1).
var accountSourceKeys = []string{
	"operator", "operators", "roots", "root", "root_operators", "root_operator",
	"resolver", "account_resolver", "accounts_resolver", "resolver_preload", "trusted", "trusted_keys",
}

// renderFindings is every finding of check in one parse m, keys matched without case as
// nats-server matches them (strings.ToLower in each parse function of server/opts.go), and the auth
// service user that parse's one authorization block names ("" with a finding).
func renderFindings(m map[string]any, files map[string]string) ([]string, string) {
	var f []string
	for _, k := range keysOf(m, "accounts") {
		f = append(f, accountFindings(k, m[k], files)...)
	}
	for _, k := range keysOf(m, "mappings", "maps") {
		f = append(f, fmt.Sprintf("a subject mapping block (%s: %s)", k, describeKeys(m[k])))
	}
	for _, k := range keysOf(m, accountSourceKeys...) {
		f = append(f, fmt.Sprintf("an account source outside $G (%s)", k))
	}
	for _, k := range keysOf(m, "gateway") {
		f = append(f, fmt.Sprintf("a gateway block (%s)", k))
	}
	for _, k := range keysOf(m, "leaf", "leafnodes") {
		f = append(f, fmt.Sprintf("a leafnodes block (%s)", k))
	}
	for _, k := range keysOf(m, "cluster") {
		if !routeAuthorized(m[k]) {
			f = append(f, fmt.Sprintf("a cluster block without route authorization (%s.authorization with a user and a password)", k))
		}
	}
	for _, k := range keysOf(m, "websocket", "ws", "mqtt") {
		f = append(f, listenerFindings(k, m[k])...)
	}
	for _, k := range keysOf(m, "no_auth_user") {
		f = append(f, fmt.Sprintf("no_auth_user %v: a client presenting no credential becomes a static user", m[k]))
	}
	auth := keysOf(m, "authorization")
	switch len(auth) {
	case 0:
		f = append(f, "no authorization block, so no auth_callout")
	case 1:
	default:
		// nats-server applies each in its map's order, so which one is in force is not known.
		f = append(f, fmt.Sprintf("more than one authorization block (%s)", strings.Join(auth, ", ")))
	}
	var service string
	for _, k := range auth {
		found, s := authorizationFindings(k, m[k], files)
		f = append(f, found...)
		service = s
	}
	return f, service
}

// keysOf are the keys of v, when it is a block, that equal one of names without case, sorted.
func keysOf(v any, names ...string) []string {
	m, _ := v.(map[string]any)
	var out []string
	for k := range m {
		if slices.Contains(names, strings.ToLower(k)) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// value is the first of v's keys equal to one of names without case.
func value(v any, names ...string) (any, bool) {
	keys := keysOf(v, names...)
	if len(keys) == 0 {
		return nil, false
	}
	return v.(map[string]any)[keys[0]], true
}

func describeKeys(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return strings.Join(sortedKeys(m), ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// accountFindings names an accounts block, each account, and what each account declares that
// moves a subject or admits a client: its imports, its exports, its mappings and its users.
func accountFindings(key string, v any, files map[string]string) []string {
	var names []string
	accounts := map[string]any{}
	switch x := v.(type) {
	case map[string]any:
		names, accounts = sortedKeys(x), x
	case []any:
		for _, a := range x {
			names = append(names, fmt.Sprintf("%v", a))
		}
	}
	f := []string{fmt.Sprintf("an accounts block (%s: %s)", key, strings.Join(names, ", "))}
	for _, name := range sortedKeys(accounts) {
		account := accounts[name]
		for _, k := range keysOf(account, "imports") {
			f = append(f, fmt.Sprintf("account %s's imports (%s)", name, k))
		}
		for _, k := range keysOf(account, "exports") {
			f = append(f, fmt.Sprintf("account %s's exports (%s)", name, k))
		}
		for _, k := range keysOf(account, "mappings", "maps") {
			f = append(f, fmt.Sprintf("account %s's subject mappings (%s: %s)", name, k, describeKeys(account.(map[string]any)[k])))
		}
		for _, k := range keysOf(account, "users") {
			for _, u := range staticUsers(account.(map[string]any)[k], files) {
				f = append(f, fmt.Sprintf("static user %s in account %s", u.label, name))
			}
		}
	}
	return f
}

// routeAuthorized reports whether a cluster block authorizes its routes: an authorization block
// with a user and a password (nats-server's parseCluster takes no token and no users there).
func routeAuthorized(v any) bool {
	auth, ok := value(v, "authorization")
	if !ok {
		return false
	}
	user, _ := value(auth, "user", "username")
	pass, _ := value(auth, "pass", "password")
	return nonEmptyString(user) && nonEmptyString(pass)
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// listenerFindings names a websocket or MQTT listener's own credential: its authorization's user
// or token, which admits a client past the callout, and its no_auth_user.
func listenerFindings(key string, v any) []string {
	var f []string
	for _, a := range keysOf(v, "authorization", "authentication") {
		auth := v.(map[string]any)[a]
		for _, k := range keysOf(auth, "user", "username", "token", "users") {
			f = append(f, fmt.Sprintf("a static credential in %s.%s (%s)", key, a, k))
		}
	}
	for _, k := range keysOf(v, "no_auth_user") {
		f = append(f, fmt.Sprintf("%s.%s %v: a client presenting no credential becomes a static user", key, k, v.(map[string]any)[k]))
	}
	return f
}

// staticUser is one entry of a users list: its nkey or its username, how a finding names it, and
// its permissions when it has its own.
type staticUser struct {
	nkey, username, label string
	permissions           any
	hasPermissions        bool
}

// staticUsers are the entries of a users list, each named by its username, or else by the comment
// line core.Authorization's render writes before it (infra/core/nats_auth.go:311), or else by its
// nkey.
func staticUsers(v any, files map[string]string) []staticUser {
	list, _ := v.([]any)
	var out []staticUser
	for i, e := range list {
		u := staticUser{label: fmt.Sprintf("users[%d]", i)}
		if n, ok := value(e, "nkey"); ok {
			u.nkey, _ = n.(string)
		}
		if n, ok := value(e, "user", "username"); ok {
			u.username, _ = n.(string)
		}
		u.permissions, u.hasPermissions = value(e, "permission", "permissions", "authorization")
		switch {
		case u.username != "":
			u.label = u.username
		case u.nkey != "":
			u.label = u.nkey
			if name := commentName(files, u.nkey); name != "" {
				u.label = name + " (" + u.nkey + ")"
			}
		}
		out = append(out, u)
	}
	return out
}

// commentName is the comment on the line before the line that holds key in one of files, which is
// where core.Authorization's render names each keyed user, or "" when there is none.
func commentName(files map[string]string, key string) string {
	for _, name := range sortedKeys(files) {
		lines := strings.Split(files[name], "\n")
		for i, line := range lines {
			if i > 0 && strings.Contains(line, key) {
				if comment, ok := strings.CutPrefix(strings.TrimSpace(lines[i-1]), "#"); ok {
					return strings.TrimSpace(comment)
				}
			}
		}
	}
	return ""
}

// moveUser is the deployment repository's static move user (envoy-cluster/hub.go hubMovePublish), which holds
// $JS.API.STREAM.UPDATE.* and $JS.API.STREAM.DELETE.* and connects past the callout; INFRA-4094's
// pull request B removes it in the callout cutover.
const moveUser = "envoy-move"

// The auth service's grant as nats-server defines it (core's calloutRequestSubject and
// calloutReplyPrefix, infra/core/nats_callout.go:84-87).
const (
	calloutRequestSubject = "$SYS.REQ.USER.AUTH"
	calloutReplyPrefix    = "$SYS._INBOX."
)

// authorizationFindings names, in the top-level authorization block key: no auth_callout, a
// callout that places clients outside $G, a single user or a token, every static user but the
// auth service's own, and an auth service grant wider than the callout's. It returns too the auth
// service's user, the one entry auth_callout's auth_users lists, once it names a user of the block,
// and "" otherwise.
func authorizationFindings(key string, v any, files map[string]string) ([]string, string) {
	var f []string
	if _, ok := v.(map[string]any); !ok {
		return []string{fmt.Sprintf("%s is not a block, so no auth_callout", key)}, ""
	}
	for _, k := range keysOf(v, "user", "username") {
		f = append(f, fmt.Sprintf("a static user %v in %s.%s", v.(map[string]any)[k], key, k))
	}
	for _, k := range keysOf(v, "token") {
		f = append(f, fmt.Sprintf("a static token in %s.%s", key, k))
	}
	callout, ok := value(v, "auth_callout", "auth_hook")
	var authUsers []string
	if !ok {
		f = append(f, fmt.Sprintf("no auth_callout in %s", key))
	} else {
		if account, ok := value(callout, "account", "acc"); ok && account != "$G" {
			f = append(f, fmt.Sprintf("auth_callout places its clients in account %v, not $G", account))
		}
		if allowed, ok := value(callout, "allowed_accounts"); ok {
			if list, _ := allowed.([]any); len(list) > 0 {
				f = append(f, fmt.Sprintf("auth_callout's allowed_accounts %v, accounts beside $G", list))
			}
		}
		listed, _ := value(callout, "auth_users", "users")
		for _, u := range asList(listed) {
			authUsers = append(authUsers, fmt.Sprintf("%v", u))
		}
	}
	listed, _ := value(v, "users")
	users := staticUsers(listed, files)
	service := -1
	if len(authUsers) == 1 {
		for i, u := range users {
			if (u.nkey != "" && u.nkey == authUsers[0]) || (u.username != "" && u.username == authUsers[0]) {
				service = i
			}
		}
		if service < 0 {
			f = append(f, fmt.Sprintf("auth_callout's auth_users names %s, which is no user of %s", authUsers[0], key))
		}
	} else if ok {
		var labels []string
		for _, a := range authUsers {
			label := a
			if name := commentName(files, a); name != "" {
				label = name + " (" + a + ")"
			}
			labels = append(labels, label)
		}
		f = append(f, fmt.Sprintf("auth_callout's auth_users lists %d users (%s), where the auth service's own is the one static user", len(authUsers), strings.Join(labels, ", ")))
	}
	defaults, hasDefaults := value(v, "default_permission", "default_permissions", "permissions")
	for i, u := range users {
		switch {
		case i == service:
			perms, has := u.permissions, u.hasPermissions
			if !has {
				perms, has = defaults, hasDefaults
			}
			f = append(f, serviceGrantFindings(u.label, perms, has)...)
		case strings.HasPrefix(u.label, moveUser+" (") || u.label == moveUser:
			f = append(f, fmt.Sprintf("the static %s user, which connects past the callout", u.label))
		default:
			f = append(f, fmt.Sprintf("static user %s beside the auth service's own", u.label))
		}
	}
	if service < 0 {
		return f, ""
	}
	return f, authUsers[0]
}

// asList is v as a list: a list, or one value standing alone as nats-server's permission parse
// takes it (parsePermSubjects).
func asList(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	default:
		return []any{x}
	}
}

// serviceGrantFindings names each way the auth service's user, label, is granted more than the
// callout needs: nats-server reads no permissions, or a direction with no allow list, as everything
// (server/client.go setPermissions), allow_responses lets it answer any request it receives, and an
// allow entry must be under $SYS._INBOX. to publish, or $SYS.REQ.USER.AUTH (with any queue group)
// to subscribe. A deny only narrows a grant, so none is read.
func serviceGrantFindings(label string, perms any, has bool) []string {
	if !has {
		return []string{fmt.Sprintf("the auth service's user %s has no permissions, so it may publish and subscribe to every subject", label)}
	}
	m, ok := perms.(map[string]any)
	if !ok {
		return []string{fmt.Sprintf("the auth service's user %s has permissions that are not a block", label)}
	}
	var f []string
	publish := keysOf(m, "pub", "publish", "import")
	subscribe := keysOf(m, "sub", "subscribe", "export")
	for _, k := range keysOf(m, "publish_allow_responses", "allow_responses") {
		f = append(f, fmt.Sprintf("the auth service's user %s is granted %s", label, k))
	}
	for _, k := range sortedKeys(m) {
		if !slices.Contains(publish, k) && !slices.Contains(subscribe, k) && !slices.Contains(keysOf(m, "publish_allow_responses", "allow_responses"), k) {
			f = append(f, fmt.Sprintf("the auth service's user %s has a permission %s the hold does not read", label, k))
		}
	}
	if len(publish) == 0 {
		f = append(f, fmt.Sprintf("the auth service's user %s may publish to every subject", label))
	}
	if len(subscribe) == 0 {
		f = append(f, fmt.Sprintf("the auth service's user %s may subscribe to every subject", label))
	}
	for _, k := range publish {
		f = append(f, directionFindings(label, k, m[k], func(s string) bool {
			return strings.HasPrefix(s, calloutReplyPrefix) && len(s) > len(calloutReplyPrefix)
		})...)
	}
	for _, k := range subscribe {
		f = append(f, directionFindings(label, k, m[k], func(s string) bool {
			subject, _, _ := strings.Cut(s, " ")
			return subject == calloutRequestSubject
		})...)
	}
	return f
}

// directionFindings names each allow entry of one direction's rules that fits is not, and an
// allow list that is empty or absent, which nats-server reads as every subject.
func directionFindings(label, key string, v any, fits func(string) bool) []string {
	allow := v
	if m, ok := v.(map[string]any); ok {
		allow, _ = value(m, "allow")
	}
	entries := asList(allow)
	if len(entries) == 0 {
		return []string{fmt.Sprintf("the auth service's user %s has no %s allow list, so it may %s every subject not denied", label, key, key)}
	}
	var f []string
	for _, e := range entries {
		if s, ok := e.(string); !ok || !fits(s) {
			f = append(f, fmt.Sprintf("the auth service's user %s is granted %s %v, beyond the callout's subscribe %s and publish under %s", label, key, e, calloutRequestSubject, calloutReplyPrefix))
		}
	}
	return f
}
