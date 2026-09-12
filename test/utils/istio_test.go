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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeTarEntryRelPath(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		want    string
		wantErr bool
	}{
		{
			name:  "benign nested path",
			entry: "istio-1.28.4/bin/istioctl",
			want:  "istio-1.28.4/bin/istioctl",
		},
		{
			name:  "skip empty",
			entry: "",
			want:  "",
		},
		{
			name:  "skip dot",
			entry: ".",
			want:  "",
		},
		{
			name:    "parent traversal",
			entry:   "../outside",
			wantErr: true,
		},
		{
			name:    "embedded traversal",
			entry:   "foo/../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path",
			entry:   "/tmp/evil",
			wantErr: true,
		},
		{
			name:    "backslash traversal",
			entry:   "..\\evil",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safeTarEntryRelPath(tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("safeTarEntryRelPath(%q) expected error", tt.entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("safeTarEntryRelPath(%q): %v", tt.entry, err)
			}
			if got != tt.want {
				t.Fatalf("safeTarEntryRelPath(%q) = %q, want %q", tt.entry, got, tt.want)
			}
		})
	}
}

func writeTar(t *testing.T, entries []struct {
	name    string
	content string
	isDir   bool
}) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name: e.name,
			Mode: 0o644,
		}
		if e.isDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if !e.isDir {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func extractTarBytes(t *testing.T, rootDir string, tarData []byte) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(bytes.NewReader(tarData))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := extractTarEntry(tr, hdr, root); err != nil {
			return err
		}
	}
}

func TestExtractTarEntry_safeExtraction(t *testing.T) {
	rootDir := t.TempDir()

	tarData := writeTar(t, []struct {
		name    string
		content string
		isDir   bool
	}{
		{name: "pkg/bin/tool", content: "hello", isDir: false},
	})

	if err := extractTarBytes(t, rootDir, tarData); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(rootDir, "pkg", "bin", "tool"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want hello", string(got))
	}
}

func TestExtractTarEntry_rejectsTraversal(t *testing.T) {
	rootDir := t.TempDir()
	parentDir := filepath.Dir(rootDir)
	outsidePath := filepath.Join(parentDir, "zipslip-outside")

	_ = os.Remove(outsidePath)

	tarData := writeTar(t, []struct {
		name    string
		content string
		isDir   bool
	}{
		{name: "../zipslip-outside", content: "pwned", isDir: false},
	})

	err := extractTarBytes(t, rootDir, tarData)
	if err == nil {
		t.Fatal("expected extraction to fail on traversal entry")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("expected traversal error, got: %v", err)
	}
	if _, statErr := os.Stat(outsidePath); statErr == nil {
		t.Fatalf("file was written outside extraction root: %s", outsidePath)
	}
}
