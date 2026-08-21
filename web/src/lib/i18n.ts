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
  try {
    localStorage.setItem('nanoasr.lang', lang)
  } catch {
    // Persisting the preference is a nicety, not a requirement.
  }
}

export function currentLanguage(): Language {
  return (i18n.language.startsWith('ru') ? 'ru' : 'en') as Language
}

/** Typed translator. Every component gets its strings through this. */
export function useT(): (key: TKey) => string {
  const { t } = useTranslation()
  return (key: TKey) => t(key)
}
