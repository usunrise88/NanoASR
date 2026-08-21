import { useEffect, useState } from 'react'

/**
 * Match the wait (SPEC §13.4).
 *
 * Anything that resolves inside ~300 ms shows nothing at all: a skeleton that
 * flashes for one frame reads as a glitch, not as feedback.
 *
 * The state is adjusted during render rather than in an effect so a new pending
 * cycle starts hidden again without an extra render pass.
 */
export function useDelayedPending(pending: boolean, delayMs = 300): boolean {
  const [tracked, setTracked] = useState({ pending, elapsed: false })

  if (tracked.pending !== pending) {
    setTracked({ pending, elapsed: false })
  }

  useEffect(() => {
    if (!pending) return
    const id = window.setTimeout(() => setTracked({ pending: true, elapsed: true }), delayMs)
    return () => window.clearTimeout(id)
  }, [pending, delayMs])

  return pending && tracked.elapsed
}
