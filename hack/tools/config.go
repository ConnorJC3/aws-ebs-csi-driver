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
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const yamlLicenseHeader = `# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the 'License');
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an 'AS IS' BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

`

const (
	linuxOS  = "linux"
	darwinOS = "darwin"
	amd64    = "amd64"
	arm64    = "arm64"
)

type config struct {
	Tools map[string]toolDef `yaml:"tools"`
}

type toolDef struct {
	Version      string       `yaml:"version"`
	Dependencies []string     `yaml:"dependencies,omitempty"`
	Download     *downloadDef `yaml:"download,omitempty"`
	Go           *goDef       `yaml:"go,omitempty"`
	PyPI         *pypiDef     `yaml:"pypi,omitempty"`
}

type downloadDef struct {
	URL      string `yaml:"url,omitempty"`
	Checksum string `yaml:"checksum,omitempty"`
	Path     string `yaml:"path,omitempty"`
	GitHub   string `yaml:"github,omitempty"`
}

type goDef struct {
	Package string `yaml:"package"`
}

type pypiDef struct {
	Package string `yaml:"package"`
}

type lockFile struct {
	Tools  map[string]toolLock `yaml:"tools"`
	Python map[string]pypiLock `yaml:"python,omitempty"`
}

type toolLock struct {
	Version   string            `yaml:"version"`
	Downloads map[string]string `yaml:"downloads,omitempty"`
	Module    string            `yaml:"module,omitempty"`
}

type pypiLock struct {
	Version string            `yaml:"version"`
	Files   map[string]string `yaml:"files"`
}

type platform struct {
	OS   string
	Arch string
}

func loadConfig(path string) (*config, error) {
	var cfg config
	if err := loadYAML(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func loadLock(path string) (*lockFile, error) {
	var lock lockFile
	if err := loadYAML(path, &lock); err != nil {
		return nil, err
	}
	return &lock, nil
}

func loadYAML(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	return errors.Join(decoder.Decode(value), file.Close())
}

func (cfg *config) validate() error {
	for name, tool := range cfg.Tools {
		if tool.Version == "" {
			return fmt.Errorf("tool %q has no version", name)
		}
		sources := 0
		if tool.Download != nil {
			sources++
			if name != awsTool && tool.Download.URL == "" {
				return fmt.Errorf("tool %q has no download URL", name)
			}
		}
		if tool.Go != nil {
			sources++
			if tool.Go.Package == "" {
				return fmt.Errorf("tool %q has no Go package", name)
			}
		}
		if tool.PyPI != nil {
			sources++
			if tool.PyPI.Package == "" {
				return fmt.Errorf("tool %q has no PyPI package", name)
			}
		}
		if sources != 1 {
			return fmt.Errorf("tool %q must have exactly one source", name)
		}
		for _, dependency := range tool.Dependencies {
			if _, ok := cfg.Tools[dependency]; !ok {
				return fmt.Errorf("tool %q has unknown dependency %q", name, dependency)
			}
		}
	}
	return cfg.checkCycles()
}

func (cfg *config) checkCycles() error {
	state := make(map[string]int, len(cfg.Tools))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("dependency cycle contains %q", name)
		case 2:
			return nil
		}
		state[name] = 1
		for _, dependency := range cfg.Tools[name].Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for name := range cfg.Tools {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func (lock *lockFile) validate(cfg *config) error {
	for name, python := range lock.Python {
		if python.Version == "" {
			return fmt.Errorf("python package %q has no version", name)
		}
		if len(python.Files) == 0 {
			return fmt.Errorf("python package %q has no distribution hashes", name)
		}
	}
	for name, tool := range cfg.Tools {
		entry, ok := lock.Tools[name]
		if !ok {
			return fmt.Errorf("tool %q is missing from the lock file", name)
		}
		if entry.Version != tool.Version {
			return fmt.Errorf("tool %q is locked at %q, not %q", name, entry.Version, tool.Version)
		}
		switch {
		case tool.Download != nil:
			for _, value := range supportedPlatforms() {
				if entry.Downloads[value] == "" {
					return fmt.Errorf("tool %q has no checksum for %s", name, value)
				}
			}
		case tool.Go != nil:
			if entry.Module == "" {
				return fmt.Errorf("tool %q has no module sum", name)
			}
		case tool.PyPI != nil:
			python, ok := lock.Python[normalizePythonPackage(tool.PyPI.Package)]
			if !ok {
				return fmt.Errorf("tool %q is missing from the Python lock", name)
			}
			if python.Version != tool.Version {
				return fmt.Errorf("tool %q has Python version %q, not %q", name, python.Version, tool.Version)
			}
			if len(python.Files) == 0 {
				return fmt.Errorf("tool %q has no Python distribution hashes", name)
			}
		}
	}
	return nil
}

func normalizePythonPackage(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(value rune) bool {
		return value == '.' || value == '-' || value == '_'
	}), "-")
}

func parsePlatform(value string) (platform, error) {
	osName, arch, ok := strings.Cut(value, "-")
	if !ok || osName == "" || arch == "" {
		return platform{}, fmt.Errorf("invalid platform %q", value)
	}
	return platform{OS: osName, Arch: arch}, nil
}

func expand(value, version string, target platform, downloadURL string) (string, error) {
	replacer := strings.NewReplacer(
		"{version}", version,
		"{version_no_v}", strings.TrimPrefix(version, "v"),
		"{os}", target.OS,
		"{os_title}", title(target.OS),
		"{arch}", target.Arch,
		"{url}", downloadURL,
	)
	result := replacer.Replace(value)
	if strings.Contains(result, "{") || strings.Contains(result, "}") {
		return "", fmt.Errorf("unknown template value in %q", value)
	}
	return result, nil
}

func awsCLIURL(version string, target platform) (string, error) {
	const baseURL = "https://awscli.amazonaws.com/"
	switch {
	case target.OS == linuxOS && target.Arch == amd64:
		return fmt.Sprintf("%sawscli-exe-linux-x86_64-%s.zip", baseURL, version), nil
	case target.OS == linuxOS && target.Arch == arm64:
		return fmt.Sprintf("%sawscli-exe-linux-aarch64-%s.zip", baseURL, version), nil
	case target.OS == darwinOS && (target.Arch == amd64 || target.Arch == arm64):
		return fmt.Sprintf("%sAWSCLIV2-%s.pkg", baseURL, version), nil
	default:
		return "", fmt.Errorf("AWS CLI does not support %s-%s", target.OS, target.Arch)
	}
}

func supportedPlatforms() []string {
	return []string{
		linuxOS + "-" + amd64,
		linuxOS + "-" + arm64,
		darwinOS + "-" + amd64,
		darwinOS + "-" + arm64,
	}
}

func title(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func writeYAML(path string, value any) error {
	data := bytes.NewBufferString(yamlLicenseHeader)
	encoder := yaml.NewEncoder(data)
	encoder.SetIndent(2)
	encodeErr := encoder.Encode(value)
	if err := errors.Join(encodeErr, encoder.Close()); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer removeWithLog(tempName)
	if _, err := temp.Write(data.Bytes()); err != nil {
		closeWithLog(temp)
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		closeWithLog(temp)
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func sortedToolNames(cfg *config) []string {
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
