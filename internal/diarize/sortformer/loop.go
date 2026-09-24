package sortformer

import "context"

// loopParams is how the offline pass cuts a recording into steps.
type loopParams struct {
	chunkLen  int // chunk_length: encoder frames whose logits one step produces
	rightCtx  int // chunk_right_context: look-ahead frames each step also sees
	cache     cacheParams
	silence   []float32 // [hiddenSize]
	maxFrames int       // the longest step input, cache + FIFO + chunk + look-ahead

	afterUpdate func(*speakerCache) // tests only: the state after each step
}

func (p loopParams) withMax() loopParams {
	p.maxFrames = p.cache.cacheLen + p.cache.fifoLen + p.chunkLen + p.rightCtx
	return p
}

// embedSource writes the embeddings of encoder frames [from, to) into dst.
type embedSource func(from, to int, dst []float32) error

// logitSink receives each step's logits for its own chunk: the 10 ms rows of
// encoder frames [first, first + len(rows)/(subsampling·numSpeakers)).
type logitSink func(first int, rows []float32)

// runOffline is the reference's offline forward (the loop in
// Nemotron3DiarizationForAudioFrameClassification.forward) over a recording of
// frames encoder frames.
//
// Every step's input is the speaker cache, the FIFO, the chunk and its
// look-ahead, packed densely: the graph takes no mask, so a step is never fed
// padding. The reference does have one padding frame to mask, the trailing
// encoder frame when the valid mel frames are a multiple of eight, and frames
// excludes it. That changes only what the head's kernel-3 convolution sees next
// to the recording's last 80 ms.
//
// Embeddings are computed a chunk at a time, not for the whole recording,
// which at four hours would be ~370 MB.
func runOffline(ctx context.Context, r runner, p loopParams, frames int, src embedSource, sink logitSink) error {
	cache := newSpeakerCache(p.cache, p.silence)
	input := make([]float32, p.maxFrames*hiddenSize)
	logits := make([]float32, p.maxFrames*subsampling*numSpeakers)
	window := make([]float32, (p.chunkLen+p.rightCtx)*hiddenSize) // frames [winFrom, winTo)
	winFrom, winTo := 0, 0
	const rowLen = subsampling * numSpeakers

	for start := 0; start < frames; start += p.chunkLen {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+p.chunkLen, frames)
		n := end - start
		right := min(end+p.rightCtx, frames)

		// the previous step's look-ahead opens this chunk: keep it
		keep := max(0, winTo-start)
		copy(window, window[(start-winFrom)*hiddenSize:(winTo-winFrom)*hiddenSize])
		if err := src(start+keep, right, window[keep*hiddenSize:(right-start)*hiddenSize]); err != nil {
			return err
		}
		winFrom, winTo = start, right

		prefix := cache.prefix(input)
		copy(input[prefix*hiddenSize:], window[:(right-start)*hiddenSize])
		total := prefix + right - start

		in, out := input[:total*hiddenSize], logits[:total*rowLen]
		if err := r.step(ctx, in, total, out); err != nil {
			return err
		}
		cache.update(in, total, out, n)
		if p.afterUpdate != nil {
			p.afterUpdate(cache)
		}
		sink(start, out[prefix*rowLen:(prefix+n)*rowLen])
	}
	return nil
}
