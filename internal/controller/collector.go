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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

const (
	collectorAPIVersion = "opentelemetry.io/v1beta1"
	collectorKind       = "OpenTelemetryCollector"

	// collectorNamePrefix keeps the collectors this controller owns apart
	// from the ones users run in the same namespace.
	collectorNamePrefix = "oe-"
	// maxDestinationNameInCollector leaves room for the longest suffix the
	// OpenTelemetry Operator appends ("-collector-monitoring") within the
	// 63-character Service name limit.
	maxDestinationNameInCollector = 36

	keyEnabled = "enabled"

	// defaultScrapeInterval is used when the destination sets no interval.
	defaultScrapeInterval = "30s"

	caMountPath = "/etc/openeverest/destination-ca"
	caFileName  = "ca.crt"

	envUsername = "OE_DESTINATION_USERNAME"
	envPassword = "OE_DESTINATION_PASSWORD"
	envToken    = "OE_DESTINATION_TOKEN"
	envAPIKey   = "OE_DESTINATION_API_KEY"
)

// collectorOptions are the admin-level settings of every collector, set on
// the controller command line.
type collectorOptions struct {
	// Image overrides the OpenTelemetry Operator's default collector image.
	Image string
	// TargetAllocatorMTLS secures the target allocator to collector link, so
	// the allocator may hand over the Secrets PodMonitors reference (basic
	// auth, TLS CAs). Requires cert-manager.
	TargetAllocatorMTLS bool
}

// collectorName is the OpenTelemetryCollector serving a destination.
func collectorName(destination string) string {
	if len(destination) <= maxDestinationNameInCollector {
		return collectorNamePrefix + destination
	}
	sum := sha256.Sum256([]byte(destination))
	return collectorNamePrefix + destination[:maxDestinationNameInCollector-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// targetAllocatorServiceAccount is the ServiceAccount the operator creates
// for the collector's target allocator when the spec names none.
func targetAllocatorServiceAccount(destination string) string {
	return collectorName(destination) + "-targetallocator"
}

// podMonitorSelectorFor selects the PodMonitors rendered for the
// destination's bindings.
func podMonitorSelectorFor(dst *monitoringv1alpha1.MonitoringDestination) map[string]string {
	return map[string]string{
		monitoringv1alpha1.BindingDestinationLabel: dst.Name,
		monitoringv1alpha1.BindingClassLabel:       dst.Spec.ClassRef.Name,
	}
}

// collectorFor renders the OpenTelemetryCollector of a destination: a
// single-replica StatefulSet whose target allocator discovers the binding
// PodMonitors in the destination's namespace and whose OTLP exporter sends
// to the destination. Credentials reach the collector as environment
// variables referencing the destination's Secret; the controller never
// reads it.
func collectorFor(dst *monitoringv1alpha1.MonitoringDestination, params destinationParameters, opts collectorOptions) (*unstructured.Unstructured, error) {
	exporterName, exporter, err := exporterFor(dst, params)
	if err != nil {
		return nil, err
	}
	extensions, extensionNames := authExtensionsFor(dst.Spec.Authentication.Mode)
	if len(extensionNames) > 0 {
		exporter["auth"] = map[string]any{"authenticator": extensionNames[0]}
	}

	spec := map[string]any{
		"mode":            "statefulset",
		"replicas":        int64(1),
		"targetAllocator": targetAllocatorFor(dst, params, opts),
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "50m", "memory": "128Mi"},
			"limits":   map[string]any{"memory": "512Mi"},
		},
		"config": collectorConfig(exporterName, exporter, extensions, extensionNames),
	}
	if opts.Image != "" {
		spec["image"] = opts.Image
	}
	if env := envFor(dst); len(env) > 0 {
		spec["env"] = env
	}
	if ca := caRef(dst.Spec.TLS); ca != nil {
		spec["volumes"] = []any{map[string]any{
			"name": "destination-ca",
			"secret": map[string]any{
				"secretName": ca.Name,
				"items":      []any{map[string]any{"key": ca.Key, "path": caFileName}},
			},
		}}
		spec["volumeMounts"] = []any{map[string]any{"name": "destination-ca", "mountPath": caMountPath, "readOnly": true}}
	}

	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion(collectorAPIVersion)
	u.SetKind(collectorKind)
	u.SetName(collectorName(dst.Name))
	u.SetNamespace(dst.Namespace)
	u.SetLabels(map[string]string{
		monitoringv1alpha1.BindingDestinationLabel: dst.Name,
		monitoringv1alpha1.BindingClassLabel:       dst.Spec.ClassRef.Name,
	})
	u.SetOwnerReferences([]metav1.OwnerReference{destinationOwner(dst)})
	return u, nil
}

// targetAllocatorFor renders the collector's target allocator: it discovers
// only the destination's PodMonitors, only in the destination's namespace,
// and drops endpoints that would read files from the collector pod.
func targetAllocatorFor(dst *monitoringv1alpha1.MonitoringDestination, params destinationParameters, opts collectorOptions) map[string]any {
	selector := toAnyMap(podMonitorSelectorFor(dst))
	interval := params.Interval
	if interval == "" {
		interval = defaultScrapeInterval
	}
	ta := map[string]any{
		keyEnabled: true,
		"prometheusCR": map[string]any{
			keyEnabled:               true,
			"scrapeInterval":         interval,
			"allowNamespaces":        []any{dst.Namespace},
			"denyFSAccessThroughSMs": true,
			"podMonitorSelector":     map[string]any{"matchLabels": selector},
			// v1beta1 requires both selectors; no ServiceMonitor carries these labels.
			"serviceMonitorSelector": map[string]any{"matchLabels": selector},
		},
	}
	if opts.TargetAllocatorMTLS {
		ta["mtls"] = map[string]any{keyEnabled: true}
	}
	return ta
}

// collectorConfig is the collector pipeline: the prometheus receiver fed by
// the target allocator, then memory_limiter and batch, then the exporter.
func collectorConfig(exporterName string, exporter, extensions map[string]any, extensionNames []string) map[string]any {
	return map[string]any{
		"extensions": extensions,
		"receivers": map[string]any{
			"prometheus": map[string]any{"config": map[string]any{"scrape_configs": []any{}}},
		},
		"processors": map[string]any{
			"memory_limiter": map[string]any{"check_interval": "1s", "limit_percentage": int64(80), "spike_limit_percentage": int64(20)},
			"batch":          map[string]any{},
		},
		"exporters": map[string]any{exporterName: exporter},
		"service": map[string]any{
			// detailed adds error.type to the exporter failure counters,
			// which tells rejected credentials apart from other failures.
			"telemetry":  map[string]any{"metrics": map[string]any{"level": "detailed"}},
			"extensions": toAnySlice(extensionNames),
			"pipelines": map[string]any{
				"metrics": map[string]any{
					"receivers":  []any{"prometheus"},
					"processors": []any{"memory_limiter", "batch"},
					"exporters":  []any{exporterName},
				},
			},
		},
	}
}

// exporterFor renders the OTLP exporter of the destination.
func exporterFor(dst *monitoringv1alpha1.MonitoringDestination, params destinationParameters) (string, map[string]any, error) {
	exporter := map[string]any{}
	name := "otlp/destination"
	switch params.Protocol {
	case ProtocolHTTP:
		name = "otlphttp/destination"
		exporter["endpoint"] = params.Endpoint
	case ProtocolGRPC:
		u, err := url.Parse(params.Endpoint)
		if err != nil {
			return "", nil, fmt.Errorf("parse endpoint: %w", err)
		}
		exporter["endpoint"] = u.Host
	}
	compression := params.Compression
	if compression == "" {
		compression = "gzip"
	}
	exporter["compression"] = compression

	headers := map[string]any{}
	for k, v := range params.Headers {
		headers[k] = v
	}
	if dst.Spec.Authentication.Mode == AuthModeAPIKey {
		if params.APIKeyHeader == "" {
			return "", nil, fmt.Errorf("authentication mode %s needs the apiKeyHeader parameter", AuthModeAPIKey)
		}
		headers[params.APIKeyHeader] = "${env:" + envAPIKey + "}"
	}
	if len(headers) > 0 {
		exporter["headers"] = headers
	}

	tls := map[string]any{}
	if params.insecureTransport() {
		tls["insecure"] = true
	}
	if dst.Spec.TLS != nil && dst.Spec.TLS.InsecureSkipVerify {
		tls["insecure_skip_verify"] = true
	}
	if caRef(dst.Spec.TLS) != nil {
		tls["ca_file"] = caMountPath + "/" + caFileName
	}
	if len(tls) > 0 {
		exporter["tls"] = tls
	}
	return name, exporter, nil
}

// authExtensionsFor renders the client authenticator extension of an
// authentication mode; APIKey and Anonymous need none.
func authExtensionsFor(mode string) (map[string]any, []string) {
	switch mode {
	case AuthModeBasicAuth:
		return map[string]any{"basicauth/destination": map[string]any{
			"client_auth": map[string]any{"username": "${env:" + envUsername + "}", "password": "${env:" + envPassword + "}"},
		}}, []string{"basicauth/destination"}
	case AuthModeBearerToken:
		return map[string]any{"bearertokenauth/destination": map[string]any{
			"token": "${env:" + envToken + "}",
		}}, []string{"bearertokenauth/destination"}
	default:
		return map[string]any{}, nil
	}
}

// envFor maps the destination's credentials keys to the environment
// variables the exporter configuration expands.
func envFor(dst *monitoringv1alpha1.MonitoringDestination) []any {
	ref := dst.Spec.Authentication.CredentialsSecretRef
	if ref == nil {
		return nil
	}
	var keys [][2]string
	switch dst.Spec.Authentication.Mode {
	case AuthModeBasicAuth:
		keys = [][2]string{{envUsername, CredentialKeyUsername}, {envPassword, CredentialKeyPassword}}
	case AuthModeBearerToken:
		keys = [][2]string{{envToken, CredentialKeyToken}}
	case AuthModeAPIKey:
		keys = [][2]string{{envAPIKey, CredentialKeyAPIKey}}
	}
	env := make([]any, 0, len(keys))
	for _, k := range keys {
		env = append(env, map[string]any{
			"name": k[0],
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{
				"name": ref.Name,
				"key":  k[1],
			}},
		})
	}
	return env
}

func caRef(tls *common.TLSConfig) *common.SecretKeyRef {
	if tls == nil || tls.CA == nil {
		return nil
	}
	return tls.CA.SecretKeyRef
}

func destinationOwner(dst *monitoringv1alpha1.MonitoringDestination) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: monitoringv1alpha1.GroupVersion.String(),
		Kind:       "MonitoringDestination",
		Name:       dst.Name,
		UID:        dst.UID,
		Controller: new(true),
	}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAnySlice(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}
