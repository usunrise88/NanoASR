package sortformer

import (
	"golang.org/x/sys/windows"
)

// loadedRuntime is the file behind the onnxruntime.dll module of the process.
// Windows keeps one module per base name, so a second copy cannot be loaded
// under the same name; what matters is which file that one module came from.
func loadedRuntime() ([]string, error) {
	name, err := windows.UTF16PtrFromString("onnxruntime.dll")
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, name, &h); err != nil {
		return nil, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetModuleFileName(h, &buf[0], uint32(len(buf)))
	if err != nil {
		return nil, err
	}
	return []string{windows.UTF16ToString(buf[:n])}, nil
}
