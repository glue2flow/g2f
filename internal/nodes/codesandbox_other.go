//go:build !linux

package nodes

import "os/exec"

func sandboxAttr(*exec.Cmd) {}

// processRSS is not available on this platform; the in-process watchdog of
// the sandbox still enforces the limit.
func processRSS(int) int64 { return 0 }
