//go:build linux

package managed

import (
	"os"
	"strconv"
	"syscall"
)

// niceness is how far below normal the instance runs: enough that the
// application beside it — a call in progress — keeps the CPU when both want
// it, not so far that a transcript stalls on an idle machine.
const niceness = 10

// LowerPriority lowers the priority of every thread of the process.
//
// On Linux a nice value belongs to a thread, not to the process, and a new
// thread takes its creator's: so every thread there is now is lowered, and
// every one the runtime or onnxruntime makes later inherits it.
func LowerPriority() error {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return syscall.Setpriority(syscall.PRIO_PROCESS, 0, niceness)
	}
	var first error
	for _, t := range tasks {
		tid, err := strconv.Atoi(t.Name())
		if err != nil {
			continue
		}
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, niceness); err != nil && first == nil {
			first = err
		}
	}
	return first
}
