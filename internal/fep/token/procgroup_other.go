//go:build !unix

package token

import "os/exec"

// killProcessGroupOnCancel is a no-op where process groups are unavailable; exec.Cmd's default
// cancellation plus WaitDelay still bounds how long ExecRunner can block.
func killProcessGroupOnCancel(*exec.Cmd) {}
