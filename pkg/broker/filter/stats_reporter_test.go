/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package filter

import (
	"net/http"
	"testing"
	"time"

	"go.opencensus.io/resource"
	broker "knative.dev/eventing/pkg/broker"
	"knative.dev/eventing/pkg/metrics"
	"knative.dev/pkg/metrics/metricstest"
	_ "knative.dev/pkg/metrics/testing"
)

func TestStatsReporter(t *testing.T) {
	setup()
	args := &ReportArgs{
		ns:         "testns",
		trigger:    "testtrigger",
		broker:     "testbroker",
		filterType: "testeventtype",
	}

	r := NewStatsReporter("testcontainer", "testpod")

	wantTags := map[string]string{
		metrics.LabelFilterType:   "testeventtype",
		broker.LabelContainerName: "testcontainer",
		broker.LabelUniqueName:    "testpod",
	}

	wantAllTags := map[string]string{}
	for k, v := range wantTags {
		wantAllTags[k] = v
	}
	wantAllTags[metrics.LabelResponseCode] = "202"
	wantAllTags[metrics.LabelResponseCodeClass] = "2xx"

	resource := resource.Resource{
		Type: metrics.ResourceTypeKnativeTrigger,
		Labels: map[string]string{
			metrics.LabelNamespaceName: "testns",
			metrics.LabelTriggerName:   "testtrigger",
			metrics.LabelBrokerName:    "testbroker",
		},
	}

	// test ReportEventCount
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	metricstest.AssertMetric(t, metricstest.IntMetric("event_count", 2, wantAllTags).WithResource(&resource))
	metricstest.CheckCountData(t, "event_count", wantAllTags, 2)

	// test ReportEventDispatchTime
	expectSuccess(t, func() error {
		return r.ReportEventDispatchTime(args, http.StatusAccepted, 1100*time.Millisecond)
	})
	expectSuccess(t, func() error {
		return r.ReportEventDispatchTime(args, http.StatusAccepted, 9100*time.Millisecond)
	})
	metricstest.AssertMetric(t, metricstest.DistributionCountOnlyMetric("event_dispatch_latencies", 2, wantAllTags).WithResource(&resource))
	metricstest.CheckDistributionData(t, "event_dispatch_latencies", wantAllTags, 2, 1100.0, 9100.0)

	// test ReportEventProcessingTime
	expectSuccess(t, func() error {
		return r.ReportEventProcessingTime(args, 1000*time.Millisecond)
	})
	expectSuccess(t, func() error {
		return r.ReportEventProcessingTime(args, 8000*time.Millisecond)
	})
	metricstest.AssertMetric(t, metricstest.DistributionCountOnlyMetric("event_processing_latencies", 2, wantTags))
	metricstest.CheckDistributionData(t, "event_processing_latencies", wantTags, 2, 1000.0, 8000.0)
}

func TestReporterEmptySourceAndTypeFilter(t *testing.T) {
	setup()

	args := &ReportArgs{
		ns:            "testns",
		trigger:       "testtrigger",
		broker:        "testbroker",
		filterType:    "",
		requestScheme: "http",
	}

	r := NewStatsReporter("testcontainer", "testpod")

	wantTags := map[string]string{
		metrics.LabelFilterType:        anyValue,
		metrics.LabelResponseCode:      "202",
		metrics.LabelResponseCodeClass: "2xx",
		broker.LabelContainerName:      "testcontainer",
		broker.LabelUniqueName:         "testpod",
		metrics.LabelEventScheme:       "http",
	}

	resource := resource.Resource{
		Type: metrics.ResourceTypeKnativeTrigger,
		Labels: map[string]string{
			metrics.LabelNamespaceName: "testns",
			metrics.LabelTriggerName:   "testtrigger",
			metrics.LabelBrokerName:    "testbroker",
		},
	}

	// test ReportEventCount
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	expectSuccess(t, func() error {
		return r.ReportEventCount(args, http.StatusAccepted)
	})
	metricstest.AssertMetric(t, metricstest.IntMetric("event_count", 4, wantTags).WithResource(&resource))
}

func expectSuccess(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Error("Reporter expected success but got error:", err)
	}
}

func setup() {
	resetMetrics()
}

func resetMetrics() {
	// OpenCensus metrics carry global state that need to be reset between unit tests.
	metricstest.Unregister(
		"event_count",
		"event_dispatch_latencies",
		"event_processing_latencies")
	register()
}
