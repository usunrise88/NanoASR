/**
 * A locale as a person reads it: "Portuguese — pt-BR".
 *
 * The name comes from the browser's own table, in the interface language, so
 * nothing here has to be translated. Only the language part is named, and the
 * code is always shown: the model's codes are its own ("ar-AR" is not a region
 * anybody means), and a region name derived from one would be a claim the model
 * never made. The code is also what tells "en-US" from "en-GB".
 */
export function localeLabel(code: string, interfaceLanguage: string): string {
  const base = code.split(/[-_]/)[0] ?? code
  try {
    const name = new Intl.DisplayNames([interfaceLanguage], { type: 'language' }).of(base)
    if (name && name.toLowerCase() !== base.toLowerCase()) {
      return `${name.charAt(0).toUpperCase()}${name.slice(1)} — ${code}`
    }
  } catch {
    // DisplayNames throws on a code it cannot parse; the code alone is fine.
  }
  return code
}

/**
 * The language a run will actually be sent with, for a model that lists locales.
 *
 * Unset means "let the model choose". A value the model does not list is kept
 * rather than quietly replaced: it is what the request will carry, and a
 * control showing something else would be lying about it.
 */
export function chosenLanguage(value: string | undefined, locales: readonly string[]): string {
  const v = (value ?? '').trim()
  if (v === '') return 'auto'
  const folded = v.toLowerCase().replace('_', '-')
  return locales.find((l) => l.toLowerCase() === folded) ?? v
}
