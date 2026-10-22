//go:build darwin

package coreml

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreML -framework Foundation
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// predictMu serialises every prediction in the process, across models.
//
// Not for the models' sake but for the framework's: CoreML's Neural Engine
// runtime shares state between models, and two predictions running at once
// on different models have been seen to corrupt it — a SIGBUS in a process that
// diarizes while it transcribes. There is one Neural Engine, so running one
// prediction at a time costs nothing it could have done in parallel.
//
// Loading is left out of it on purpose: the first load of a model compiles
// it for the Neural Engine, for tens of seconds, and a realtime session must
// not wait behind a model the pool is loading in the background.
var predictMu sync.Mutex

// Model is one loaded CoreML model, or one function of a model with several.
type Model struct {
	h    *C.cm_model
	desc Description
	// names are the C copies of the feature names, made once.
	names map[string]*C.char
}

// Available reports whether this host can run the converted models: they are
// built for macOS 15, the first with models of several functions.
func Available() error {
	return available()
}

// Load loads a compiled model directory (.mlmodelc). function picks one
// function of a multifunction model; empty is its default.
//
// The first load of a model on a machine is slow — tens of seconds for a large
// encoder — because CoreML compiles it for the Neural Engine then. The result
// is cached by the system, keyed by the model, so every later load, in this
// process or the next, takes a fraction of a second.
func Load(path, function string, units Units) (*Model, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	cpath, cfn := C.CString(path), C.CString(function)
	defer C.free(unsafe.Pointer(cpath))
	defer C.free(unsafe.Pointer(cfn))
	var cerr *C.char
	h := C.cm_load(cpath, cfn, C.int(units), &cerr)
	if h == nil {
		return nil, fmt.Errorf("coreml: cannot load %s%s: %s", path, fnSuffix(function), takeError(cerr))
	}
	m := &Model{h: h, names: map[string]*C.char{}}
	js := C.cm_describe(h, &cerr)
	if js == nil {
		m.Close()
		return nil, fmt.Errorf("coreml: cannot describe %s: %s", path, takeError(cerr))
	}
	err := json.Unmarshal([]byte(C.GoString(js)), &m.desc)
	C.free(unsafe.Pointer(js))
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("coreml: description of %s: %w", path, err)
	}
	for _, fs := range [][]Feature{m.desc.Inputs, m.desc.Outputs} {
		for _, f := range fs {
			m.names[f.Name] = C.CString(f.Name)
		}
	}
	runtime.SetFinalizer(m, (*Model).Close)
	return m, nil
}

func fnSuffix(function string) string {
	if function == "" {
		return ""
	}
	return " (function " + function + ")"
}

// Description says what the model takes and returns.
func (m *Model) Description() Description { return m.desc }

// Predict runs the model once. in holds a value for every input and out a
// buffer for every output wanted, each exactly the size of its feature; the
// buffers are written in place.
func (m *Model) Predict(in, out map[string][]float32) error {
	if m.h == nil {
		return fmt.Errorf("coreml: the model is closed")
	}
	var pin runtime.Pinner
	defer pin.Unpin()

	cIn := newArgs(len(in))
	defer cIn.free()
	for name, data := range in {
		if err := cIn.add(m, &pin, name, data); err != nil {
			return err
		}
	}
	cOut := newArgs(len(out))
	defer cOut.free()
	for name, data := range out {
		if err := cOut.add(m, &pin, name, data); err != nil {
			return err
		}
	}

	var cerr *C.char
	predictMu.Lock()
	rc := C.cm_predict(m.h,
		C.int(cIn.n), cIn.names, cIn.data, cIn.counts,
		C.int(cOut.n), cOut.names, cOut.data, cOut.counts, &cerr)
	predictMu.Unlock()
	if rc != 0 {
		return fmt.Errorf("coreml: prediction failed: %s", takeError(cerr))
	}
	return nil
}

// Close frees the model. It is safe to call more than once.
func (m *Model) Close() error {
	if m.h != nil {
		C.cm_free(m.h)
		m.h = nil
	}
	for k, s := range m.names {
		C.free(unsafe.Pointer(s))
		delete(m.names, k)
	}
	runtime.SetFinalizer(m, nil)
	return nil
}

// args are the parallel C arrays one side of a prediction is passed as. They
// live in C memory, so the Go buffers they point at are pinned for the call.
type args struct {
	n      int
	names  **C.char
	data   **C.float
	counts *C.int64_t
}

func newArgs(n int) *args {
	size := C.size_t(max(n, 1))
	return &args{
		names:  (**C.char)(C.malloc(size * C.size_t(unsafe.Sizeof((*C.char)(nil))))),
		data:   (**C.float)(C.malloc(size * C.size_t(unsafe.Sizeof((*C.float)(nil))))),
		counts: (*C.int64_t)(C.malloc(size * C.size_t(unsafe.Sizeof(C.int64_t(0))))),
	}
}

func (a *args) add(m *Model, pin *runtime.Pinner, name string, data []float32) error {
	cname, ok := m.names[name]
	if !ok {
		return fmt.Errorf("coreml: the model has no feature %q", name)
	}
	if len(data) == 0 {
		return fmt.Errorf("coreml: no buffer for %q", name)
	}
	pin.Pin(&data[0])
	unsafe.Slice(a.names, a.n+1)[a.n] = cname
	unsafe.Slice(a.data, a.n+1)[a.n] = (*C.float)(unsafe.Pointer(&data[0]))
	unsafe.Slice(a.counts, a.n+1)[a.n] = C.int64_t(len(data))
	a.n++
	return nil
}

func (a *args) free() {
	C.free(unsafe.Pointer(a.names))
	C.free(unsafe.Pointer(a.data))
	C.free(unsafe.Pointer(a.counts))
}

func takeError(s *C.char) string {
	if s == nil {
		return "unknown error"
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s)
}
