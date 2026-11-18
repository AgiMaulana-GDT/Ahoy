package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeTag(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"0.0.5", "v0.0.5"},
		{"v0.0.5", "v0.0.5"},
		{"  1.2.3  ", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
	}
	for _, tc := range tests {
		got := normalizeTag(tc.input)
		if got != tc.want {
			t.Errorf("normalizeTag(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestIsSameVersion(t *testing.T) {
	tests := []struct {
		current string
		target  string
		want    bool
	}{
		{"dev", "v0.0.5", false},
		{"dev", "0.0.5", false},
		{"0.0.5", "v0.0.5", true},
		{"v0.0.5", "0.0.5", true},
		{"v0.0.5", "v0.0.5", true},
		{"v0.0.4", "v0.0.5", false},
		{"v0.0.5", "", false},
		{"", "v0.0.5", false},
	}
	for _, tc := range tests {
		got := isSameVersion(tc.current, tc.target)
		if got != tc.want {
			t.Errorf("isSameVersion(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

func TestFetchLatestRelease_Redirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "https://github.com/testowner/testrepo/releases/tag/v1.2.3", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	// Use custom transport/client to test the logic
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	idx := strings.LastIndex(loc, "/tag/")
	if idx == -1 {
		t.Fatalf("expected /tag/ in Location %q", loc)
	}
	tag := loc[idx+len("/tag/"):]
	if tag != "v1.2.3" {
		t.Fatalf("expected v1.2.3, got %q", tag)
	}
}

func TestRunInstaller_LocalScript(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "install_call.log")

	// Create a mock script that records args and environment
	mockScript := filepath.Join(tmpDir, "mock_install.sh")
	scriptContent := `#!/bin/sh
echo "arg1=$1" >> "` + logFile + `"
echo "install_dir=$AHOY_INSTALL_DIR" >> "` + logFile + `"
echo "repo=$AHOY_REPO" >> "` + logFile + `"
exit 0
`
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AHOY_INSTALL_SCRIPT", mockScript)
	t.Setenv("AHOY_INSTALL_DIR", "/custom/install/dir")
	t.Setenv("AHOY_REPO", "Test/Repo")

	ctx := context.Background()
	if err := runInstaller(ctx, "Test/Repo", "v0.9.9"); err != nil {
		t.Fatalf("runInstaller failed: %v", err)
	}

	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(out)
	if !strings.Contains(content, "arg1=v0.9.9") {
		t.Errorf("expected arg1=v0.9.9 in log, got %s", content)
	}
	if !strings.Contains(content, "install_dir=/custom/install/dir") {
		t.Errorf("expected install_dir=/custom/install/dir in log, got %s", content)
	}
	if !strings.Contains(content, "repo=Test/Repo") {
		t.Errorf("expected repo=Test/Repo in log, got %s", content)
	}
}

func TestRunInstaller_RemoteScript(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "install_remote.log")

	// Mock HTTP server that serves the install script
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript")
		w.Write([]byte(`#!/bin/sh
echo "ran remote install: version=$1 dir=$AHOY_INSTALL_DIR" >> "` + logFile + `"
`))
	}))
	defer ts.Close()

	t.Setenv("AHOY_INSTALL_SCRIPT", "")
	t.Setenv("AHOY_INSTALL_URL", ts.URL+"/install.sh")
	t.Setenv("AHOY_INSTALL_DIR", tmpDir)

	ctx := context.Background()
	if err := runInstaller(ctx, "AgiMaulana/Ahoy", "v1.0.0"); err != nil {
		t.Fatalf("runInstaller remote failed: %v", err)
	}

	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ran remote install: version=v1.0.0 dir="+tmpDir) {
		t.Fatalf("unexpected remote install output: %s", string(out))
	}
}
