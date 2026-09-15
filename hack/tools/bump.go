/*
Copyright 2026 The Kubernetes Authors.

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

package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
	"k8s.io/klog/v2"
)

var sha256Pattern = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

func bumpVersions(cfg *config) error {
	for _, name := range sortedToolNames(cfg) {
		tool := cfg.Tools[name]
		klog.InfoS("Checking tool", "tool", name, "version", tool.Version)
		version, err := latestVersion(name, tool)
		if err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if version != tool.Version {
			if versionIsOlder(version, tool.Version) {
				return fmt.Errorf("latest %s version %s is older than configured version %s", name, version, tool.Version)
			}
			klog.InfoS("Bumping tool", "tool", name, "oldVersion", tool.Version, "newVersion", version)
			tool.Version = version
			cfg.Tools[name] = tool
		}
	}
	return nil
}

func latestVersion(name string, tool toolDef) (string, error) {
	switch {
	case tool.Go != nil:
		module, err := moduleForPackage(tool.Go.Package, tool.Version)
		if err != nil {
			return "", err
		}
		var result struct {
			Version string `json:"version"`
			Error   *struct {
				Err string `json:"err"`
			} `json:"error"`
		}
		//nolint:gosec // The package path is read from the reviewed repository config.
		command := exec.CommandContext(context.Background(), "go", "list", "-m", "-json", module+"@latest")
		data, err := command.Output()
		if err != nil {
			return "", commandError(err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return "", err
		}
		if result.Error != nil {
			return "", errors.New(result.Error.Err)
		}
		return result.Version, nil
	case tool.PyPI != nil:
		return latestPyPIVersion(tool.PyPI.Package, tool.Version)
	case tool.Download != nil:
		if tool.Download.GitHub == "" {
			return "", errors.New("download has no GitHub version source")
		}
		return latestGitHubVersion(name, tool.Download.GitHub)
	default:
		return "", errors.New("tool has no source")
	}
}

func latestPyPIVersion(packageName, currentVersion string) (string, error) {
	var result struct {
		Releases map[string][]struct {
			RequiresPython string `json:"requires_python"` //nolint:tagliatelle // PyPI uses snake case.
			Yanked         bool   `json:"yanked"`
		} `json:"releases"`
	}
	endpoint := "https://pypi.org/pypi/" + url.PathEscape(packageName) + "/json"
	if err := getJSON(endpoint, &result); err != nil {
		return "", err
	}
	data, err := json.Marshal(result.Releases)
	if err != nil {
		return "", err
	}
	python, err := findPython()
	if err != nil {
		return "", err
	}
	const script = `
import json
import sys
from pip._vendor.packaging.specifiers import InvalidSpecifier, SpecifierSet
from pip._vendor.packaging.version import InvalidVersion, Version

current = Version(".".join(str(value) for value in sys.version_info[:3]))
configured = Version(sys.argv[1])
compatible = []
for release, files in json.load(sys.stdin).items():
    try:
        version = Version(release)
    except InvalidVersion:
        continue
    if version.is_prerelease:
        continue
    for file in files:
        if file["yanked"]:
            continue
        try:
            supported = not file["requires_python"] or SpecifierSet(file["requires_python"]).contains(current)
        except InvalidSpecifier:
            continue
        if supported:
            compatible.append(version)
            break
if not compatible:
    raise SystemExit("no compatible release found")
latest = max(compatible)
if latest < configured:
    raise SystemExit(f"latest compatible version {latest} is older than configured version {configured}")
print(latest)
`
	command := exec.CommandContext(context.Background(), python, "-c", script, currentVersion)
	command.Stdin = strings.NewReader(string(data))
	output, err := command.Output()
	if err != nil {
		return "", commandError(err)
	}
	return strings.TrimSpace(string(output)), nil
}

func latestGitHubVersion(name, repository string) (string, error) {
	base := "https://api.github.com/repos/" + repository
	if name == awsTool {
		var tags []struct {
			Name string `json:"name"`
		}
		if err := getJSON(base+"/tags?per_page=100", &tags); err != nil {
			return "", err
		}
		latest := ""
		for _, tag := range tags {
			version := tag.Name
			if !strings.HasPrefix(version, "v") {
				version = "v" + version
			}
			if !semver.IsValid(version) {
				continue
			}
			if latest == "" || versionIsOlder(latest, tag.Name) {
				latest = tag.Name
			}
		}
		if latest == "" {
			return "", errors.New("repository has no semantic version tags")
		}
		return latest, nil
	}
	if name == kopsTool {
		var releases []struct {
			TagName string `json:"tag_name"` //nolint:tagliatelle // GitHub uses snake case.
			Draft   bool   `json:"draft"`
		}
		if err := getJSON(base+"/releases?per_page=20", &releases); err != nil {
			return "", err
		}
		for _, release := range releases {
			if !release.Draft {
				return release.TagName, nil
			}
		}
		return "", errors.New("repository has no published releases")
	}
	var release struct {
		TagName string `json:"tag_name"` //nolint:tagliatelle // GitHub uses snake case.
	}
	if err := getJSON(base+"/releases/latest", &release); err != nil {
		return "", err
	}
	return release.TagName, nil
}

func versionIsOlder(candidate, current string) bool {
	if !strings.HasPrefix(candidate, "v") {
		candidate = "v" + candidate
	}
	if !strings.HasPrefix(current, "v") {
		current = "v" + current
	}
	return semver.IsValid(candidate) && semver.IsValid(current) && semver.Compare(candidate, current) < 0
}

func buildLock(cfg *config) (*lockFile, error) {
	lock := &lockFile{Tools: make(map[string]toolLock, len(cfg.Tools))}
	var pythonRequirements []string
	for _, name := range sortedToolNames(cfg) {
		tool := cfg.Tools[name]
		klog.InfoS("Locking tool", "tool", name, "version", tool.Version)
		entry := toolLock{Version: tool.Version}
		var err error
		switch {
		case tool.Download != nil:
			entry.Downloads, err = lockDownloads(name, tool)
		case tool.Go != nil:
			entry.Module, err = packageModuleSum(tool.Go.Package, tool.Version)
		case tool.PyPI != nil:
			pythonRequirements = append(pythonRequirements, tool.PyPI.Package+"=="+tool.Version)
		}
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", name, err)
		}
		lock.Tools[name] = entry
	}
	if len(pythonRequirements) != 0 {
		var err error
		lock.Python, err = lockPyPIRequirements(pythonRequirements)
		if err != nil {
			return nil, fmt.Errorf("lock Python requirements: %w", err)
		}
	}
	return lock, nil
}

func lockDownloads(name string, tool toolDef) (map[string]string, error) {
	result := make(map[string]string)
	hashes := make(map[string]string)
	for _, value := range supportedPlatforms() {
		target, err := parsePlatform(value)
		if err != nil {
			return nil, err
		}
		var downloadURL string
		if name == awsTool {
			downloadURL, err = awsCLIURL(tool.Version, target)
		} else {
			downloadURL, err = expand(tool.Download.URL, tool.Version, target, "")
		}
		if err != nil {
			return nil, err
		}
		if hash := hashes[downloadURL]; hash != "" {
			result[value] = hash
			continue
		}
		var hash string
		if tool.Download.Checksum == "" {
			hash, err = hashURL(downloadURL)
		} else {
			checksumURL, expandErr := expand(tool.Download.Checksum, tool.Version, target, downloadURL)
			if expandErr != nil {
				return nil, expandErr
			}
			hash, err = checksumFromURL(checksumURL, path.Base(downloadURL))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", value, err)
		}
		hashes[downloadURL] = hash
		result[value] = hash
	}
	return result, nil
}

func lockPyPIRequirements(requirements []string) (map[string]pypiLock, error) {
	python, err := findPython()
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "tools-python-lock-")
	if err != nil {
		return nil, err
	}
	defer removeAllWithLog(tempDir)
	args := []string{
		"-m", "pip", "download",
		"--disable-pip-version-check",
		"--no-cache-dir",
		"--only-binary=:all:",
		"--dest", tempDir,
	}
	args = append(args, requirements...)
	command := exec.CommandContext(context.Background(), python, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(tempDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]pypiLock, len(files))
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".whl" {
			continue
		}
		name, version, err := readWheelMetadata(filepath.Join(tempDir, file.Name()))
		if err != nil {
			return nil, err
		}
		key := normalizePythonPackage(name)
		if old, ok := result[key]; ok && old.Version != version {
			return nil, fmt.Errorf("pip resolved %s at both %s and %s", name, old.Version, version)
		}
		hashes, err := lockPyPIRelease(name, version)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", name, version, err)
		}
		result[key] = pypiLock{Version: version, Files: hashes}
	}
	if len(result) == 0 {
		return nil, errors.New("pip resolved no Python packages")
	}
	return result, nil
}

func readWheelMetadata(path string) (string, string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return "", "", err
	}
	defer closeWithLog(reader)
	for _, file := range reader.File {
		if !strings.HasSuffix(file.Name, ".dist-info/METADATA") {
			continue
		}
		source, err := file.Open()
		if err != nil {
			return "", "", err
		}
		message, readErr := mail.ReadMessage(source)
		closeErr := source.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", "", err
		}
		name := message.Header.Get("Name")
		version := message.Header.Get("Version")
		if name == "" || version == "" {
			return "", "", fmt.Errorf("%s has incomplete package metadata", path)
		}
		return name, version, nil
	}
	return "", "", fmt.Errorf("%s has no package metadata", path)
}

func lockPyPIRelease(packageName, version string) (map[string]string, error) {
	var result struct {
		URLs []struct {
			Filename string `json:"filename"`
			Digests  struct {
				SHA256 string `json:"sha256"`
			} `json:"digests"`
		} `json:"urls"`
	}
	endpoint := fmt.Sprintf(
		"https://pypi.org/pypi/%s/%s/json",
		url.PathEscape(packageName),
		url.PathEscape(version),
	)
	if err := getJSON(endpoint, &result); err != nil {
		return nil, err
	}
	files := make(map[string]string, len(result.URLs))
	for _, file := range result.URLs {
		if file.Filename == "" || file.Digests.SHA256 == "" {
			continue
		}
		if old, ok := files[file.Filename]; ok && old != file.Digests.SHA256 {
			return nil, fmt.Errorf("PyPI returned conflicting hashes for %s", file.Filename)
		}
		files[file.Filename] = file.Digests.SHA256
	}
	if len(files) == 0 {
		return nil, errors.New("PyPI release has no hashed files")
	}
	return files, nil
}

func moduleSum(module, version string) (string, error) {
	var result struct {
		Sum   string  `json:"sum"`
		Error *string `json:"error"`
	}
	//nolint:gosec // The module path is inferred from the reviewed repository config.
	command := exec.CommandContext(context.Background(), "go", "mod", "download", "-json", module+"@"+version)
	data, err := command.Output()
	if err != nil {
		return "", commandError(err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if result.Error != nil {
		return "", errors.New(*result.Error)
	}
	if result.Sum == "" {
		return "", errors.New("module download returned no sum")
	}
	return result.Sum, nil
}

func packageModuleSum(packagePath, version string) (string, error) {
	module, err := moduleForPackage(packagePath, version)
	if err != nil {
		return "", err
	}
	return moduleSum(module, version)
}

func moduleForPackage(packagePath, version string) (string, error) {
	candidate := strings.TrimSuffix(packagePath, "/...")
	for strings.Contains(candidate, "/") {
		var result struct {
			Path string `json:"path"`
		}
		//nolint:gosec // The package path and version are read from the reviewed repository config.
		command := exec.CommandContext(context.Background(), "go", "list", "-m", "-json", candidate+"@"+version)
		data, err := command.Output()
		if err == nil {
			if err := json.Unmarshal(data, &result); err != nil {
				return "", err
			}
			if result.Path == "" {
				return "", errors.New("module query returned no path")
			}
			return result.Path, nil
		}
		candidate = candidate[:strings.LastIndex(candidate, "/")]
	}
	return "", fmt.Errorf("no module contains package %s at %s", packagePath, version)
}

func hashURL(value string) (string, error) {
	response, err := get(value)
	if err != nil {
		return "", err
	}
	defer closeWithLog(response.Body)
	hash := sha256.New()
	if _, err := io.Copy(hash, response.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func checksumFromURL(value, filename string) (string, error) {
	response, err := get(value)
	if err != nil {
		return "", err
	}
	defer closeWithLog(response.Body)
	scanner := bufio.NewScanner(response.Body)
	var onlyHash string
	lineCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		match := sha256Pattern.FindString(line)
		if match == "" {
			continue
		}
		lineCount++
		onlyHash = strings.ToLower(match)
		fields := strings.Fields(line)
		if len(fields) > 1 && path.Base(strings.TrimPrefix(fields[len(fields)-1], "*")) == filename {
			return onlyHash, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if lineCount == 1 {
		return onlyHash, nil
	}
	return "", fmt.Errorf("checksum for %s not found in %s", filename, value)
}

func getJSON(value string, result any) error {
	response, err := get(value)
	if err != nil {
		return err
	}
	defer closeWithLog(response.Body)
	return json.NewDecoder(response.Body).Decode(result)
}

func get(value string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, value, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "aws-ebs-csi-driver-tools")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		closeWithLog(response.Body)
		return nil, fmt.Errorf("GET %s: %s", value, response.Status)
	}
	return response, nil
}

func commandError(err error) error {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && len(exitError.Stderr) != 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitError.Stderr)))
	}
	return err
}
