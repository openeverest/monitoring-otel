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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/runtime"
)

// Protocol is the OTLP transport a destination receives on.
type Protocol string

// OTLP transports.
const (
	ProtocolGRPC Protocol = "grpc"
	ProtocolHTTP Protocol = "http/protobuf"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Authentication modes of the class, besides the core-reserved Anonymous.
// Each names the credentials keys of its Secret.
const (
	AuthModeBasicAuth   = "BasicAuth"
	AuthModeBearerToken = "BearerToken"
	AuthModeAPIKey      = "APIKey"

	CredentialKeyUsername = "username"
	CredentialKeyPassword = "password"
	CredentialKeyToken    = "token"
	CredentialKeyAPIKey   = "apiKey"
)

// destinationParameters is MonitoringDestination.spec.parameters for an
// opentelemetry class.
type destinationParameters struct {
	// Endpoint is the OTLP receiver URL. For grpc it is the server address
	// (https://otlp.example.com:4317); for http/protobuf it is the base URL
	// the exporter appends /v1/metrics to.
	Endpoint string `json:"endpoint"`
	// Protocol is grpc (default) or http/protobuf.
	Protocol Protocol `json:"protocol,omitempty"`
	// Headers are extra, non-secret request headers (e.g. a tenant id).
	Headers map[string]string `json:"headers,omitempty"`
	// APIKeyHeader is the header the APIKey credential is sent in
	// (e.g. x-honeycomb-team, DD-API-KEY).
	APIKeyHeader string `json:"apiKeyHeader,omitempty"`
	// Compression is gzip (default), zstd or none.
	Compression string `json:"compression,omitempty"`
	// Interval is the scrape interval; empty uses 30s.
	Interval string `json:"interval,omitempty"`
}

func decodeParameters(raw *runtime.RawExtension) (destinationParameters, error) {
	p := destinationParameters{}
	if raw != nil && len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &p); err != nil {
			return p, fmt.Errorf("decode parameters: %w", err)
		}
	}
	if p.Protocol == "" {
		p.Protocol = ProtocolGRPC
	}
	return p, p.validate()
}

func (p destinationParameters) validate() error {
	if p.Protocol != ProtocolGRPC && p.Protocol != ProtocolHTTP {
		return fmt.Errorf("protocol %q is not grpc or http/protobuf", p.Protocol)
	}
	if p.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "" {
		return fmt.Errorf("endpoint %q is not an http:// or https:// URL", p.Endpoint)
	}
	if !slices.Contains([]string{"", "gzip", "zstd", "none"}, p.Compression) {
		return fmt.Errorf("compression %q is not gzip, zstd or none", p.Compression)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			return errors.New("headers: Authorization comes from the authentication mode, not from parameters")
		}
	}
	if p.Interval != "" {
		if _, err := model.ParseDuration(p.Interval); err != nil {
			return fmt.Errorf("interval %q is not a Prometheus duration such as 30s or 1m", p.Interval)
		}
	}
	return nil
}

// insecureTransport reports whether the endpoint is plaintext.
func (p destinationParameters) insecureTransport() bool {
	u, err := url.Parse(p.Endpoint)
	return err == nil && u.Scheme == schemeHTTP
}
