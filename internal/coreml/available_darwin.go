//go:build darwin

package coreml

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	availOnce sync.Once
	availErr  error
)

// minMacOS is the release the models are converted for (coremltools'
// macOS15 target), and the first that loads models of several functions,
// which T-one's and CAM++'s are.
const minMacOS = 15

func available() error {
	availOnce.Do(func() {
		v, err := syscall.Sysctl("kern.osproductversion")
		if err != nil {
			availErr = fmt.Errorf("coreml: cannot read the macOS version: %w", err)
			return
		}
		major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
		if major < minMacOS {
			availErr = fmt.Errorf("coreml: the converted models need macOS %d or later, this is %s", minMacOS, v)
		}
	})
	return availErr
}
