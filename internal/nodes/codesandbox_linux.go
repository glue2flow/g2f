package nodes

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// sandboxAttr makes the kernel kill the sandbox when the server dies, so a
// crashed server leaves no scripts running.
func sandboxAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// processRSS returns the resident memory of a process in bytes (0 if unknown).
func processRSS(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0
	}
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(string(f[1]), 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
