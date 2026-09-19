// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package llm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCLIProcessTreeHelper(t *testing.T) {
	mode := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--ocr-tree=") {
			mode = strings.TrimPrefix(arg, "--ocr-tree=")
		}
	}
	if mode == "" {
		return
	}
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		os.Exit(17)
	}
	fmt.Println(child.Process.Pid)
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--ocr-pid-file=") {
			_ = os.WriteFile(strings.TrimPrefix(arg, "--ocr-pid-file="), []byte(strconv.Itoa(child.Process.Pid)), 0600)
		}
	}
	if mode == "timeout" {
		time.Sleep(60 * time.Second)
	}
	os.Exit(0)
}

func TestCLIProcessTreeCleanup(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The helper exits successfully with a live descendant that does not retain
	// stdout. Cleanup must not depend on Command.Wait detecting open pipes.
	out, err := runCLI(context.Background(), binary, []string{"-test.run=^TestCLIProcessTreeHelper$", "--", "--ocr-tree=success"}, os.Environ(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("CLI descendant survived successful parent exit")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = runCLI(ctx, binary, []string{"-test.run=^TestCLIProcessTreeHelper$", "--", "--ocr-tree=timeout", "--ocr-pid-file=" + pidFile}, os.Environ(), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("timeout not propagated: %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err = strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline = time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("CLI descendant survived timeout")
	}
}
