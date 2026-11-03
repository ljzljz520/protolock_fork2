//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// configurePluginProcess puts a plugin in its own process group so that, on
// timeout, the plugin and any processes it spawned are all signaled and no
// child is left running (holding the pipe open or consuming resources) after
// protolock gives up. WaitDelay reaps the plugin even if a child keeps an
// I/O pipe open after cancellation.
func configurePluginProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// negative pid signals the entire plugin process group
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}
