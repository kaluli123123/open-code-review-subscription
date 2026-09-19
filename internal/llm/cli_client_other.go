// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package llm

import (
	"errors"
	"os/exec"
)

func configureCLIProcess(_ *exec.Cmd) error {
	return errors.New("subscription CLI unsupported on this platform: process-tree cleanup unavailable")
}

func cleanupCLIProcess(_ *exec.Cmd) {}
