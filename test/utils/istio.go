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
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DetectOSArch returns the OS and architecture strings used in Istio release artifact names.
// Istio uses "osx" for macOS, "linux" for Linux; "amd64" or "arm64" for architecture.
func DetectOSArch() (osName, arch string) {
	switch runtime.GOOS {
	case "darwin":
		osName = "osx"
	case "linux": //nolint:goconst
		osName = "linux"
	default:
		osName = "linux"
	}
	switch runtime.GOARCH {
	case "arm64", "aarch64":
		arch = "arm64"
	default:
		arch = "amd64"
	}
	return osName, arch
}

// VersionToRevision converts an Istio version like "1.28.4" to a revision name "1-28-4".
func VersionToRevision(version string) string {
	return strings.ReplaceAll(version, ".", "-")
}

// DownloadIstio downloads an Istio release from GitHub, extracts it to tmpDir, and returns
// the path to the extracted directory (e.g. tmpDir/istio-1.28.4).
func DownloadIstio(version, tmpDir string) (istioDir string, err error) {
	osName, arch := DetectOSArch()
	url := fmt.Sprintf("https://github.com/istio/istio/releases/download/%s/istio-%s-%s-%s.tar.gz",
		version, version, osName, arch)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download Istio %s: %w", version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download Istio %s: HTTP %d", version, resp.StatusCode)
	}

	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read tar: %w", err)
		}

		target := filepath.Join(tmpDir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", fmt.Errorf("failed to create dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("failed to create parent dir for %s: %w", target, err)
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return "", fmt.Errorf("failed to create file %s: %w", target, err)
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				_ = outFile.Close()
				return "", fmt.Errorf("failed to write file %s: %w", target, err)
			}
			_ = outFile.Close()
		}
	}

	istioDir = filepath.Join(tmpDir, fmt.Sprintf("istio-%s", version))
	if _, err := os.Stat(istioDir); os.IsNotExist(err) {
		return "", fmt.Errorf("extracted Istio dir not found: %s", istioDir)
	}
	return istioDir, nil
}

// GetIstioctlPath returns the path to the istioctl binary within an extracted Istio directory.
func GetIstioctlPath(istioDir string) string {
	return filepath.Join(istioDir, "bin", "istioctl")
}

// RunIstioInstall runs istioctl install with the given revision and profile.
// If revision is empty, the default revision is used (no --set revision).
func RunIstioInstall(istioctl, revision, profile string) error {
	args := []string{"install", "-y", "--set", "profile=" + profile}
	if revision != "" {
		args = append(args, "--set", "revision="+revision)
	}
	cmd := exec.Command(istioctl, args...)
	_, err := Run(cmd)
	return err
}

// RunIstioUpgrade runs istioctl upgrade with the given profile.
func RunIstioUpgrade(istioctl, profile string) error {
	cmd := exec.Command(istioctl, "upgrade", "-y", "--set", "profile="+profile)
	_, err := Run(cmd)
	return err
}

// RunIstioTagSet runs istioctl tag set to associate a tag with a revision.
func RunIstioTagSet(istioctl, tag, revision string, overwrite bool) error {
	args := []string{"tag", "set", tag, "--revision", revision}
	if overwrite {
		args = append(args, "--overwrite")
	}
	cmd := exec.Command(istioctl, args...)
	_, err := Run(cmd)
	return err
}

// WaitForIstioReady waits for the istiod deployment to be available in istio-system.
// For revisioned installs (e.g. revision "1-28-4"), pass the revision; for default install, pass "".
func WaitForIstioReady(revision string) error {
	deployName := "istiod"
	if revision != "" {
		deployName = "istiod-" + revision
	}
	cmd := exec.Command("kubectl", "wait", "deployment/"+deployName,
		"--for", "condition=Available",
		"-n", "istio-system",
		"--timeout", "120s")
	_, err := Run(cmd)
	return err
}
