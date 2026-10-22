//go:build !darwin

package coreml

import "errors"

var errUnavailable = errors.New("coreml: CoreML exists on macOS only")

// Model is one loaded CoreML model. Outside macOS none can be loaded.
type Model struct{}

// Available reports that this host cannot run CoreML models.
func Available() error { return errUnavailable }

// Load fails: CoreML exists on macOS only.
func Load(path, function string, units Units) (*Model, error) { return nil, errUnavailable }

func (m *Model) Description() Description { return Description{} }

func (m *Model) Predict(in, out map[string][]float32) error { return errUnavailable }

func (m *Model) Close() error { return nil }
