//go:build windows

package claudecode

import "os"

func interruptSignal() os.Signal { return os.Kill }
