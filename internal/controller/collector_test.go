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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

func authenticatedDestination(mode, parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := testDestination(parameters)
	dst.UID = "d-uid"
	dst.Spec.Authentication = common.Authentication{Mode: mode, CredentialsSecretRef: &common.SecretRef{Name: "otlp-credentials"}}
	return dst
}

func renderCollector(t *testing.T, dst *monitoringv1alpha1.MonitoringDestination, opts collectorOptions) map[string]any {
	t.Helper()
	params, err := decodeParameters(dst.Spec.Parameters)
	require.NoError(t, err)
	u, err := collectorFor(dst, params, opts)
	require.NoError(t, err)
	return u.Object
}

func nested(t *testing.T, obj map[string]any, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(obj, path...)
	require.NoError(t, err)
	require.True(t, found, "missing %s", strings.Join(path, "."))
	return v
}

func TestCollectorForBasicAuthOverGRPC(t *testing.T) {
	t.Parallel()
	dst := authenticatedDestination(AuthModeBasicAuth, `{"endpoint":"https://otlp.example.com:4317","headers":{"X-Scope-OrgID":"team-a"}}`)
	dst.Spec.TLS = &common.TLSConfig{CA: &common.CABundleSource{SecretKeyRef: &common.SecretKeyRef{Name: "otlp-ca", Key: "bundle.pem"}}}

	obj := renderCollector(t, dst, collectorOptions{TargetAllocatorMTLS: true})

	assert.Equal(t, "oe-otlp", nested(t, obj, "metadata", "name"))
	exporter := nested(t, obj, "spec", "config", "exporters", "otlp/destination")
	assert.Equal(t, map[string]any{
		"endpoint":    "otlp.example.com:4317",
		"compression": "gzip",
		"headers":     map[string]any{"X-Scope-OrgID": "team-a"},
		"auth":        map[string]any{"authenticator": "basicauth/destination"},
		"tls":         map[string]any{"ca_file": "/etc/openeverest/destination-ca/ca.crt"},
	}, exporter)
	assert.Equal(t, map[string]any{"client_auth": map[string]any{ //nolint:gosec // environment placeholders, not credentials
		"username": "${env:OE_DESTINATION_USERNAME}", "password": "${env:OE_DESTINATION_PASSWORD}",
	}}, nested(t, obj, "spec", "config", "extensions", "basicauth/destination"))
	assert.Equal(t, []any{"basicauth/destination"}, nested(t, obj, "spec", "config", "service", "extensions"))
	assert.Equal(t, []any{
		map[string]any{"name": "OE_DESTINATION_USERNAME", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "otlp-credentials", "key": "username"}}},
		map[string]any{"name": "OE_DESTINATION_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "otlp-credentials", "key": "password"}}},
	}, nested(t, obj, "spec", "env"))
	assert.Equal(t, []any{map[string]any{
		"name":   "destination-ca",
		"secret": map[string]any{"secretName": "otlp-ca", "items": []any{map[string]any{"key": "bundle.pem", "path": "ca.crt"}}},
	}}, nested(t, obj, "spec", "volumes"))

	assert.Equal(t, map[string]any{"enabled": true}, nested(t, obj, "spec", "targetAllocator", "mtls"))
	assert.Equal(t, []any{testNamespace}, nested(t, obj, "spec", "targetAllocator", "prometheusCR", "allowNamespaces"),
		"the allocator only discovers in the destination's namespace")
	assert.Equal(t, true, nested(t, obj, "spec", "targetAllocator", "prometheusCR", "denyFSAccessThroughSMs"))
	assert.Equal(t, map[string]any{"matchLabels": map[string]any{
		monitoringv1alpha1.BindingDestinationLabel: "otlp",
		monitoringv1alpha1.BindingClassLabel:       "opentelemetry",
	}}, nested(t, obj, "spec", "targetAllocator", "prometheusCR", "podMonitorSelector"))
}

func TestCollectorForPlaintextHTTPWithAPIKey(t *testing.T) {
	t.Parallel()
	dst := authenticatedDestination(AuthModeAPIKey,
		`{"endpoint":"http://collector.obs:4318","protocol":"http/protobuf","apiKeyHeader":"x-honeycomb-team","compression":"none"}`)

	obj := renderCollector(t, dst, collectorOptions{})

	assert.Equal(t, map[string]any{
		"endpoint":    "http://collector.obs:4318",
		"compression": "none",
		"headers":     map[string]any{"x-honeycomb-team": "${env:OE_DESTINATION_API_KEY}"},
		"tls":         map[string]any{"insecure": true},
	}, nested(t, obj, "spec", "config", "exporters", "otlphttp/destination"))
	assert.Equal(t, []any{}, nested(t, obj, "spec", "config", "service", "extensions"))
	_, mtls, err := unstructured.NestedFieldNoCopy(obj, "spec", "targetAllocator", "mtls")
	require.NoError(t, err)
	assert.False(t, mtls)
}

func TestCollectorForAPIKeyNeedsItsHeader(t *testing.T) {
	t.Parallel()
	dst := authenticatedDestination(AuthModeAPIKey, `{"endpoint":"https://otlp.example.com"}`)
	params, err := decodeParameters(dst.Spec.Parameters)
	require.NoError(t, err)
	_, err = collectorFor(dst, params, collectorOptions{})
	assert.ErrorContains(t, err, "apiKeyHeader")
}

func TestCollectorNameFitsOperatorSuffixes(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 63)
	name := collectorName(long)
	assert.LessOrEqual(t, len(name+"-collector-monitoring"), 63)
	assert.NotEqual(t, name, collectorName(strings.Repeat("a", 62)+"b"), "truncated names stay distinct")
	assert.Equal(t, "oe-short", collectorName("short"))
}

func TestDecodeParameters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    destinationParameters
		wantErr string
	}{
		{name: "defaults to grpc", raw: `{"endpoint":"https://otlp.example.com:4317"}`, want: destinationParameters{Endpoint: "https://otlp.example.com:4317", Protocol: ProtocolGRPC}},
		{name: "endpoint required", raw: `{}`, wantErr: "endpoint is required"},
		{name: "endpoint must be a URL", raw: `{"endpoint":"otlp.example.com:4317"}`, wantErr: "is not an http:// or https:// URL"},
		{name: "unknown protocol", raw: `{"endpoint":"https://x","protocol":"thrift"}`, wantErr: `protocol "thrift"`},
		{name: "Authorization belongs to the auth mode", raw: `{"endpoint":"https://x","headers":{"authorization":"Bearer t"}}`, wantErr: "Authorization comes from the authentication mode"},
		{name: "interval", raw: `{"endpoint":"https://x","interval":"soon"}`, wantErr: `interval "soon"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeParameters(&runtime.RawExtension{Raw: []byte(tt.raw)})
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadinessFromExports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		delta exportCounters
		want  monitoringv1alpha1.ReadyReason
	}{
		{exportCounters{}, monitoringv1alpha1.ReasonReady},
		{exportCounters{Sent: 10}, monitoringv1alpha1.ReasonReady},
		{exportCounters{Sent: 10, Failed: 2}, monitoringv1alpha1.ReasonReady},
		{exportCounters{Failed: 2}, monitoringv1alpha1.ReasonUnreachable},
		{exportCounters{Failed: 2, Rejected: 2}, monitoringv1alpha1.ReasonUnauthorized},
		{exportCounters{Sent: 1, Failed: 2, Rejected: 2}, monitoringv1alpha1.ReasonReady},
	}
	for _, tt := range tests {
		got, _ := readinessFromExports(tt.delta)
		assert.Equal(t, tt.want, got, "%+v", tt.delta)
	}
}

func TestRecordReportsIncreaseSinceLastProbe(t *testing.T) {
	t.Parallel()
	r := &destinationReconciler{}
	key := client.ObjectKey{Namespace: testNamespace, Name: "otlp"}

	assert.Equal(t, exportCounters{Sent: 100, Failed: 5}, r.record(key, "u1", exportCounters{Sent: 100, Failed: 5}), "first probe counts from zero")
	assert.Equal(t, exportCounters{Sent: 0, Failed: 3}, r.record(key, "u1", exportCounters{Sent: 100, Failed: 8}))
	assert.Equal(t, exportCounters{Sent: 7}, r.record(key, "u1", exportCounters{Sent: 7}), "a restarted collector counts from zero")
	assert.Equal(t, exportCounters{Sent: 9}, r.record(key, "u2", exportCounters{Sent: 9}), "a recreated destination counts from zero")
}

func TestParseExportCountersSumsTheDestinationExporter(t *testing.T) {
	t.Parallel()
	unsuffixed := `# TYPE otelcol_exporter_sent_metric_points counter
otelcol_exporter_sent_metric_points{exporter="otlphttp/destination",server_port="8428"} 67395
`
	got, err := parseExportCounters(strings.NewReader(unsuffixed))
	require.NoError(t, err)
	assert.Equal(t, exportCounters{Sent: 67395}, got, "collector 0.162 drops the _total suffix")

	telemetry := `# TYPE otelcol_exporter_sent_metric_points_total counter
otelcol_exporter_sent_metric_points_total{exporter="otlp/destination",service_name="otelcol-k8s"} 120
otelcol_exporter_sent_metric_points_total{exporter="debug",service_name="otelcol-k8s"} 999
# TYPE otelcol_exporter_send_failed_metric_points_total counter
otelcol_exporter_send_failed_metric_points_total{exporter="otlp/destination",error_type="Unavailable"} 4
otelcol_exporter_send_failed_metric_points_total{exporter="otlp/destination",error_type="Unauthenticated"} 6
`
	got, err = parseExportCounters(strings.NewReader(telemetry))
	require.NoError(t, err)
	assert.Equal(t, exportCounters{Sent: 120, Failed: 10, Rejected: 6}, got)
}

func acceptedDestination(parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := testDestination(parameters)
	dst.UID = "d-uid"
	dst.Status.Conditions = []metav1.Condition{{
		Type: monitoringv1alpha1.MonitoringDestinationConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1,
		Reason: "Accepted", LastTransitionTime: metav1.Now(),
	}}
	return dst
}

func destinationReady(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	dst := &monitoringv1alpha1.MonitoringDestination{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "otlp"}, dst))
	for i := range dst.Status.Conditions {
		if dst.Status.Conditions[i].Type == monitoringv1alpha1.MonitoringDestinationConditionReady {
			return &dst.Status.Conditions[i]
		}
	}
	return nil
}

func TestDestinationReconcilerRunsACollectorAndReportsExports(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, nil, testClass(), acceptedDestination(""))
	probed := exportCounters{}
	r := &destinationReconciler{
		client: c, cc: testController(c), targetAllocatorCRB: "monitoring-otel-target-allocator",
		probe: func(context.Context, string) (exportCounters, error) { return probed, nil },
	}
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "otlp"}}

	res, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, startingRecheckInterval, res.RequeueAfter)
	assert.Equal(t, string(monitoringv1alpha1.ReasonReadyPending), destinationReady(t, c).Reason, "Pending until the operator reports a running collector")

	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"})
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "oe-otlp-targetallocator"}, rb))
	assert.Equal(t, "monitoring-otel-target-allocator", nested(t, rb.Object, "roleRef", "name"))

	collector := &unstructured.Unstructured{}
	collector.SetGroupVersionKind(schema.GroupVersionKind{Group: "opentelemetry.io", Version: "v1beta1", Kind: "OpenTelemetryCollector"})
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "oe-otlp"}, collector))
	require.NoError(t, unstructured.SetNestedField(collector.Object, "1/1", "status", "scale", "statusReplicas"))
	require.NoError(t, unstructured.SetNestedField(collector.Object, "0.162.0", "status", "version"))
	require.NoError(t, c.Update(t.Context(), collector))

	probed = exportCounters{Failed: 50}
	res, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, readyRecheckInterval, res.RequeueAfter)
	ready := destinationReady(t, c)
	assert.Equal(t, string(monitoringv1alpha1.ReasonUnreachable), ready.Reason)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)

	probed = exportCounters{Sent: 400, Failed: 50}
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	ready = destinationReady(t, c)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, "collector oe-otlp: exported 400 data points", ready.Message)
}

func TestDestinationReconcilerWaitsForCoreToAccept(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, nil, testClass(), testDestination(""))
	r := &destinationReconciler{client: c, cc: testController(c), targetAllocatorCRB: "ta"}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "otlp"}})
	require.NoError(t, err)

	assert.Equal(t, string(monitoringv1alpha1.ReasonReadyPending), destinationReady(t, c).Reason)
	collectors := &unstructured.UnstructuredList{}
	collectors.SetGroupVersionKind(schema.GroupVersionKind{Group: "opentelemetry.io", Version: "v1beta1", Kind: "OpenTelemetryCollectorList"})
	require.NoError(t, c.List(t.Context(), collectors))
	assert.Empty(t, collectors.Items, "no collector runs credentials core has not validated")
}
