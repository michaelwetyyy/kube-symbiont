/*
Copyright 2026.

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

package sources

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// DestinationPolicy is the operator-owned boundary for Prometheus egress.
// Its zero value denies every destination. Construct it once at manager
// startup, then share the immutable value with every reconciler.
type DestinationPolicy struct {
	allowAny bool
	allowed  map[string]struct{}
}

// NewDestinationPolicy builds an exact-origin allowlist. Rules are absolute
// HTTP(S) origins only: scheme, host and optional port, with no path or other
// URL components. An omitted port is canonicalized to 80 or 443.
//
// allowAny is the explicit compatibility mode for installations where every
// ShadowWorkload author is trusted to choose a controller-reachable backend.
// Combining compatibility mode with rules is rejected to keep the effective
// policy unambiguous.
func NewDestinationPolicy(allowedOrigins []string, allowAny bool) (DestinationPolicy, error) {
	if allowAny && len(allowedOrigins) != 0 {
		return DestinationPolicy{}, errors.New("prometheus destination policy cannot combine allow-any with allowed origins")
	}

	policy := DestinationPolicy{allowAny: allowAny}
	if len(allowedOrigins) == 0 {
		return policy, nil
	}
	policy.allowed = make(map[string]struct{}, len(allowedOrigins))
	for _, rule := range allowedOrigins {
		parsed, origin, err := parsePrometheusURL(rule)
		if err != nil {
			return DestinationPolicy{}, errors.New("invalid prometheus allowed destination")
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return DestinationPolicy{}, errors.New("invalid prometheus allowed destination: origin must not contain a path")
		}
		policy.allowed[origin] = struct{}{}
	}
	return policy, nil
}

// Validate permits rawURL only when it is a safe HTTP(S) URL and its
// canonical origin is allowed. It deliberately does not resolve DNS: network
// policy remains the enforcement layer for IP ranges and DNS rebinding.
func (p DestinationPolicy) Validate(rawURL string) (*url.URL, error) {
	parsed, origin, err := parsePrometheusURL(rawURL)
	if err != nil {
		return nil, err
	}
	if p.allowAny {
		return parsed, nil
	}
	if _, ok := p.allowed[origin]; !ok {
		return nil, errors.New("prometheus destination is not allowed by manager policy")
	}
	return parsed, nil
}

func parsePrometheusURL(rawURL string) (*url.URL, string, error) {
	// Reject leading/trailing whitespace instead of silently changing the
	// operator or workload author's value.
	if rawURL == "" || strings.TrimSpace(rawURL) != rawURL {
		return nil, "", errors.New("invalid prometheusURL")
	}
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, "", errors.New("invalid prometheusURL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" || parsed.Opaque != "" {
		return nil, "", errors.New("invalid prometheusURL: absolute http(s) URL with a host required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", errors.New("invalid prometheusURL: credentials, query parameters and fragments are not allowed")
	}

	host, err := canonicalHostname(parsed.Hostname())
	if err != nil {
		return nil, "", errors.New("invalid prometheusURL: host is not canonicalizable")
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	} else {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, "", errors.New("invalid prometheusURL: port must be between 1 and 65535")
		}
		port = strconv.Itoa(portNumber)
	}
	parsed.Scheme = scheme
	return parsed, fmt.Sprintf("%s\x00%s\x00%s", scheme, host, port), nil
}

func canonicalHostname(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("empty or scoped host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if strings.Trim(host, "0123456789.") == "" {
		return "", errors.New("ambiguous numeric host")
	}
	if errs := validation.IsDNS1123Subdomain(host); len(errs) != 0 {
		return "", errors.New("invalid DNS host")
	}
	return host, nil
}
