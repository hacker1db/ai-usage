//go:build unix

package token

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestExecRunnerKillsWrapperChildren mimics the Homebrew az wrapper: a shell script that runs a
// long-lived child without exec, so the child inherits and holds stdout and stderr.
func TestExecRunnerKillsWrapperChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "az")
	body := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := ExecRunner(ctx, script, "account", "get-access-token")
		done <- err
	}()

	// Cancel only once the child exists, so a slow machine cannot kill the shell before it forks.
	var raw []byte
	for wait := time.Now().Add(10 * time.Second); ; {
		var err error
		if raw, err = os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("wrapper script never recorded its child pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error after cancellation")
		}
	case <-time.After(2 * time.Second):
		// Well under execWaitDelay: the group kill, not the WaitDelay backstop, must release Run.
		t.Fatalf("ExecRunner still running %v after cancel; child kept the pipes open", time.Since(start))
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child sleep (pid %d) survived the context deadline", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
