package sortformer

import (
	"math"
	"slices"
)

// cacheParams is the offline pass's speaker cache and FIFO policy, as the
// reference configures it (Nemotron3DiarizationSpeakerCache with the offline
// fifo_length and speaker_cache_update_period).
type cacheParams struct {
	cacheLen     int // speaker_cache_length
	silenceSlots int // speaker_cache_silence_frames_per_speaker
	fifoLen      int
	updatePeriod int

	scoreThreshold float64 // prediction_score_threshold
	latestBoost    float64 // latest_frames_score_boost

	minPositive   int // frames per speaker, derived from the rates below
	strongBoosted int
	weakBoosted   int
}

func newCacheParams(cacheLen, silenceSlots, fifoLen, updatePeriod int,
	scoreThreshold, latestBoost, minPositiveRate, strongRate, weakRate float64) cacheParams {
	// the share of the cache each speaker is budgeted, excluding its silence slots
	budget := float64(cacheLen/numSpeakers - silenceSlots)
	return cacheParams{
		cacheLen: cacheLen, silenceSlots: silenceSlots, fifoLen: fifoLen, updatePeriod: updatePeriod,
		scoreThreshold: scoreThreshold, latestBoost: latestBoost,
		minPositive:   int(math.Floor(budget * minPositiveRate)),
		strongBoosted: int(math.Floor(budget * strongRate)),
		weakBoosted:   int(math.Floor(budget * weakRate)),
	}
}

// speakerCache is the Arrival-Order Speaker Cache and the FIFO of the most
// recent frames that every step attends to: a line-by-line port of the
// reference's Nemotron3DiarizationSpeakerCache for one recording.
//
// It lives for one Process call and is not safe for concurrent use.
type speakerCache struct {
	p       cacheParams
	silence []float32 // [hiddenSize], the learned silence embedding

	embeds     []float32 // [cacheLen][hiddenSize]
	probs      []float32 // [cacheLen][numSpeakers], at the encoder frame rate
	n          int
	fifo       []float32 // [fifoLen][hiddenSize]
	nFIFO      int
	compressed bool

	// scratch, reused across updates
	pooled   []float32
	catEmb   []float32
	catProbs []float32
	scores   []float64
	order    []int
}

func newSpeakerCache(p cacheParams, silence []float32) *speakerCache {
	return &speakerCache{
		p:       p,
		silence: silence,
		embeds:  make([]float32, p.cacheLen*hiddenSize),
		probs:   make([]float32, p.cacheLen*numSpeakers),
		fifo:    make([]float32, p.fifoLen*hiddenSize),
	}
}

// context is how many frames precede the chunk in a step's input.
func (c *speakerCache) context() int { return c.n + c.nFIFO }

// prefix writes the cache and the FIFO — the start of the next step's input —
// into dst and returns how many frames it wrote.
func (c *speakerCache) prefix(dst []float32) int {
	copy(dst, c.embeds[:c.n*hiddenSize])
	copy(dst[c.n*hiddenSize:], c.fifo[:c.nFIFO*hiddenSize])
	return c.context()
}

// update pushes a processed chunk into the FIFO, moving its oldest frames into
// the cache when it overflows and compressing the cache when that overflows.
//
// input is the step's input, frames rows of it: the context this cache
// supplied, then the chunk's own n frames, then look-ahead. logits are the
// step's output, frames·subsampling rows at 10 ms.
func (c *speakerCache) update(input []float32, frames int, logits []float32, n int) {
	probs := c.poolProbs(logits, frames)

	nCache, nFIFO := c.n, c.nFIFO
	chunkStart := nCache + nFIFO
	// the FIFO followed by the chunk is exactly input[nCache : chunkStart+n]
	fifoEmb := input[nCache*hiddenSize : (chunkStart+n)*hiddenSize]
	fifoFrames := nFIFO + n

	popped := 0
	if fifoFrames > c.p.fifoLen {
		popped = min(max(c.p.updatePeriod, fifoFrames-c.p.fifoLen), fifoFrames)
	}
	if popped > 0 {
		fifoProbs := probs[nCache*numSpeakers : (nCache+fifoFrames)*numSpeakers]
		// An uncompressed cache still holds plain chunk frames, whose
		// probabilities this step re-estimated. A compressed one is out of
		// order, so the probabilities stored with its frames are the only ones.
		stored := probs[:nCache*numSpeakers]
		if c.compressed {
			stored = c.probs[:nCache*numSpeakers]
		}
		total := nCache + popped
		c.catEmb = append(append(c.catEmb[:0], c.embeds[:nCache*hiddenSize]...), fifoEmb[:popped*hiddenSize]...)
		c.catProbs = append(append(c.catProbs[:0], stored...), fifoProbs[:popped*numSpeakers]...)

		if total > c.p.cacheLen {
			c.compress(total)
			c.compressed = true
			total = c.p.cacheLen
		} else {
			copy(c.embeds, c.catEmb)
			copy(c.probs, c.catProbs)
		}
		c.n = total
	}

	rest := fifoEmb[popped*hiddenSize:]
	// rest aliases input, never c.fifo, so the copy cannot overlap itself
	copy(c.fifo, rest)
	c.nFIFO = fifoFrames - popped
}

// poolProbs is the reference's _pool_probs: the mean of sigmoid(logit) over
// each encoder frame's subsampling rows.
func (c *speakerCache) poolProbs(logits []float32, frames int) []float32 {
	c.pooled = slices.Grow(c.pooled[:0], frames*numSpeakers)[:frames*numSpeakers]
	for f := 0; f < frames; f++ {
		for s := 0; s < numSpeakers; s++ {
			var sum float64
			for r := 0; r < subsampling; r++ {
				sum += float64(sigmoid(logits[(f*subsampling+r)*numSpeakers+s]))
			}
			c.pooled[f*numSpeakers+s] = float32(sum / subsampling)
		}
	}
	return c.pooled
}

func sigmoid(x float32) float32 { return float32(1 / (1 + math.Exp(-float64(x)))) }

// compress keeps cacheLen of the total frames in catEmb/catProbs: the most
// important ones, grouped by speaker and in their original order within a
// speaker, with silenceSlots slots per speaker given to the silence embedding.
// It writes the result into c.embeds and c.probs.
func (c *speakerCache) compress(total int) {
	p := c.p
	scored := total + p.silenceSlots
	c.scores = slices.Grow(c.scores[:0], scored*numSpeakers)[:scored*numSpeakers]
	scores := c.scores // speaker-major: scores[s*scored + f]
	c.frameScores(total, scored)
	// the frames beyond the cache's capacity are the ones just popped from the FIFO
	for s := 0; s < numSpeakers; s++ {
		for f := p.cacheLen; f < total; f++ {
			scores[s*scored+f] += p.latestBoost
		}
	}
	for s := 0; s < numSpeakers; s++ {
		row := scores[s*scored : (s+1)*scored]
		c.boost(row[:total], p.strongBoosted, -2*math.Log(0.5))
		c.boost(row[:total], p.weakBoosted, -math.Log(0.5))
		// every speaker's silence slots are always kept
		for f := total; f < scored; f++ {
			row[f] = math.Inf(1)
		}
	}

	// the cacheLen best (speaker, frame) pairs, in speaker-major index order;
	// pairs with no score at all become silence, after all the others
	c.order = topK(c.order, scores, p.cacheLen)
	sentinel := scored * numSpeakers
	for i, idx := range c.order {
		if math.IsInf(scores[idx], -1) {
			c.order[i] = sentinel
		}
	}
	slices.Sort(c.order)

	for i, idx := range c.order {
		frame := total // the silence frame
		if idx != sentinel {
			frame = min(idx%scored, total)
		}
		dstE := c.embeds[i*hiddenSize : (i+1)*hiddenSize]
		dstP := c.probs[i*numSpeakers : (i+1)*numSpeakers]
		if frame == total {
			copy(dstE, c.silence)
			clear(dstP)
			continue
		}
		copy(dstE, c.catEmb[frame*hiddenSize:(frame+1)*hiddenSize])
		copy(dstP, c.catProbs[frame*numSpeakers:(frame+1)*numSpeakers])
	}
}

// frameScores is the reference's _get_frame_scores over catProbs, written
// speaker-major into c.scores with a stride of scored frames.
func (c *speakerCache) frameScores(total, scored int) {
	th := c.p.scoreThreshold
	log05 := math.Log(0.5)
	positive := make([]int, numSpeakers)
	for f := 0; f < total; f++ {
		pr := c.catProbs[f*numSpeakers : (f+1)*numSpeakers]
		var sumComp float64
		for _, v := range pr {
			sumComp += math.Log(math.Max(1-float64(v), th))
		}
		for s, v := range pr {
			pv := float64(v)
			sc := math.Log(math.Max(pv, th)) - math.Log(math.Max(1-pv, th)) + sumComp - log05
			if !(pv > 0.5) {
				sc = math.Inf(-1)
			} else if sc > 0 {
				positive[s]++
			}
			c.scores[s*scored+f] = sc
		}
	}
	// A speaker with enough confidently positive frames keeps only those.
	for s := 0; s < numSpeakers; s++ {
		if positive[s] < c.p.minPositive {
			continue
		}
		for f := 0; f < total; f++ {
			i := s*scored + f
			if !(c.scores[i] > 0) && float64(c.catProbs[f*numSpeakers+s]) > 0.5 {
				c.scores[i] = math.Inf(-1)
			}
		}
	}
}

// boost adds b to the k highest scores of row, the reference's _boost_scores.
// A score of -Inf stays -Inf.
func (c *speakerCache) boost(row []float64, k int, b float64) {
	c.order = topK(c.order, row, k)
	for _, i := range c.order {
		row[i] += b
	}
}

// topK writes into dst the indices of the k largest values, breaking ties
// towards the lower index. torch.topk leaves ties unspecified; any fixed rule
// is as faithful, and this one makes the result reproducible.
func topK(dst []int, values []float64, k int) []int {
	k = min(k, len(values))
	dst = dst[:0]
	for i := range values {
		dst = append(dst, i)
	}
	slices.SortStableFunc(dst, func(a, b int) int {
		va, vb := values[a], values[b]
		switch {
		case va > vb:
			return -1
		case va < vb:
			return 1
		}
		return 0
	})
	return dst[:k]
}
