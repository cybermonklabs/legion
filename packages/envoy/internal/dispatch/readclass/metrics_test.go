package readclass

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
	"testing"
)

// metricsPath is the package Dispatch's metric registry is built from.
const metricsPath = "github.com/sjawhar/envoy/internal/metrics"

// namingLabels are the label names that would name what a viewer may not see.
var namingLabels = []string{"project", "issue", "key", "login", "session"}

// TestMetricsNameNoProject: no metric family of the registry GET /metrics serves carries a label
// named project, issue, key, login or session, which is what lets /metrics be a read of the
// deployment (readclass.Deployment) and no project's. A label of this registry is named where a
// series is written: a call of internal/metrics that takes labels (Counter.Inc and Add,
// Histogram.Observe, Timer.ObserveDuration) and a GaugeSample's Labels. The test reads every such
// site in the server's source (every package of this module cmd/dispatch builds in, and every
// package under internal/dispatch) and each label's name, which must be a constant: a label it
// cannot read the name of fails it, named, rather than passing unread.
//
// The walk's controls run every time: it must read the two labels the outbox's families carry
// today, and over testdata/metriclabels, a tree no build reads, it must name that tree's project
// label.
func TestMetricsNameNoProject(t *testing.T) {
	source := loadedServerSource(t)
	var read []string
	for _, pkg := range source.packages {
		// internal/metrics names no label of its own: it passes on the labels its callers give it.
		if pkg.path == metricsPath {
			continue
		}
		for _, label := range metricLabels(source, pkg) {
			read = append(read, label.name)
			if label.problem != "" {
				t.Errorf("%s: %s", label.position, label.problem)
			}
		}
	}
	for _, want := range []string{"reason", "holder_state"} {
		if !slices.Contains(read, want) {
			t.Fatalf("the walk did not read the label %q the outbox's families carry (it read %q): it is blind", want, read)
		}
	}

	t.Run("a project label in the fixture tree fails the walk", func(t *testing.T) {
		var problems []string
		for _, label := range metricLabels(source, source.fixture(t, "metriclabels")) {
			if label.problem != "" {
				problems = append(problems, label.problem)
			}
		}
		if want := []string{`a series labelled "project", which names what a viewer may not see`}; !slices.Equal(problems, want) {
			t.Fatalf("the walk over testdata/metriclabels answered %q, want %q", problems, want)
		}
	})
}

// metricLabel is one label a series is written with: its name, where, and what is wrong with it.
type metricLabel struct {
	position, name, problem string
}

// metricLabels reads every label pkg writes a series of internal/metrics with.
func metricLabels(source *serverSource, pkg *checkedPackage) []metricLabel {
	var labels []metricLabel
	read := func(node ast.Node, pair ast.Expr) {
		label := metricLabel{position: source.position(node)}
		literal, ok := ast.Unparen(pair).(*ast.CompositeLit)
		if ok && len(literal.Elts) == 2 {
			if name := pkg.info.Types[literal.Elts[0]].Value; name != nil && name.Kind() == constant.String {
				label.name = constant.StringVal(name)
			}
		}
		switch {
		case label.name == "":
			label.problem = fmt.Sprintf("a series labelled %s, whose name the walk cannot read: write the label as a [2]string literal with a constant name", types.ExprString(pair))
		case slices.Contains(namingLabels, label.name):
			label.problem = fmt.Sprintf("a series labelled %q, which names what a viewer may not see", label.name)
		}
		labels = append(labels, label)
	}
	for _, file := range pkg.files {
		functionsOf(file, func(_ string, node ast.Node) {
			switch node := node.(type) {
			case *ast.CallExpr:
				called := pkg.calledFunc(node)
				if called == nil || called.Pkg() == nil || called.Pkg().Path() != metricsPath || !called.Signature().Variadic() {
					return
				}
				params := called.Signature().Params()
				if !isLabels(params.At(params.Len() - 1).Type()) {
					return
				}
				first := params.Len() - 1
				if node.Ellipsis.IsValid() {
					labels = append(labels, metricLabel{position: source.position(node),
						problem: "a series labelled by a spread slice, whose names the walk cannot read: pass each label as a [2]string literal"})
					return
				}
				for _, pair := range node.Args[min(first, len(node.Args)):] {
					read(node, pair)
				}
			case *ast.CompositeLit:
				named, ok := types.Unalias(pkg.info.Types[node].Type).(*types.Named)
				if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != metricsPath || named.Obj().Name() != "GaugeSample" {
					return
				}
				for _, element := range node.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok || types.ExprString(field.Key) != "Labels" {
						continue
					}
					pairs, ok := ast.Unparen(field.Value).(*ast.CompositeLit)
					if !ok {
						labels = append(labels, metricLabel{position: source.position(field),
							problem: "a gauge sample labelled by a value, whose names the walk cannot read: write its Labels as a literal"})
						continue
					}
					for _, pair := range pairs.Elts {
						read(field, pair)
					}
				}
			}
		})
	}
	return labels
}

// isLabels reports whether typ is []([2]string), the type a label set is passed as.
func isLabels(typ types.Type) bool {
	slice, ok := typ.(*types.Slice)
	if !ok {
		return false
	}
	array, ok := slice.Elem().(*types.Array)
	if !ok || array.Len() != 2 {
		return false
	}
	basic, ok := array.Elem().(*types.Basic)
	return ok && basic.Kind() == types.String
}
