//go:build !linux && !windows

package sortformer

import (
	"fmt"
	"runtime"
)

func loadedRuntime() ([]string, error) {
	return nil, fmt.Errorf("the sortformer diarizer is not supported on %s", runtime.GOOS)
}
