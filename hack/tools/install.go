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
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"k8s.io/klog/v2"
)

const (
	maxExtractedFileSize = 1 << 30
	awsTool              = "aws"
	awsCompleterCommand  = "aws_completer"
	helmTool             = "helm"
	kopsTool             = "kops"
)

type installer struct {
	cfg        *config
	lock       *lockFile
	binDir     string
	installed  map[string]bool
	pythonDone bool
}

func (i *installer) install(name string) error {
	tool, ok := i.cfg.Tools[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	if i.installed[name] {
		return nil
	}
	for _, dependency := range tool.Dependencies {
		if err := i.install(dependency); err != nil {
			return fmt.Errorf("install dependency %s: %w", dependency, err)
		}
	}
	klog.InfoS("Installing tool", "tool", name, "version", tool.Version)
	var err error
	switch {
	case name == awsTool:
		err = i.installAWS(tool, i.lock.Tools[name])
	case tool.Download != nil:
		err = i.installDownload(name, tool, i.lock.Tools[name])
	case tool.Go != nil:
		err = i.installGo(name, tool, i.lock.Tools[name])
	case tool.PyPI != nil:
		err = i.installPyPI(name)
	}
	if err != nil {
		return fmt.Errorf("install %s: %w", name, err)
	}
	i.installed[name] = true
	return nil
}

func (i *installer) installDownload(name string, tool toolDef, lock toolLock) error {
	target := platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	platformName := target.OS + "-" + target.Arch
	expected := lock.Downloads[platformName]
	if expected == "" {
		return fmt.Errorf("platform %s is not supported", platformName)
	}
	url, err := expand(tool.Download.URL, tool.Version, target, "")
	if err != nil {
		return err
	}
	artifact, err := i.download(url, expected)
	if err != nil {
		return err
	}
	defer removeWithLog(artifact)
	output := name
	if name == helmTool {
		output = ".helm"
	}
	destination := filepath.Join(i.binDir, output)
	if tool.Download.Path == "" {
		source, err := os.Open(artifact)
		if err != nil {
			return err
		}
		writeErr := writeExecutable(source, destination)
		if err := errors.Join(writeErr, source.Close()); err != nil {
			return err
		}
	} else {
		path, err := expand(tool.Download.Path, tool.Version, target, url)
		if err != nil {
			return err
		}
		if err := extractExecutable(artifact, url, path, destination); err != nil {
			return err
		}
	}
	if name == helmTool {
		return i.installWrapper("helm-runner.sh", helmTool)
	}
	return nil
}

func (i *installer) installGo(name string, tool toolDef, lock toolLock) error {
	sum, err := packageModuleSum(tool.Go.Package, tool.Version)
	if err != nil {
		return err
	}
	if sum != lock.Module {
		return fmt.Errorf("module sum is %s, expected %s", sum, lock.Module)
	}
	//nolint:gosec // The package path and version are read from the reviewed repository config.
	command := exec.CommandContext(context.Background(), "go", "install", tool.Go.Package+"@"+tool.Version)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), "GOBIN="+i.binDir)
	if err := command.Run(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(i.binDir, name)); err != nil {
		return fmt.Errorf("installed package did not create %s: %w", name, err)
	}
	return nil
}

func (i *installer) installPyPI(name string) error {
	if !i.pythonDone {
		if err := i.installPythonPackages(); err != nil {
			return err
		}
		i.pythonDone = true
	}
	if _, err := os.Stat(filepath.Join(i.binDir, "venv", "bin", name)); err != nil {
		return fmt.Errorf("package did not create command %s: %w", name, err)
	}
	return i.installWrapper("python-runner.sh", name)
}

func (i *installer) installPythonPackages() error {
	python, err := i.ensureVenv()
	if err != nil {
		return err
	}
	requirements, err := os.CreateTemp("", "tools-requirements-")
	if err != nil {
		return err
	}
	requirementsPath := requirements.Name()
	defer removeWithLog(requirementsPath)
	writer := bufio.NewWriter(requirements)
	names := make([]string, 0, len(i.lock.Python))
	for name := range i.lock.Python {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		requirement := i.lock.Python[name]
		if _, err := fmt.Fprintf(writer, "%s==%s", name, requirement.Version); err != nil {
			return err
		}
		for _, hash := range sortedValues(requirement.Files) {
			if _, err := fmt.Fprintf(writer, " \\\n    --hash=sha256:%s", hash); err != nil {
				return err
			}
		}
		if _, err := writer.WriteString("\n"); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := requirements.Close(); err != nil {
		return err
	}
	command := exec.CommandContext(context.Background(), python, "-m", "pip", "install", "--require-hashes", "-r", requirementsPath)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

func (i *installer) ensureVenv() (string, error) {
	python := filepath.Join(i.binDir, "venv", "bin", "python")
	if _, err := os.Stat(python); err == nil {
		return python, nil
	}
	commandName, err := findPython()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(i.binDir, 0o755); err != nil {
		return "", err
	}
	command := exec.CommandContext(context.Background(), commandName, "-m", "venv", filepath.Join(i.binDir, "venv"))
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), "VIRTUAL_ENV_DISABLE_PROMPT=1")
	if err := command.Run(); err != nil {
		return "", err
	}
	return python, nil
}

func (i *installer) installWrapper(sourceName, outputName string) error {
	source, err := os.Open(filepath.Join(toolsDir, sourceName))
	if err != nil {
		return err
	}
	return errors.Join(writeExecutable(source, filepath.Join(i.binDir, outputName)), source.Close())
}

func (i *installer) installAWS(tool toolDef, lock toolLock) error {
	target := platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	platformName := target.OS + "-" + target.Arch
	expected := lock.Downloads[platformName]
	if expected == "" {
		return fmt.Errorf("platform %s is not supported", platformName)
	}
	url, err := awsCLIURL(tool.Version, target)
	if err != nil {
		return err
	}
	artifact, err := i.download(url, expected)
	if err != nil {
		return err
	}
	defer removeWithLog(artifact)
	switch runtime.GOOS {
	case linuxOS:
		return i.installAWSLinux(artifact)
	case darwinOS:
		return i.installAWSDarwin(artifact)
	default:
		return fmt.Errorf("platform %s is not supported", platformName)
	}
}

func (i *installer) installAWSLinux(artifact string) error {
	tempDir, err := os.MkdirTemp("", "aws-cli-")
	if err != nil {
		return err
	}
	defer removeAllWithLog(tempDir)
	if err := extractZip(artifact, tempDir); err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(i.binDir, "aws-cli"),
		filepath.Join(i.binDir, awsTool),
		filepath.Join(i.binDir, awsCompleterCommand),
	} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	//nolint:gosec // The executable is from the checksum-verified AWS CLI archive.
	command := exec.CommandContext(
		context.Background(),
		filepath.Join(tempDir, "aws", "install"),
		"--install-dir", filepath.Join(i.binDir, "aws-cli"),
		"--bin-dir", i.binDir,
	)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (i *installer) installAWSDarwin(artifact string) error {
	tempDir, err := os.MkdirTemp("", "aws-cli-")
	if err != nil {
		return err
	}
	defer removeAllWithLog(tempDir)
	expanded := filepath.Join(tempDir, "expanded")
	command := exec.CommandContext(context.Background(), "/usr/sbin/pkgutil", "--expand-full", artifact, expanded)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	source, err := findAWSCLIRoot(expanded)
	if err != nil {
		return err
	}
	return i.copyAWSCLI(source)
}

func findAWSCLIRoot(root string) (string, error) {
	var result string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || entry.Name() != "aws-cli" {
			return nil
		}
		for _, name := range []string{awsTool, awsCompleterCommand} {
			info, err := os.Stat(filepath.Join(path, name))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
		}
		if result != "" {
			return errors.New("AWS CLI package contains multiple aws-cli directories")
		}
		result = path
		return filepath.SkipDir
	})
	if err != nil {
		return "", err
	}
	if result == "" {
		return "", errors.New("AWS CLI package does not contain an aws-cli directory")
	}
	return result, nil
}

func (i *installer) copyAWSCLI(source string) error {
	if err := os.MkdirAll(i.binDir, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(i.binDir, ".aws-cli-")
	if err != nil {
		return err
	}
	defer removeAllWithLog(staging)
	if err := os.CopyFS(staging, os.DirFS(source)); err != nil {
		return err
	}
	destination := filepath.Join(i.binDir, "aws-cli")
	for _, path := range []string{
		destination,
		filepath.Join(i.binDir, awsTool),
		filepath.Join(i.binDir, awsCompleterCommand),
	} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	for _, name := range []string{awsTool, awsCompleterCommand} {
		if err := os.Symlink(filepath.Join("aws-cli", name), filepath.Join(i.binDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) download(url, expected string) (string, error) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer closeWithLog(response.Body)
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, response.Status)
	}
	file, err := os.CreateTemp("", "tool-download-")
	if err != nil {
		return "", err
	}
	path := file.Name()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hash), response.Body); err != nil {
		closeWithLog(file)
		removeWithLog(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		removeWithLog(path)
		return "", err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		removeWithLog(path)
		return "", fmt.Errorf("download %s has SHA-256 %s, expected %s", url, actual, expected)
	}
	return path, nil
}

func extractExecutable(archivePath, archiveURL, sourcePath, destination string) error {
	switch {
	case strings.HasSuffix(archiveURL, ".zip"):
		return extractZipExecutable(archivePath, sourcePath, destination)
	case strings.HasSuffix(archiveURL, ".tar.gz"):
		file, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer closeWithLog(file)
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer closeWithLog(gzipReader)
		reader := tar.NewReader(gzipReader)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if strings.TrimPrefix(header.Name, "./") == strings.TrimPrefix(sourcePath, "./") {
				return writeExecutable(reader, destination)
			}
		}
		return fmt.Errorf("%s is not present in archive", sourcePath)
	default:
		return fmt.Errorf("unsupported archive %s", archiveURL)
	}
}

func extractZipExecutable(archivePath, sourcePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer closeWithLog(reader)
	for _, file := range reader.File {
		if strings.TrimPrefix(file.Name, "./") != strings.TrimPrefix(sourcePath, "./") {
			continue
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		return errors.Join(writeExecutable(source, destination), source.Close())
	}
	return fmt.Errorf("%s is not present in archive", sourcePath)
}

func writeExecutable(source io.Reader, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer removeWithLog(tempName)
	if err := copyLimited(temp, source); err != nil {
		closeWithLog(temp)
		return err
	}
	if err := temp.Chmod(0o755); err != nil {
		closeWithLog(temp)
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, destination)
}

func extractZip(archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer closeWithLog(reader)
	for _, file := range reader.File {
		path, err := safeArchivePath(destination, file.Name)
		if err != nil {
			return err
		}
		mode := file.Mode()
		switch {
		case file.FileInfo().IsDir():
			permissions := mode.Perm()
			if permissions == 0 {
				permissions = 0o755
			}
			if err := os.MkdirAll(path, permissions); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			return fmt.Errorf("archive contains unsupported symlink %q", file.Name)
		default:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			source, err := file.Open()
			if err != nil {
				return err
			}
			permissions := mode.Perm()
			if permissions == 0 {
				permissions = 0o644
			}
			destinationFile, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
			if err != nil {
				closeWithLog(source)
				return err
			}
			copyErr := copyLimited(destinationFile, source)
			closeErr := destinationFile.Close()
			sourceCloseErr := source.Close()
			if err := errors.Join(copyErr, closeErr, sourceCloseErr); err != nil {
				return err
			}
		}
	}
	return nil
}

func safeArchivePath(root, name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return filepath.Join(root, clean), nil
}

func findPython() (string, error) {
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("python3 or python is required")
}

func sortedValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func copyLimited(destination io.Writer, source io.Reader) error {
	written, err := io.Copy(destination, io.LimitReader(source, maxExtractedFileSize+1))
	if err != nil {
		return err
	}
	if written > maxExtractedFileSize {
		return fmt.Errorf("extracted file exceeds %d bytes", maxExtractedFileSize)
	}
	return nil
}

func closeWithLog(closer io.Closer) {
	if err := closer.Close(); err != nil {
		klog.ErrorS(err, "Failed to close resource")
	}
}

func removeWithLog(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		klog.ErrorS(err, "Failed to remove file", "path", path)
	}
}

func removeAllWithLog(path string) {
	if err := os.RemoveAll(path); err != nil {
		klog.ErrorS(err, "Failed to remove directory", "path", path)
	}
}
