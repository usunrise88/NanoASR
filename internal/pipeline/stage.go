package pipeline

import "time"

// runStage times one pipeline stage.
//
// It is a free function rather than a method because Go methods cannot take
// type parameters, and the alternative — every stage returning any — would put
// a type assertion between each step of the pipeline.
func runStage[T any](t *stageTimer, stage string, fn func() (T, error)) (T, error) {
	t.observer.StageStarted(t.ctx, t.jobID, stage)
	start := time.Now()
	v, err := fn()
	t.record(stage, time.Since(start), err)
	return v, err
}
