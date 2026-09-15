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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	toolsDir      = "hack/tools"
	defaultConfig = toolsDir + "/tools.yaml"
	defaultLock   = toolsDir + "/tools.lock.yaml"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tools <install|lock|bump>")
	}
	switch args[0] {
	case "install":
		return installCommand(args[1:])
	case "lock":
		return lockCommand(args[1:], false)
	case "bump":
		return lockCommand(args[1:], true)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func installCommand(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "dependency config")
	lockPath := flags.String("lock", defaultLock, "dependency lock")
	binDir := flags.String("bin-dir", "bin", "installation directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tools install [flags] TOOL")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	lock, err := loadLock(*lockPath)
	if err != nil {
		return err
	}
	if err := lock.validate(cfg); err != nil {
		return fmt.Errorf("%s: %w; run make bump-tools or tools lock", *lockPath, err)
	}
	absoluteBinDir, err := filepath.Abs(*binDir)
	if err != nil {
		return err
	}
	installer := &installer{
		cfg:       cfg,
		lock:      lock,
		binDir:    absoluteBinDir,
		installed: make(map[string]bool),
	}
	return installer.install(flags.Arg(0))
}

func lockCommand(args []string, bump bool) error {
	name := "lock"
	if bump {
		name = "bump"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := flags.String("config", defaultConfig, "dependency config")
	lockPath := flags.String("lock", defaultLock, "dependency lock")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: tools %s [flags]", name)
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if bump {
		if err := bumpVersions(cfg); err != nil {
			return err
		}
	}
	lock, err := buildLock(cfg)
	if err != nil {
		return err
	}
	if bump {
		if err := writeYAML(*configPath, cfg); err != nil {
			return err
		}
	}
	if err := writeYAML(*lockPath, lock); err != nil {
		return err
	}
	return nil
}
