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

import "testing"

func TestDestinationPolicyExactOriginMatching(t *testing.T) {
	policy, err := NewDestinationPolicy([]string{
		"https://PROMETHEUS.monitoring.svc./",
		"http://prometheus.monitoring.svc:9090",
		"http://[2001:db8::10]:9090",
	}, false)
	if err != nil {
		t.Fatalf("NewDestinationPolicy: %v", err)
	}

	allowed := []string{
		"https://prometheus.monitoring.svc/api/prometheus",
		"https://prometheus.monitoring.svc:443",
		"https://PROMETHEUS.MONITORING.SVC./prefix",
		"http://prometheus.monitoring.svc:9090",
		"http://prometheus.monitoring.svc:09090",
		"http://prometheus.monitoring.svc:9090/prefix",
		"http://[2001:db8::10]:9090",
	}
	for _, rawURL := range allowed {
		if _, err := policy.Validate(rawURL); err != nil {
			t.Errorf("Validate(%q) unexpectedly rejected: %v", rawURL, err)
		}
	}

	rejected := []string{
		"http://prometheus.monitoring.svc",                // scheme and effective port differ
		"https://prometheus.monitoring.svc:9090",          // scheme differs
		"http://prometheus.monitoring.svc:9091",           // port differs
		"http://prometheus.monitoring.svc.evil:9090",      // DNS suffix bypass
		"http://evilprometheus.monitoring.svc:9090",       // DNS prefix bypass
		"http://user@prometheus.monitoring.svc:9090",      // credentials remain forbidden
		"http://prometheus.monitoring.svc:9090?next=evil", // query remains forbidden
		"http://prometheus.monitoring.svc:9090#evil",      // fragment remains forbidden
		" http://prometheus.monitoring.svc:9090",          // no whitespace normalization
		"http://prometheus_mirror.monitoring.svc:9090",    // ambiguous/non-DNS hostname
		"http://prometheus.monitoring.svc:9090@evil:9090", // authority confusion
		"http://[2001:db8::11]:9090",                      // IPv6 address differs
		"http://[2001:db8::10%25eth0]:9090",               // scoped IPv6 is interface-dependent
		"http://127.1:9090",                               // ambiguous alternate IPv4 form
		"http://prometheus.monitoring.svc:65536",          // out-of-range port
	}
	for _, rawURL := range rejected {
		if _, err := policy.Validate(rawURL); err == nil {
			t.Errorf("Validate(%q) should reject origin bypass", rawURL)
		}
	}
}

func TestDestinationPolicyFailsClosed(t *testing.T) {
	policy, err := NewDestinationPolicy(nil, false)
	if err != nil {
		t.Fatalf("NewDestinationPolicy: %v", err)
	}
	if _, err := policy.Validate("https://prometheus.monitoring.svc"); err == nil {
		t.Fatal("an empty policy must deny every destination")
	}

	var zero DestinationPolicy
	if _, err := zero.Validate("https://prometheus.monitoring.svc"); err == nil {
		t.Fatal("the zero-value policy must deny every destination")
	}
}

func TestDestinationPolicyTrustedCompatibilityMode(t *testing.T) {
	policy, err := NewDestinationPolicy(nil, true)
	if err != nil {
		t.Fatalf("NewDestinationPolicy: %v", err)
	}
	if _, err := policy.Validate("http://another-backend.example:9191/prometheus"); err != nil {
		t.Fatalf("allow-any compatibility policy rejected a safe URL: %v", err)
	}
	if _, err := policy.Validate("http://user:secret@another-backend.example:9191"); err == nil {
		t.Fatal("allow-any must not bypass URL safety validation")
	}
	if _, err := NewDestinationPolicy([]string{"https://prometheus.example"}, true); err == nil {
		t.Fatal("allow-any combined with rules must fail closed")
	}
}

func TestDestinationPolicyRejectsInvalidRules(t *testing.T) {
	badRules := []string{
		"prometheus.monitoring.svc:9090",
		"ftp://prometheus.monitoring.svc",
		"http://user@prometheus.monitoring.svc",
		"http://prometheus.monitoring.svc/prometheus",
		"http://prometheus.monitoring.svc?tenant=one",
		"http://prometheus.monitoring.svc#fragment",
		"http://prometheus_mirror.monitoring.svc",
		"http://[fe80::1%25eth0]:9090",
		"http://prometheus.monitoring.svc:0",
		"http://prometheus.monitoring.svc:65536",
	}
	for _, rule := range badRules {
		if _, err := NewDestinationPolicy([]string{rule}, false); err == nil {
			t.Errorf("NewDestinationPolicy(%q) should reject invalid rule", rule)
		}
	}
}

func TestNewClientEnforcesPolicyBeforeTransport(t *testing.T) {
	policy, err := NewDestinationPolicy([]string{"https://prometheus.example:9443"}, false)
	if err != nil {
		t.Fatalf("NewDestinationPolicy: %v", err)
	}
	if _, err := NewClient("https://prometheus.example:9443/prefix", 0, policy); err != nil {
		t.Fatalf("allowed client: %v", err)
	}
	if _, err := NewClient("https://metadata.example:9443", 0, policy); err == nil {
		t.Fatal("client must reject a non-allowed destination")
	}
}
