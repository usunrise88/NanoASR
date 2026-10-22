// Package coreml runs compiled CoreML models, which is how NanoASR reaches the
// Neural Engine and the GPU of an Apple Silicon Mac.
//
// It exists beside onnxruntime rather than through onnxruntime's own CoreML
// provider because that provider was measured and failed on every model this
// server ships: it builds the legacy model format, cannot place int8 graphs or
// dynamic shapes on the Neural Engine, and was 10 to 30 times slower than the
// CPU it was meant to relieve. The models here are converted for CoreML from
// their original weights instead — fp16, fixed shapes, no op the Neural Engine
// lacks — and this package only loads and runs them.
//
// Every value crosses the boundary as float32. The bridge converts to the
// model's own element type and back, so callers never see fp16, and a model's
// precision can change without its caller changing.
package coreml

import "fmt"

// Units says which processors a model may run on. CoreML treats it as a
// permission rather than an order: a layer the chosen accelerator cannot run
// falls back to the CPU, which is why the converted models contain no such
// layer and why a model's placement is measured when it is converted.
type Units int

// The values are MLComputeUnits'.
const (
	CPUOnly      Units = 0
	CPUAndGPU    Units = 1
	All          Units = 2
	NeuralEngine Units = 3 // the Neural Engine, with the CPU for what it cannot run
)

// Feature is one input or output of a model.
type Feature struct {
	Name string `json:"name"`
	// Type is the element type the model stores: float16, float32, int32.
	Type  string `json:"type"`
	Shape []int  `json:"shape"`
}

// Size is the number of elements the feature holds.
func (f Feature) Size() int {
	n := 1
	for _, d := range f.Shape {
		n *= d
	}
	return n
}

// Description is what a model takes and returns.
type Description struct {
	Inputs  []Feature `json:"inputs"`
	Outputs []Feature `json:"outputs"`
}

// Input finds an input by name.
func (d Description) Input(name string) (Feature, error) {
	return find(d.Inputs, "input", name)
}

// Output finds an output by name.
func (d Description) Output(name string) (Feature, error) {
	return find(d.Outputs, "output", name)
}

func find(fs []Feature, what, name string) (Feature, error) {
	for _, f := range fs {
		if f.Name == name {
			return f, nil
		}
	}
	return Feature{}, fmt.Errorf("the model has no %s %q", what, name)
}
