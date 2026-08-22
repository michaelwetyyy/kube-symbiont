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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	symbiontv1alpha1 "github.com/michaelwetyyy/kube-symbiont/api/v1alpha1"
)

func dockerSpec(job, instance string) *symbiontv1alpha1.ShadowWorkloadSpec {
	return &symbiontv1alpha1.ShadowWorkloadSpec{
		Node: "lab",
		Source: symbiontv1alpha1.SourceSpec{
			Type:   symbiontv1alpha1.SourceTypeDocker,
			Docker: &symbiontv1alpha1.DockerSource{Selector: symbiontv1alpha1.SelectorAll, CadvisorJob: job, CadvisorInstance: instance},
		},
		Metrics: symbiontv1alpha1.MetricsConfig{PrometheusURL: "http://prom:9090", Window: "5m"},
	}
}

func TestResolveDockerGeneratesIDPrefixSelectors(t *testing.T) {
	q, err := Resolve(dockerSpec("cadvisor", "192.0.2.10:4194"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	wantMatchers := `job="cadvisor",instance="192.0.2.10:4194",id=~"/system.slice/docker-.*"`
	wantCPU := "sum(rate(container_cpu_usage_seconds_total{" + wantMatchers + "}[5m]))"
	wantMem := "sum(avg_over_time(container_memory_working_set_bytes{" + wantMatchers + "}[5m]))"

	if q.CPUCores != wantCPU {
		t.Errorf("cpu query:\n got %s\nwant %s", q.CPUCores, wantCPU)
	}
	if q.MemoryBytes != wantMem {
		t.Errorf("mem query:\n got %s\nwant %s", q.MemoryBytes, wantMem)
	}
}

func TestResolveDockerDefaultsAndOmittedInstance(t *testing.T) {
	q, err := Resolve(dockerSpec("", ""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(q.CPUCores, `job="cadvisor"`) {
		t.Errorf("default job label missing: %s", q.CPUCores)
	}
	if strings.Contains(q.CPUCores, "instance=") {
		t.Errorf("unexpected instance label: %s", q.CPUCores)
	}
	if !strings.Contains(q.CPUCores, "[5m]") || !strings.Contains(q.MemoryBytes, "[5m]") {
		t.Errorf("window not injected: %s | %s", q.CPUCores, q.MemoryBytes)
	}
}

func TestResolveDockerEscapesLabelValues(t *testing.T) {
	q, err := Resolve(dockerSpec(`my"job\`, ""))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(q.CPUCores, `job="my\"job\\"`) {
		t.Errorf("label value not escaped: %s", q.CPUCores)
	}
}

func TestResolvePromQLPassthrough(t *testing.T) {
	spec := &symbiontv1alpha1.ShadowWorkloadSpec{
		Source: symbiontv1alpha1.SourceSpec{
			Type: symbiontv1alpha1.SourceTypePromQL,
			PromQL: &symbiontv1alpha1.PromQLSource{
				CPUCores:    `sum(rate(node_cpu_seconds_total{mode!="idle"}[2m]))`,
				MemoryBytes: `sum(node_memory_Active_bytes)`,
			},
		},
		Metrics: symbiontv1alpha1.MetricsConfig{PrometheusURL: "http://prom:9090", Window: "5m"},
	}
	q, err := Resolve(spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if q.CPUCores != spec.Source.PromQL.CPUCores || q.MemoryBytes != spec.Source.PromQL.MemoryBytes {
		t.Fatalf("passthrough mutated queries: %+v", q)
	}
}

func TestResolveErrors(t *testing.T) {
	dangling := &symbiontv1alpha1.ShadowWorkloadSpec{
		Source:  symbiontv1alpha1.SourceSpec{Type: symbiontv1alpha1.SourceTypeDocker},
		Metrics: symbiontv1alpha1.MetricsConfig{Window: "5m"},
	}
	if _, err := Resolve(dangling); err == nil {
		t.Error("docker type without block should error")
	}
	badWindow := dockerSpec("cadvisor", "")
	badWindow.Metrics.Window = "bogus"
	if _, err := Resolve(badWindow); err == nil {
		t.Error("invalid window should error")
	}
}

func TestClientQuery(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantVal   float64
		wantFound bool
		wantErr   bool
	}{
		{
			name: "vector sample",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if err := r.Context().Err(); err != nil {
					return
				}
				if got := r.URL.Query().Get("query"); got != "up" {
					t.Errorf("expr not forwarded: %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"id":"/system.slice/docker-abc"},"value":[1755000000,"1.25"]}]}}`))
			},
			wantVal: 1.25, wantFound: true,
		},
		{
			name: "empty vector means workload off",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			},
			wantVal: 0, wantFound: false,
		},
		{
			name: "NaN sample treated as absent",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"NaN"]}]}}`))
			},
			wantFound: false,
		},
		{
			name: "prom error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
			},
			wantErr: true,
		},
		{
			name: "http 503",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			wantErr: true,
		},
		{
			name: "garbage json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>not prometheus</html>`))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			c, err := NewClient(srv.URL, 0)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			val, found, err := c.Query(context.Background(), "up")
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if val != tt.wantVal || found != tt.wantFound {
				t.Fatalf("got (%v,%v), want (%v,%v)", val, found, tt.wantVal, tt.wantFound)
			}
		})
	}
}

func TestQueryPairCombines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "cpu":
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"value":[0,"2"]}]}}`))
		case "mem":
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		default:
			t.Errorf("unexpected query %q", r.URL.Query().Get("query"))
		}
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, 0)
	cpu, mem, found, err := c.QueryPair(context.Background(), Queries{CPUCores: "cpu", MemoryBytes: "mem"})
	if err != nil {
		t.Fatalf("QueryPair: %v", err)
	}
	if cpu != 2 || mem != 0 || !found {
		t.Fatalf("got cpu=%v mem=%v found=%v, want 2/0/true", cpu, mem, found)
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("ftp://prom:9090", 0); err == nil {
		t.Error("non-http scheme should be rejected")
	}
}
