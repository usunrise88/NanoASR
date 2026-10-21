//go:build unix && !linux

package managed

import "syscall"

// niceness is how far below normal the instance runs: enough that the
// application beside it — a call in progress — keeps the CPU when both want
// it, not so far that a transcript stalls on an idle machine.
const niceness = 10

// LowerPriority lowers the priority of the whole process, which is what a
// nice value applies to outside Linux.
func LowerPriority() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, niceness)
}
