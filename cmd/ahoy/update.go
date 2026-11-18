package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultRepo = "AgiMaulana/Ahoy"

const updateUsage = `Update ahoy to the latest version (or a specified version).

Usage:
  ahoy update [flags] [version]

Flags:
  -check          check for available updates without installing
  -force          force update even if already on the latest version
  -version string target version to install (e.g. v0.0.5 or 0.0.5)`

func update(args []string) {
	fs := flag.NewFlagSet("ahoy update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, updateUsage) }

	checkOnly := fs.Bool("check", false, "check for available updates without installing")
	force := fs.Bool("force", false, "force update even if already on the latest version")
	versionFlag := fs.String("version", "", "target version to install")

	positionals, _, err := parseArgs(fs, args)
	if err != nil {
		fail(err)
	}
	if len(positionals) > 1 {
		fs.Usage()
		os.Exit(2)
	}

	targetVersion := *versionFlag
	if len(positionals) == 1 {
		if targetVersion != "" && normalizeTag(targetVersion) != normalizeTag(positionals[0]) {
			fail(fmt.Errorf("conflicting versions specified: flag %q and argument %q", targetVersion, positionals[0]))
		}
		targetVersion = positionals[0]
	}

	repo := os.Getenv("AHOY_REPO")
	if repo == "" {
		repo = defaultRepo
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var latestVersion string
	if targetVersion == "" || *checkOnly {
		latest, err := fetchLatestRelease(ctx, repo)
		if err != nil {
			fail(err)
		}
		latestVersion = latest
	}

	// Normalizing tags to "vX.Y.Z"
	if targetVersion != "" {
		targetVersion = normalizeTag(targetVersion)
	}

	isLatest := version != "dev" && isSameVersion(version, latestVersion)

	if *checkOnly {
		if version == "dev" {
			fmt.Printf("running development build (dev); latest release is %s\n", latestVersion)
			return
		}
		if isLatest {
			fmt.Printf("ahoy is already up to date (%s)\n", version)
		} else {
			fmt.Printf("a new version of ahoy is available: %s -> %s\nRun 'ahoy update' to install.\n", version, latestVersion)
		}
		return
	}

	if targetVersion == "" {
		if !*force && isLatest {
			fmt.Printf("ahoy is already up to date (%s). Use -force to reinstall.\n", version)
			return
		}
		targetVersion = latestVersion
	} else if !*force && isSameVersion(version, targetVersion) {
		fmt.Printf("ahoy is already on version %s. Use -force to reinstall.\n", version)
		return
	}

	if version == "dev" {
		fmt.Fprintf(os.Stderr, "ahoy: updating from dev to %s...\n", targetVersion)
	} else {
		fmt.Fprintf(os.Stderr, "ahoy: updating from %s to %s...\n", version, targetVersion)
	}

	if err := runInstaller(ctx, repo, targetVersion); err != nil {
		fail(err)
	}
}

func normalizeTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func isSameVersion(current, target string) bool {
	if current == "dev" || target == "" {
		return false
	}
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(target, "v")
}

func fetchLatestRelease(ctx context.Context, repo string) (string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ahoy-update")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
			loc := resp.Header.Get("Location")
			if loc != "" {
				if idx := strings.LastIndex(loc, "/tag/"); idx != -1 {
					tag := strings.TrimSpace(loc[idx+len("/tag/"):])
					if tag != "" {
						return tag, nil
					}
				}
			}
		}
	}

	// Fallback to GitHub Releases API if available
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	apiReq, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err == nil {
		apiReq.Header.Set("User-Agent", "ahoy-update")
		apiReq.Header.Set("Accept", "application/vnd.github.v3+json")
		apiClient := &http.Client{Timeout: 15 * time.Second}
		if apiResp, err := apiClient.Do(apiReq); err == nil {
			defer apiResp.Body.Close()
			if apiResp.StatusCode == http.StatusOK {
				var rel struct {
					TagName string `json:"tag_name"`
				}
				if err := json.NewDecoder(apiResp.Body).Decode(&rel); err == nil && rel.TagName != "" {
					return rel.TagName, nil
				}
			}
		}
	}

	return "", fmt.Errorf("could not determine latest release version for %s", repo)
}

func runInstaller(ctx context.Context, repo, targetVersion string) error {
	exePath, err := os.Executable()
	var installDir string
	if err == nil {
		if realPath, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = realPath
		}
		if !strings.Contains(exePath, "go-build") {
			installDir = filepath.Dir(exePath)
		}
	}

	scriptPath := os.Getenv("AHOY_INSTALL_SCRIPT")
	var tmpFile *os.File
	if scriptPath == "" {
		installURL := os.Getenv("AHOY_INSTALL_URL")
		if installURL == "" {
			installURL = fmt.Sprintf("https://raw.githubusercontent.com/%s/main/install.sh", repo)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, installURL, nil)
		if err != nil {
			return fmt.Errorf("invalid installer URL: %w", err)
		}
		req.Header.Set("User-Agent", "ahoy-update/"+version)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to fetch installer: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to fetch installer from %s: HTTP %d", installURL, resp.StatusCode)
		}

		tmpFile, err = os.CreateTemp("", "ahoy-install-*.sh")
		if err != nil {
			return fmt.Errorf("failed to create temporary file for installer: %w", err)
		}
		defer os.Remove(tmpFile.Name())

		if _, err := io.Copy(tmpFile, resp.Body); err != nil {
			tmpFile.Close()
			return fmt.Errorf("failed to write installer script: %w", err)
		}
		if err := tmpFile.Close(); err != nil {
			return fmt.Errorf("failed to close installer script: %w", err)
		}
		if err := os.Chmod(tmpFile.Name(), 0700); err != nil {
			return fmt.Errorf("failed to make installer executable: %w", err)
		}
		scriptPath = tmpFile.Name()
	}

	shPath, err := exec.LookPath("sh")
	if err != nil {
		return fmt.Errorf("'sh' is required to run the installer: %w", err)
	}

	var cmdArgs []string
	cmdArgs = append(cmdArgs, scriptPath)
	if targetVersion != "" {
		cmdArgs = append(cmdArgs, targetVersion)
	}

	cmd := exec.CommandContext(ctx, shPath, cmdArgs...)
	cmd.Env = os.Environ()
	if installDir != "" && os.Getenv("AHOY_INSTALL_DIR") == "" {
		cmd.Env = append(cmd.Env, "AHOY_INSTALL_DIR="+installDir)
	}
	if os.Getenv("AHOY_REPO") == "" {
		cmd.Env = append(cmd.Env, "AHOY_REPO="+repo)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer failed: %w", err)
	}
	return nil
}
