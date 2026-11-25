// Package metriclabels is TestMetricsNameNoProject's fixture: one series labelled with a project,
// which the walk must name, and one labelled with an outcome, which it must pass.
package metriclabels

import "github.com/sjawhar/envoy/internal/metrics"

func count(registry *metrics.Registry, project string) {
	reads := registry.NewCounter("fixture_reads_total", "Reads, by project and outcome.")
	reads.Inc([2]string{"project", project})
	reads.Inc([2]string{"outcome", "served"})
}
