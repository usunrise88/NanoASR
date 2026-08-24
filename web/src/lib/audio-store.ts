/**
 * The audio a job was run on, for as long as this tab lives.
 *
 * Uploaded audio never reaches the server's disk beyond the job itself, so the
 * only copy the player can use is the File the user picked. Keeping it in a
 * module rather than in router state or storage matches what is actually true:
 * it survives navigation within the app and nothing else. A reload loses it,
 * and the result page says so and offers to take the file again.
 *
 * Bounded, because a session that transcribes twenty files should not pin
 * twenty files in memory — the oldest are dropped, and dropping one degrades
 * the page to exactly the state a reload produces.
 */
const LIMIT = 3

const files = new Map<string, File>()

export function remember(jobID: string, file: File): void {
  files.delete(jobID)
  files.set(jobID, file)
  while (files.size > LIMIT) {
    const oldest = files.keys().next().value
    if (oldest === undefined) break
    files.delete(oldest)
  }
}

export function recall(jobID: string): File | undefined {
  return files.get(jobID)
}

export function forget(jobID: string): void {
  files.delete(jobID)
}
