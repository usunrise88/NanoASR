//go:build !linux && !windows && !darwin

package ortenv

import (
	"fmt"
	"runtime"
)

func loadedRuntime() ([]string, error) {
	return nil, fmt.Errorf("onnxruntime engines are not supported on %s", runtime.GOOS)
}
