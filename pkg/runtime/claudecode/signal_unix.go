//go:build !windows

package claudecode

import (
	"os"
	"syscall"
)

func interruptSignal() os.Signal { return syscall.SIGINT }
