//go:build windows

package main

import (
	"os/exec"
	"time"
)

// configurePluginProcess relies on the default context cancellation (the
// process is killed) and sets WaitDelay so a child keeping an I/O pipe open
// cannot prevent Wait from returning.
func configurePluginProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}
