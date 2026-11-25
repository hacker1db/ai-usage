//go:build !unix

package token

import "os/exec"

func killProcessGroupOnCancel(*exec.Cmd) {}
