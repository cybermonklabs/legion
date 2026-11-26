//go:build natslive

package gcp_test

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The stream and the durable consumer the exporter test's server holds. Each series the exporter
// serves for a consumer carries its name and its description, as consumer_name and consumer_desc
// (prometheus-nats-exporter 0.20.2, collector/jsz.go:111-115), as each daemon's consumer on the
// hub carries its daemon's name.
const (
	exporterTestStream      = "LIVE_LIMITED"
	exporterTestStreamBytes = 1 << 20
	exporterTestConsumer    = "live-daemon-consumer"
	exporterTestDescription = "the live daemon's \"consumer\"\\description\n"
)

// The hub's scrape keeps no series that names a consumer, and keeps every other series, on every
// render: the exporter runs the chart's -jsz=all, which serves a series of each JetStream consumer
// named by it, and the hub's PodMonitoring drops those at the scrape (the PodMonitoring in
// clusteredhub_kube.go has why the exporter keeps -jsz=all).
//
// For the hub with privateProjects and without, the test runs the exporter with the arguments the
// rendered chart gives it against a nats-server holding a stream with a byte limit and a durable
// consumer with a description, and applies the PodMonitoring's metricRelabeling, as the hub
// registers it, to every series one scrape serves.
func TestTheHubsScrapeKeepsNoConsumerSeriesOnEveryRender(t *testing.T) {
	exporter := installTools(t, t.TempDir(), liveTool{exporterTool, "prometheus-nats-exporter"})[0]
	for _, row := range []struct {
		name string
		args func() *gcp.ClusteredNatsHubArgs
	}{
		{"without privateProjects", clusteredArgs},
		{"with privateProjects", privateClusteredHub},
	} {
		t.Run(row.name, func(t *testing.T) {
			rendered := stringList(t, dig(t, renderClusteredHubArgs(t, liveHubKeys{}, row.args()).container(t, "prom-exporter"), "args"))
			require.Contains(t, rendered, "-jsz=all", "the exporter runs the chart's JetStream arguments")
			served := serveExporter(t, exporter, rendered)
			podMonitoring, rules := hubMetricRelabeling(t, runClusteredHub(t, row.args()))

			// The control uses the same relabeling path, with no rules, before trusting a drop.
			naming := seriesWhere(applyRelabeling(nil, served), namesConsumer)
			require.ElementsMatch(t, []string{
				"nats_consumer_ack_floor_consumer_seq", "nats_consumer_ack_floor_stream_seq",
				"nats_consumer_delivered_consumer_seq", "nats_consumer_delivered_stream_seq",
				"nats_consumer_last_ack_seconds", "nats_consumer_last_delivery_seconds",
				"nats_consumer_num_ack_pending", "nats_consumer_num_pending",
				"nats_consumer_num_redelivered", "nats_consumer_num_waiting",
			}, seriesNames(naming), "without relabeling, every consumer metric names the consumer")
			for _, s := range naming {
				require.Equal(t, exporterTestConsumer, s.labels["consumer_name"], seriesText(s))
				require.Equal(t, exporterTestDescription, s.labels["consumer_desc"], seriesText(s))
			}
			kept := applyRelabeling(rules, served)
			if leaked := seriesWhere(kept, namesConsumer); len(leaked) > 0 {
				t.Fatalf("the PodMonitoring %s keeps %d series naming the consumer %q at the hub's scrape, %s among them", podMonitoring, len(leaked), exporterTestConsumer, seriesText(leaked[0]))
			}
			want := seriesWhere(served, func(s promSeries) bool { return !namesConsumer(s) })
			require.Equal(t, want, kept, "the scrape drops the consumer series and nothing else")
			keptNames := seriesNames(kept)
			require.Contains(t, keptNames, "nats_stream_limit_bytes", "the byte-limit alert's divisor is kept")
			require.Contains(t, keptNames, lastSeqMetric, "the under-replication alert's series is kept")
			t.Logf("the exporter, %s, served %d series; the scrape dropped %d naming the consumer (%s) and kept %d, stream series %s",
				strings.Join(rendered, " "), len(served), len(naming), strings.Join(seriesNames(naming), ", "), len(kept), strings.Join(seriesNames(seriesWhere(kept, inTestStream)), ", "))
		})
	}
}

// serveExporter runs exporter with rendered, the hub container's arguments, against a nats-server
// holding exporterTestStream, with a byte limit, and the durable consumer exporterTestConsumer on
// it, and returns every series one scrape serves once the consumer's delivery and ack are visible.
// The pod's exporter port, 7777, and its server's monitoring URL, http://localhost:8222/, are
// replaced by this machine's.
func serveExporter(t *testing.T, exporter string, rendered []string) []promSeries {
	t.Helper()
	srv, natsURL := testnats.Start(t)
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&natsgo.StreamConfig{
		Name: exporterTestStream, Subjects: []string{"live.limited.>"}, Storage: natsgo.FileStorage, MaxBytes: exporterTestStreamBytes,
	})
	require.NoError(t, err)
	_, err = js.AddConsumer(exporterTestStream, &natsgo.ConsumerConfig{
		Durable: exporterTestConsumer, Description: exporterTestDescription, AckPolicy: natsgo.AckExplicitPolicy,
	})
	require.NoError(t, err)
	_, err = js.Publish("live.limited.one", []byte("one"))
	require.NoError(t, err)
	// Both timestamps are optional until a delivery and an ack (collector/jsz.go:625-631).
	sub, err := js.PullSubscribe("live.limited.one", exporterTestConsumer, natsgo.Bind(exporterTestStream, exporterTestConsumer))
	require.NoError(t, err)
	messages, err := sub.Fetch(1, natsgo.MaxWait(5*time.Second))
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.NoError(t, messages[0].AckSync())

	monitor, err := url.Parse(srv.MonitorURL())
	require.NoError(t, err)
	port := reservePorts(t, 1)[0]
	args := podArgs(t, rendered, map[string]string{"-port=7777": fmt.Sprintf("-port=%d", port), "localhost:8222": monitor.Host})
	var served []promSeries
	startReady(t, "the exporter", exec.Command(exporter, args...), func(*cmdtest.Process) error {
		series, err := scrape(port)
		if err != nil {
			return err
		}
		// scrape preserves the exposition's escaping. Decode it before checking label values.
		for _, s := range series {
			for label, value := range s.labels {
				decoded, err := strconv.Unquote(`"` + value + `"`)
				if err != nil {
					return fmt.Errorf("decode %s label %s: %w", s.name, label, err)
				}
				s.labels[label] = decoded
			}
		}
		if !slices.ContainsFunc(series, inTestStream) {
			return errors.New("no series of " + exporterTestStream + " yet")
		}
		for _, name := range []string{"nats_consumer_last_delivery_seconds", "nats_consumer_last_ack_seconds"} {
			if !slices.ContainsFunc(series, func(s promSeries) bool {
				return s.name == name && inTestStream(s) && s.labels["consumer_name"] == exporterTestConsumer && s.labels["consumer_desc"] == exporterTestDescription
			}) {
				return fmt.Errorf("no %s with the consumer's decoded labels yet", name)
			}
		}
		served = series
		return nil
	})
	return served
}

// relabelRule is one entry of a PodMonitoring endpoint's metricRelabeling: a drop (drop true) or
// a keep of the series whose sourceLabels match regex.
type relabelRule struct {
	drop         bool
	sourceLabels []string
	regex        *regexp.Regexp
}

// TestMetricNameSourceLabels holds the boundary between exporter labels the model sees and the
// target labels Managed Prometheus adds later. Removing metricNameSourceLabels' exact check lets a
// target label pass and makes this test fail.
func TestMetricNameSourceLabels(t *testing.T) {
	require.True(t, metricNameSourceLabels([]any{"__name__"}))
	require.False(t, metricNameSourceLabels([]any{"job"}))
	require.False(t, metricNameSourceLabels([]any{"__name__", "job"}))
}

// metricNameSourceLabels is whether sourceLabels is the one exporter label the model applies.
func metricNameSourceLabels(sourceLabels any) bool {
	labels, ok := sourceLabels.([]any)
	return ok && len(labels) == 1 && labels[0] == "__name__"
}

// hubMetricRelabeling is the namespace and name of the PodMonitoring the hub registers under m, as
// <namespace>/<name>, and the metricRelabeling of its one endpoint, none when it has none. It
// accepts only drop and keep rules whose fields are action, sourceLabels and regex, and whose
// sourceLabels are exactly [__name__]: the model sees exporter labels, not target labels Managed
// Prometheus adds after scraping. It also refuses a rule that leaves action, sourceLabels or regex
// unset or its regex empty.
func hubMetricRelabeling(t *testing.T, m *pulumitest.Mocks) (string, []relabelRule) {
	t.Helper()
	podMonitoring := m.One(t, podMonitoringType).Inputs.Mappable()
	name := fmt.Sprintf("%v/%v", dig(t, podMonitoring, "metadata", "namespace"), dig(t, podMonitoring, "metadata", "name"))
	endpoints, ok := dig(t, podMonitoring, "spec", "endpoints").([]any)
	require.True(t, ok, "the PodMonitoring's endpoints are a list")
	require.Len(t, endpoints, 1, "the PodMonitoring scrapes one endpoint")
	endpoint, ok := endpoints[0].(map[string]any)
	require.True(t, ok, "the PodMonitoring's endpoint is an object: %v", endpoints[0])
	var entries []any
	if v, set := endpoint["metricRelabeling"]; set {
		entries, ok = v.([]any)
		require.True(t, ok, "the endpoint's metricRelabeling is a list: %v", v)
	}
	var rules []relabelRule
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		require.True(t, ok, "each metricRelabeling entry is an object: %v", entry)
		for field := range fields {
			require.Contains(t, []string{"action", "sourceLabels", "regex"}, field, "the test applies rules of these fields alone: %v", fields)
		}
		action, _ := fields["action"].(string)
		require.Contains(t, []string{"drop", "keep"}, action, "the test applies drop and keep alone: %v", fields)
		sourceLabels := fields["sourceLabels"]
		require.True(t, metricNameSourceLabels(sourceLabels),
			"the model has only the exporter's metric name, not target labels Managed Prometheus adds: %v", fields)
		labels := stringList(t, sourceLabels)
		pattern, _ := fields["regex"].(string)
		require.NotEmpty(t, pattern, "the test applies rules that set a regex: %v", fields)
		regex, err := regexp.Compile("^(?:" + pattern + ")$")
		require.NoError(t, err, "the rule's regex compiles as the collectors anchor it: %v", fields)
		rules = append(rules, relabelRule{drop: action == "drop", sourceLabels: labels, regex: regex})
	}
	return name, rules
}

// applyRelabeling is the series of served that rules keep, as Managed Prometheus's collectors
// apply a drop or a keep rule of a PodMonitoring's metricRelabeling to a scrape's series. They run
// Google's Prometheus fork (v2.53.5-gmp.7 in GKE; github.com/GoogleCloudPlatform/prometheus at
// 3f3316d7cf6f, model/relabel/relabel.go): the values of the rule's source labels, __name__ the
// series' name, joined by the default separator ";" (:34, :251), and matched by the regex anchored
// at both ends (NewRegexp, :173-175); drop removes a series that matches, keep one that does not
// (:254-261).
func applyRelabeling(rules []relabelRule, served []promSeries) []promSeries {
	return seriesWhere(served, func(s promSeries) bool {
		return !slices.ContainsFunc(rules, func(rule relabelRule) bool { return rule.drops(s) })
	})
}

// drops reports whether rule removes s: a drop rule whose regex matches the values of its source
// labels, or a keep rule whose regex does not.
func (rule relabelRule) drops(s promSeries) bool {
	values := make([]string, len(rule.sourceLabels))
	for i, label := range rule.sourceLabels {
		if label == "__name__" {
			values[i] = s.name
		} else {
			values[i] = s.labels[label]
		}
	}
	return rule.regex.MatchString(strings.Join(values, ";")) == rule.drop
}

// seriesWhere is each series of series for which keep is true, in series' order.
func seriesWhere(series []promSeries, keep func(promSeries) bool) []promSeries {
	var out []promSeries
	for _, s := range series {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// inTestStream reports whether s is a series of exporterTestStream.
func inTestStream(s promSeries) bool { return s.labels["stream_name"] == exporterTestStream }

// namesConsumer reports whether any label value contains the test consumer's name or description,
// including a name embedded in a subject or another compound value.
func namesConsumer(s promSeries) bool {
	for _, value := range s.labels {
		if strings.Contains(value, exporterTestConsumer) || strings.Contains(value, exporterTestDescription) {
			return true
		}
	}
	return false
}

// seriesText is s in the exposition format's form, its labels sorted: name{label="value",...}.
func seriesText(s promSeries) string {
	labels := make([]string, 0, len(s.labels))
	for label, value := range s.labels {
		labels = append(labels, fmt.Sprintf("%s=%q", label, value))
	}
	sort.Strings(labels)
	return s.name + "{" + strings.Join(labels, ",") + "}"
}
