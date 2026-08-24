import i18n from 'i18next'
import { initReactI18next, useTranslation } from 'react-i18next'

import en from '@/locales/en.json'
import ru from '@/locales/ru.json'

/**
 * Translation keys are derived from the resource files, so `t('nav.new')` is
 * checked at compile time and a typo is a build failure rather than a string
 * shown to a user.
 */
type Leaves<T, Prefix extends string = ''> = {
  [K in keyof T & string]: T[K] extends string
    ? `${Prefix}${K}`
    : Leaves<T[K], `${Prefix}${K}.`>
}[keyof T & string]

export type TKey = Leaves<typeof ru>

/**
 * The two catalogues must have the same keys.
 *
 * TKey is derived from ru alone — one of them has to be the reference — so
 * without this a key missing from en.json is caught by nobody: i18next falls
 * back at runtime and the reader gets Russian in an English interface. This
 * makes the omission a type error at the point where it happens.
 */
type SameShape<A, B> = {
  [K in keyof A]: K extends keyof B
    ? A[K] extends string
      ? string
      : SameShape<A[K], B[K]>
    : never
}
const _enCoversRu = en satisfies SameShape<typeof ru, typeof en>
const _ruCoversEn = ru satisfies SameShape<typeof en, typeof ru>
void _enCoversRu
void _ruCoversEn

export const languages = ['ru', 'en'] as const
export type Language = (typeof languages)[number]

void i18n.use(initReactI18next).init({
  resources: { ru: { translation: ru }, en: { translation: en } },
  lng: detectLanguage(),
  fallbackLng: 'en',
  interpolation: { escapeValue: false },
})

function detectLanguage(): Language {
  try {
    const stored = localStorage.getItem('nanoasr.lang')
    if (stored === 'ru' || stored === 'en') return stored
  } catch {
    // Storage can throw in private mode; the default is fine.
  }
  return navigator.language.startsWith('ru') ? 'ru' : 'en'
}

export function setLanguage(lang: Language): void {
  void i18n.changeLanguage(lang)
  applyDocumentLanguage(lang)
  try {
    localStorage.setItem('nanoasr.lang', lang)
  } catch {
    // Persisting the preference is a nicety, not a requirement.
  }
}

/**
 * Keeps <html lang> in step with the chosen language.
 *
 * index.html cannot state it: the language is a runtime choice, and a fixed
 * attribute tells a screen reader to pronounce English with Russian phonetics —
 * or the reverse — for as long as the user leaves the switch alone.
 */
function applyDocumentLanguage(lang: Language): void {
  document.documentElement.lang = lang
}

applyDocumentLanguage(detectLanguage())

export function currentLanguage(): Language {
  return (i18n.language.startsWith('ru') ? 'ru' : 'en') as Language
}

/** Values interpolated into a translation, as `{{name}}` in the resource. */
export type TVars = Record<string, string | number>

/** Typed translator. Every component gets its strings through this. */
export function useT(): (key: TKey, vars?: TVars) => string {
  const { t } = useTranslation()
  return (key: TKey, vars?: TVars) => (vars ? t(key, vars) : t(key))
}
