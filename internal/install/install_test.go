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

package install_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const systemNamespace = "kube-symbiont-system"

func readInstaller(t *testing.T, path string) []unstructured.Unstructured {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open installer: %v", err)
	}
	defer f.Close() //nolint:errcheck

	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	objects := make([]unstructured.Unstructured, 0, 16)
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode installer: %v", err)
		}
		if obj.Object == nil {
			continue
		}
		objects = append(objects, obj)
	}
	return objects
}

func assertVersionedManagerImage(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object,
		"spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("deployment has no manager container: %v", err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("manager container has unexpected shape %T", containers[0])
	}
	image, _ := container["image"].(string)
	if image == "" || image == "controller:latest" {
		t.Fatalf("installer deployment is not versioned: %q", image)
	}
}

func assertPrometheusDestinationPolicy(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object,
		"spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("deployment has no manager container: %v", err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("manager container has unexpected shape %T", containers[0])
	}
	args, ok := container["args"].([]any)
	if !ok {
		t.Fatalf("manager args have unexpected shape %T", container["args"])
	}
	want := "--prometheus-allowed-destination=http://kube-prometheus-stack-prometheus.monitoring:9090"
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Errorf("installer manager does not declare default Prometheus destination %q", want)
}

func assertBindingNamespaces(t *testing.T, key string, obj *unstructured.Unstructured) {
	t.Helper()
	subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
	if err != nil {
		t.Fatalf("read %s subjects: %v", key, err)
	}
	for _, rawSubject := range subjects {
		subject, ok := rawSubject.(map[string]any)
		if ok && subject["kind"] == "ServiceAccount" && subject["namespace"] != systemNamespace {
			t.Errorf("%s service-account subject uses namespace %v", key, subject["namespace"])
		}
	}
}

func TestInstallerContainsCoherentFreshInstall(t *testing.T) {
	path := filepath.Join("..", "..", "dist", "install.yaml")
	objects := readInstaller(t, path)

	required := map[string]bool{
		"Namespace/" + systemNamespace:                                     false,
		"PriorityClass/symbiont-ballast":                                   false,
		"CustomResourceDefinition/shadowworkloads.symbiont.tensorhost.com": false,
		"ServiceAccount/kube-symbiont-controller-manager":                  false,
		"ClusterRole/kube-symbiont-manager-role":                           false,
		"ClusterRoleBinding/kube-symbiont-manager-rolebinding":             false,
		"Deployment/kube-symbiont-controller-manager":                      false,
	}
	namespaceIndex, deploymentIndex := -1, -1
	for i := range objects {
		obj := &objects[i]
		key := obj.GetKind() + "/" + obj.GetName()
		if _, ok := required[key]; ok {
			required[key] = true
		}
		if obj.GetKind() == "Namespace" && obj.GetName() == systemNamespace {
			namespaceIndex = i
		}
		if obj.GetKind() == "Deployment" && obj.GetName() == "kube-symbiont-controller-manager" {
			deploymentIndex = i
			assertVersionedManagerImage(t, obj)
			assertPrometheusDestinationPolicy(t, obj)
		}
		if ns := obj.GetNamespace(); ns != "" && ns != systemNamespace {
			t.Errorf("%s uses namespace %q, want %q", key, ns, systemNamespace)
		}
		if obj.GetKind() == "ClusterRoleBinding" {
			assertBindingNamespaces(t, key, obj)
		}
	}

	for key, found := range required {
		if !found {
			t.Errorf("installer missing required %s", key)
		}
	}
	if namespaceIndex < 0 || deploymentIndex < 0 || namespaceIndex > deploymentIndex {
		t.Errorf("namespace must precede deployment: namespace index=%d deployment index=%d", namespaceIndex, deploymentIndex)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	if strings.Contains(string(data), "namespace: kube-symbiont\n") {
		t.Error("installer contains obsolete kube-symbiont namespace")
	}
}
