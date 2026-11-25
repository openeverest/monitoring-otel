// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

// collectorTelemetryPort is where the collector serves its own metrics; the
// OpenTelemetry Operator exposes it through the <name>-collector-monitoring
// Service.
const collectorTelemetryPort = 8888

// Exporter self-telemetry of the collector, as Prometheus metric families.
// Collector versions differ in whether counters carry the _total suffix.
const (
	sentMetricPoints   = "otelcol_exporter_sent_metric_points"
	failedMetricPoints = "otelcol_exporter_send_failed_metric_points"
)

// exportCounters are the collector's cumulative exporter counters.
type exportCounters struct {
	Sent   float64
	Failed float64
	// Rejected are the failures the destination answered with an
	// authentication or authorization error, a subset of Failed.
	Rejected float64
}

// rejectedErrorTypes are the error.type values (gRPC status codes, also
// used for HTTP exporters) of credentials the destination refused.
var rejectedErrorTypes = []string{"Unauthenticated", "PermissionDenied"} //nolint:gochecknoglobals // constant set

// exportProbe reads the exporter counters from a collector's telemetry URL.
type exportProbe func(ctx context.Context, url string) (exportCounters, error)

func collectorTelemetryURL(dst *monitoringv1alpha1.MonitoringDestination) string {
	return fmt.Sprintf("http://%s-collector-monitoring.%s.svc:%d/metrics", collectorName(dst.Name), dst.Namespace, collectorTelemetryPort)
}

// httpExportProbe scrapes the collector's telemetry over HTTP.
func httpExportProbe(httpClient *http.Client) exportProbe {
	return func(ctx context.Context, url string) (exportCounters, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return exportCounters{}, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return exportCounters{}, err
		}
		defer resp.Body.Close() //nolint:errcheck // read-only body
		if resp.StatusCode != http.StatusOK {
			return exportCounters{}, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return parseExportCounters(resp.Body)
	}
}

// parseExportCounters sums the exporter counters of the destination
// exporter over every label set.
func parseExportCounters(r io.Reader) (exportCounters, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return exportCounters{}, fmt.Errorf("parse collector telemetry: %w", err)
	}
	everyMetric := func(*dto.Metric) bool { return true }
	rejected := func(m *dto.Metric) bool { return slices.Contains(rejectedErrorTypes, labelValue(m, "error_type")) }
	sum := func(family string, match func(*dto.Metric) bool) float64 {
		return sumDestinationExporter(families[family], match) + sumDestinationExporter(families[family+"_total"], match)
	}
	return exportCounters{
		Sent:     sum(sentMetricPoints, everyMetric),
		Failed:   sum(failedMetricPoints, everyMetric),
		Rejected: sum(failedMetricPoints, rejected),
	}, nil
}

func sumDestinationExporter(mf *dto.MetricFamily, match func(*dto.Metric) bool) float64 {
	if mf == nil {
		return 0
	}
	var sum float64
	for _, m := range mf.GetMetric() {
		if strings.HasSuffix(labelValue(m, "exporter"), "/destination") && match(m) {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func toUnstructured(obj any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return u, nil
}
