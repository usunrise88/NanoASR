import js from '@eslint/js'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'

import local from './eslint-rules/require-page-meta.js'

/**
 * Consistency here is a wall, not a guideline (SPEC §13.2).
 *
 * Each rule below closes one way the UI could drift: ad-hoc layout in pages,
 * per-component spacing, private toast stacks, animation sprinkled across
 * components, or a route that grows its own header. Every message says what to
 * use instead — a wall you cannot climb is only useful if it has a gate.
 */
export default tseslint.config(
  { ignores: ['dist', 'node_modules', 'src/routeTree.gen.ts'] },

  js.configs.recommended,
  ...tseslint.configs.recommended,

  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      'react-hooks': reactHooks,
      local,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,

      'local/require-page-meta': 'error',

      // Surfaces, toasts, i18n and Base UI are funnelled through single
      // primitives so behaviour and motion stay identical everywhere.
      'no-restricted-imports': [
        'error',
        {
          paths: [
            {
              name: 'sonner',
              message:
                'Import toast() from @/lib/toast instead. It records every notification in the ' +
                'history dropdown; a direct sonner call would be invisible there.',
            },
            {
              name: 'react-i18next',
              message: 'Use useT() from @/lib/i18n so translation keys stay type-checked.',
            },
          ],
          patterns: [
            {
              group: ['@base-ui/react/*', '@base-ui-components/react/*'],
              message:
                'Wrap Base UI once in @/components/ui and import that. Direct usage forks ' +
                'motion, focus handling and backdrop styling.',
            },
          ],
        },
      ],

      'no-restricted-syntax': [
        'error',
        {
          selector:
            'JSXAttribute[name.name="style"]',
          message:
            'Inline style is not a design token. Use layout primitives and the token classes; ' +
            'canvas geometry in components/player is the only exception (see overrides).',
        },
        {
          selector:
            'JSXAttribute[name.name="className"][value.value=/\\b(animate-|transition|duration-)/]',
          message:
            'Animations are off by default. Declare motion in src/styles/motion.css and opt in ' +
            'with data-motion so prefers-reduced-motion keeps working.',
        },
      ],

      '@typescript-eslint/consistent-type-imports': 'error',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
      eqeqeq: ['error', 'always'],
    },
  },

  // Routes may not lay themselves out. Composition happens through Page /
  // Section / Stack / Grid / Inline, which own every spacing decision.
  {
    files: ['src/routes/**/*.tsx'],
    rules: {
      'no-restricted-syntax': [
        'error',
        {
          selector:
            'JSXOpeningElement[name.name=/^(div|section|main|header|footer|aside)$/] > ' +
            'JSXAttribute[name.name="className"]',
          message:
            'Do not hand-roll layout in a route. Use <Page>, <Section>, <Stack>, <Grid> or ' +
            '<Inline> from @/components/layout — they are the only source of spacing.',
        },
        {
          selector: 'JSXAttribute[name.name="style"]',
          message: 'Inline style is not a design token. Use the layout primitives.',
        },
      ],
    },
  },

  // The root shell is the one file that may lay out the frame and mount the
  // toast viewport — that is precisely what makes every route unable to.
  {
    files: ['src/routes/__root.tsx'],
    rules: {
      'no-restricted-syntax': 'off',
      'no-restricted-imports': 'off',
    },
  },

  // The waveform renderer computes pixel geometry at runtime; that is real
  // layout maths, not styling drift.
  {
    files: ['src/components/player/**/*.tsx'],
    rules: { 'no-restricted-syntax': 'off' },
  },

  // The primitives themselves are allowed to touch what they encapsulate.
  {
    files: ['src/components/ui/**/*.tsx', 'src/lib/toast.ts', 'src/lib/i18n.ts'],
    rules: { 'no-restricted-imports': 'off' },
  },

  {
    files: ['vite.config.ts', 'eslint.config.js', 'eslint-rules/**/*.js'],
    languageOptions: { globals: globals.node },
  },
)
