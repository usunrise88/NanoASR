package ortenv

/*
#include <mach-o/dyld.h>
*/
import "C"

import (
	"slices"
	"strings"
)

// loadedRuntime lists every onnxruntime image loaded into the process, from
// dyld's own list: macOS has no /proc to read the mappings from.
func loadedRuntime() ([]string, error) {
	var paths []string
	for i := C.uint32_t(0); i < C._dyld_image_count(); i++ {
		name := C._dyld_get_image_name(i)
		if name == nil {
			continue
		}
		if p := C.GoString(name); strings.Contains(p, "libonnxruntime") && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
