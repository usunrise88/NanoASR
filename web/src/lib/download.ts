/**
 * Hands a Blob to the browser as a download. The URL is revoked after a
 * moment rather than at once: a browser that starts the download
 * asynchronously would otherwise find nothing behind it.
 */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}
