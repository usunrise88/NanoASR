import { describe, expect, it } from 'vitest'

import { chosenLanguage, localeLabel } from './locale'

describe('localeLabel', () => {
  it('names the language and keeps the code', () => {
    expect(localeLabel('pt-BR', 'en')).toBe('Portuguese — pt-BR')
    expect(localeLabel('ru-RU', 'ru')).toBe('Русский — ru-RU')
  })

  it('does not name a region the model never claimed', () => {
    // ar-AR is the model's spelling; "Argentina" would be invented.
    expect(localeLabel('ar-AR', 'en')).toBe('Arabic — ar-AR')
    expect(localeLabel('ar-AR', 'en')).not.toMatch(/Argentina/)
  })

  it('tells variants of one language apart by their code', () => {
    expect(localeLabel('en-US', 'en')).not.toBe(localeLabel('en-GB', 'en'))
  })

  it('falls back to the bare code for what it cannot name', () => {
    expect(localeLabel('zzz-ZZ', 'en')).toBe('zzz-ZZ')
    expect(localeLabel('', 'en')).toBe('')
  })
})

describe('chosenLanguage', () => {
  const locales = ['en-US', 'pt-BR', 'ru-RU']

  it('is auto when nothing was chosen', () => {
    expect(chosenLanguage(undefined, locales)).toBe('auto')
    expect(chosenLanguage('', locales)).toBe('auto')
    expect(chosenLanguage('  ', locales)).toBe('auto')
  })

  it('finds the listed spelling of what was typed', () => {
    expect(chosenLanguage('ru-ru', locales)).toBe('ru-RU')
    expect(chosenLanguage('pt_br', locales)).toBe('pt-BR')
  })

  it('keeps a value the model does not list, because that is what will be sent', () => {
    expect(chosenLanguage('de', locales)).toBe('de')
    expect(chosenLanguage('auto', locales)).toBe('auto')
  })
})
