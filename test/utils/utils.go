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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	certmanagerVersion = "v1.20.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
	certmanagerMaxSize = 16 << 20
	certmanagerRetries = 5

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

var certmanagerManifestPath string

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager using the exact manifest
// downloaded for this test run. Cleanup therefore does not depend on GitHub
// still being reachable after the suite finishes.
func UninstallCertManager() {
	if certmanagerManifestPath != "" {
		cmd := exec.Command("kubectl", "delete", "-f", certmanagerManifestPath)
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
		if err := os.Remove(certmanagerManifestPath); err != nil && !os.IsNotExist(err) {
			warnError(fmt.Errorf("remove cached cert-manager manifest: %w", err))
		}
		certmanagerManifestPath = ""
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd := exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the pinned cert-manager bundle. The manifest is
// downloaded before kubectl sees it, with bounded retries for transient release
// hosting/proxy failures.
func InstallCertManager() error {
	manifestPath, err := downloadCertManagerManifest()
	if err != nil {
		return err
	}
	certmanagerManifestPath = manifestPath
	cmd := exec.Command("kubectl", "apply", "-f", manifestPath)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err = Run(cmd)
	return err
}

func downloadCertManagerManifest() (string, error) {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error

	for attempt := 1; attempt <= certmanagerRetries; attempt++ {
		response, err := client.Get(url) // #nosec G107 -- pinned release URL controlled by this test binary.
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, certmanagerMaxSize+1))
			closeErr := response.Body.Close()
			switch {
			case response.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("download cert-manager manifest: HTTP %s", response.Status)
			case readErr != nil:
				lastErr = fmt.Errorf("read cert-manager manifest: %w", readErr)
			case closeErr != nil:
				lastErr = fmt.Errorf("close cert-manager response: %w", closeErr)
			case len(body) > certmanagerMaxSize:
				lastErr = fmt.Errorf("cert-manager manifest exceeds %d bytes", certmanagerMaxSize)
			case len(body) < 1024:
				lastErr = fmt.Errorf("cert-manager manifest is unexpectedly small: %d bytes", len(body))
			default:
				file, err := os.CreateTemp("", fmt.Sprintf("kube-symbiont-cert-manager-%s-*.yaml", certmanagerVersion))
				if err != nil {
					return "", fmt.Errorf("create cert-manager manifest cache: %w", err)
				}
				path := file.Name()
				if err := file.Chmod(0o600); err != nil {
					_ = file.Close()
					_ = os.Remove(path)
					return "", fmt.Errorf("secure cert-manager manifest cache: %w", err)
				}
				if _, err := file.Write(body); err != nil {
					_ = file.Close()
					_ = os.Remove(path)
					return "", fmt.Errorf("cache cert-manager manifest: %w", err)
				}
				if err := file.Close(); err != nil {
					_ = os.Remove(path)
					return "", fmt.Errorf("close cert-manager manifest cache: %w", err)
				}
				return path, nil
			}
		} else {
			lastErr = fmt.Errorf("download cert-manager manifest: %w", err)
		}

		if attempt < certmanagerRetries {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}

	return "", fmt.Errorf("cert-manager manifest unavailable after %d attempts: %w", certmanagerRetries, lastErr)
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncommented", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
