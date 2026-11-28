package main

// cmd/dispatch/memory_profile_test.go:147-148 reads three documents only when
// DISPATCH_EDIT_PROFILE_OUT is set (line 125), which CI never sets: not a CI read.
//ci:reads packages/envoy/infra/gcp/testdata/dispatch-pod.golden TestDispatchPodGracePeriodCoversStopGrace
//ci:reads packages/envoy/infra/gcp/dispatch.go TestDispatchPodGracePeriodCoversStopGrace parses it (infraStopGraceRead) for the infra module's copy of stopGrace

import (
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strconv"
	"testing"
	"time"
)

// dispatchPodGolden is the pod that envoy-infra's gcp:Dispatch component renders for the app
// Deployment with preStopSeconds and terminationGracePeriodSeconds unset, recorded byte for byte,
// read from this package's directory; dispatchPodGoldenPath names the same file from the
// repository root, for a failure read in CI. The component lives in packages/envoy/infra, which is
// a Go module of its own, so this test reads what it renders rather than calling it.
const (
	dispatchPodGolden     = "../../infra/gcp/testdata/dispatch-pod.golden"
	dispatchPodGoldenPath = "packages/envoy/infra/gcp/testdata/dispatch-pod.golden"
)

// Where the infra module writes stopGrace again: infraStopGraceRead is the path from this
// package's directory, infraStopGracePath names the same file from the repository root for a
// failure read in CI, and infraStopGraceName is the constant in it, the seconds gcp:Dispatch adds
// to preStopSeconds for the least terminationGracePeriodSeconds it takes.
const (
	infraStopGraceRead = "../../infra/gcp/dispatch.go"
	infraStopGracePath = "packages/envoy/infra/gcp/dispatch.go"
	infraStopGraceName = "dispatchStopGraceSeconds"
)

// dispatchPodContainer is the container of that pod which runs this command.
const dispatchPodContainer = "envoy-dispatch"

// dispatchPod is as much of the rendered pod as this test reads. Every pointer is nil when the
// golden does not carry that field, which is a failure here rather than a zero: the shape this
// test reads is the shape the component is required to render.
type dispatchPod struct {
	Spec *struct {
		TerminationGracePeriodSeconds *int `json:"terminationGracePeriodSeconds"`
		Containers                    []struct {
			Name      string `json:"name"`
			Lifecycle *struct {
				PreStop *struct {
					Exec *struct {
						Command []string `json:"command"`
					} `json:"exec"`
				} `json:"preStop"`
			} `json:"lifecycle"`
		} `json:"containers"`
	} `json:"spec"`
}

// TestDispatchPodGracePeriodCoversStopGrace holds the Kubernetes side of stopGrace, which nothing
// else does. The pod that gcp:Dispatch renders is one of the runtimes that grant these seconds: its
// preStop wait and its grace period are that component's inputs preStopSeconds and
// terminationGracePeriodSeconds, and the component refuses a grace period under the wait plus
// dispatchStopGraceSeconds, its own copy of stopGrace, since it lives in another Go module, which
// cannot import this package. So this test holds two things: the copy is stopGrace, in whole
// seconds, which makes that refusal cover every pod the component renders; and the pod it renders
// with both inputs unset, which its golden records, leaves stopGrace whole. Raising stopGrace here
// breaks neither over there, and this test is what tells whoever raises it that the copy and the
// default grace period have to be raised with it.
//
// The kubelet runs the pre-stop hook and the server's shutdown inside the one
// terminationGracePeriodSeconds - it sends SIGTERM only once the hook returns - so the seconds
// left for the shutdown are the grace period less the hook's sleep, and they must cover stopGrace.
//
// The copy is read by parsing the Go source and evaluating the constant's expression, as the
// listener's TestTheInfraCopyOfTheStopBudgetIsTheStopBudget reads its own: a file that cannot be
// parsed, no constant of that name, and an expression that is no integer constant each fail by
// name, since each is this check losing its other side.
func TestDispatchPodGracePeriodCoversStopGrace(t *testing.T) {
	t.Parallel()
	if stopGrace%time.Second != 0 {
		t.Fatalf("stopGrace (packages/envoy/cmd/dispatch/shutdown.go) is %s, which is not whole seconds, while %s states it in seconds as %s: a shutdown the infra module cannot state is one its pods cannot grant.",
			stopGrace, infraStopGracePath, infraStopGraceName)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, infraStopGraceRead, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s, which holds gcp:Dispatch's copy of stopGrace as %s: %v", infraStopGracePath, infraStopGraceName, err)
	}
	value := infraDispatchConstExpr(file, infraStopGraceName)
	if value == nil {
		t.Fatalf("%s declares no constant %s: gcp:Dispatch's copy of stopGrace (%s) was renamed or removed, and nothing holds the least grace period the component takes to the shutdown this server spends. Whoever moved it points this check at the name it has now.",
			infraStopGracePath, infraStopGraceName, stopGrace)
	}
	// Evaluated in the universe scope, so a copy that is still a constant expression of literals is
	// read whatever its spelling, and one that names something of its own package is not silently
	// skipped: Eval reports it here.
	expr := types.ExprString(value)
	evaluated, err := types.Eval(fset, nil, token.NoPos, expr)
	if err != nil || evaluated.Value == nil || evaluated.Value.Kind() != constant.Int {
		t.Fatalf("%s declares %s = %s, which this check cannot evaluate as an integer constant (%v): the copy has to stay a constant expression of literals for anything to hold it to stopGrace, which is %s.",
			infraStopGracePath, infraStopGraceName, expr, err, stopGrace)
	}
	copied, exact := constant.Int64Val(evaluated.Value)
	if want := int64(stopGrace / time.Second); !exact || copied != want {
		t.Fatalf("%s declares %s = %s, where stopGrace (packages/envoy/cmd/dispatch/shutdown.go) is %s, %d seconds: the component would take a grace period that leaves this server's shutdown short, or refuse one that covers it. This server owns the number; whoever moves stopGrace moves the copy with it.",
			infraStopGracePath, infraStopGraceName, expr, stopGrace, want)
	}

	golden, err := os.ReadFile(dispatchPodGolden)
	if err != nil {
		t.Fatalf("read %s, the pod gcp:Dispatch renders: %v", dispatchPodGoldenPath, err)
	}
	var pod dispatchPod
	if err := json.Unmarshal(golden, &pod); err != nil {
		t.Fatalf("parse %s as the pod JSON it records: %v", dispatchPodGoldenPath, err)
	}
	if pod.Spec == nil || pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatalf("%s carries no spec.terminationGracePeriodSeconds; this test cannot tell whether the pod covers stopGrace", dispatchPodGoldenPath)
	}
	grace := *pod.Spec.TerminationGracePeriodSeconds

	var command []string
	found := false
	for _, container := range pod.Spec.Containers {
		if container.Name != dispatchPodContainer {
			continue
		}
		found = true
		if container.Lifecycle == nil || container.Lifecycle.PreStop == nil || container.Lifecycle.PreStop.Exec == nil {
			t.Fatalf("container %s in %s has no lifecycle.preStop.exec; this test cannot tell how much of the grace period the kubelet spends before SIGTERM", dispatchPodContainer, dispatchPodGoldenPath)
		}
		command = container.Lifecycle.PreStop.Exec.Command
	}
	if !found {
		t.Fatalf("%s has no container named %s; this test cannot tell which container's shutdown the grace period has to cover", dispatchPodGoldenPath, dispatchPodContainer)
	}
	if len(command) != 2 || command[0] != "sleep" {
		t.Fatalf("the preStop hook of %s in %s is %q, not a sleep of some seconds; this test reads the wait out of a sleep", dispatchPodContainer, dispatchPodGoldenPath, command)
	}
	wait, err := strconv.Atoi(command[1])
	if err != nil {
		t.Fatalf("the preStop sleep of %s in %s waits %q, which is no whole number of seconds: %v", dispatchPodContainer, dispatchPodGoldenPath, command[1], err)
	}

	if time.Duration(grace-wait)*time.Second < stopGrace {
		t.Fatalf("%s asks for %d seconds of termination grace and spends %d of them on %s's pre-stop sleep, leaving its shutdown %d seconds where stopGrace in packages/envoy/cmd/dispatch/shutdown.go builds it for %s: the pod asks for less than the server's shutdown needs, so the kubelet kills %s mid-shutdown, past its document service's five-second reserve, with a write still waiting on a lock killed mid-commit. Whoever moves one of the two moves the other.",
			dispatchPodGoldenPath, grace, wait, dispatchPodContainer, grace-wait, stopGrace, dispatchPodContainer)
	}
}

// infraDispatchConstExpr returns the expression the constant named name is declared with in file,
// or nil when the file declares no constant of that name with a value of its own, as
// cmd/listener's infraConstExpr does for the listener's copy: a spec that repeats the line above
// it, as an iota run does, carries no expression to evaluate, so it reads as the name being absent.
func infraDispatchConstExpr(file *ast.File, name string) ast.Expr {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range valueSpec.Names {
				if ident.Name == name && i < len(valueSpec.Values) {
					return valueSpec.Values[i]
				}
			}
		}
	}
	return nil
}
