//go:build !linux && !windows

package ortenv

import (
	"fmt"
	"runtime"
)

func loadedRuntime() ([]string, error) {
	return nil, fmt.Errorf("onnxruntime engines are not supported on %s", runtime.GOOS)
}
