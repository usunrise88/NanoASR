import { Link } from '@tanstack/react-router'
import { Bell, HalfMoon, Settings, SunLight, Translate } from 'iconoir-react'

import { Inline } from '@/components/layout'
import { Button, IconButton, Popover } from '@/components/ui'
import { currentLanguage, setLanguage, useT } from '@/lib/i18n'
import { clearAll, markAllRead, useNotifications, useUnreadCount } from '@/lib/notifications'
import { setTheme, useTheme } from '@/lib/theme'

/**
 * The application header. It is rendered once, by the root shell, and no route
 * may draw its own — that is the whole point of the persistent layout.
 */
export function AppHeader() {
  const t = useT()
  return (
    <header className="sticky top-0 z-20 border-b border-[var(--border-subtle)] bg-[var(--bg-canvas)]/85 backdrop-blur">
      <div className="mx-auto flex h-12 w-full max-w-5xl items-center gap-6 px-6">
        <Link to="/" className="text-[13px] font-semibold tracking-tight">
          {t('app.name')}
        </Link>
        <nav className="flex-1">
          <Inline gap={4}>
            <NavLink to="/" label={t('nav.new')} />
            <NavLink to="/models" label={t('nav.models')} />
            <NavLink to="/jobs" label={t('nav.jobs')} />
          </Inline>
        </nav>
        <Inline gap={1}>
          <LanguageToggle />
          <ThemeToggle />
          <NotificationBell />
          <SettingsLink />
        </Inline>
      </div>
    </header>
  )
}

function NavLink({ to, label }: { to: string; label: string }) {
  return (
    <Link
      to={to}
      className="text-[13px] text-[var(--text-secondary)] hover:text-[var(--text-primary)]"
      activeProps={{ className: 'text-[13px] text-[var(--text-primary)] font-medium' }}
      activeOptions={{ exact: to === '/' }}
    >
      {label}
    </Link>
  )
}

function SettingsLink() {
  const t = useT()
  return (
    <Link to="/settings" aria-label={t('settings.title')} title={t('settings.title')}
      className="flex h-7 w-7 items-center justify-center rounded-[var(--radius-md)] text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] hover:text-[var(--text-primary)]">
      <Settings width={15} height={15} />
    </Link>
  )
}

function LanguageToggle() {
  const lang = currentLanguage()
  return (
    <IconButton
      label={lang === 'ru' ? 'English' : 'Русский'}
      onClick={() => setLanguage(lang === 'ru' ? 'en' : 'ru')}
    >
      <Translate width={15} height={15} />
    </IconButton>
  )
}

function ThemeToggle() {
  const theme = useTheme()
  const t = useT()
  const next = theme === 'dark' ? 'light' : 'dark'
  return (
    <IconButton label={next === 'dark' ? t('theme.dark') : t('theme.light')} onClick={() => setTheme(next)}>
      {theme === 'dark' ? <SunLight width={15} height={15} /> : <HalfMoon width={15} height={15} />}
    </IconButton>
  )
}

/**
 * Notification history. Toasts vanish; this is where they are still readable
 * ten minutes later (SPEC §13.5).
 *
 * On the shared Popover rather than a hand-rolled panel: this was the last
 * surface in the product drawing its own box, which meant its focus handling,
 * dismissal and motion were nobody's decision in particular.
 */
function NotificationBell() {
  const t = useT()
  const items = useNotifications()
  const unread = useUnreadCount()

  return (
    <Popover
      trigger={
        <IconButton label={t('notifications.title')} onClick={markAllRead}>
          <Bell width={15} height={15} />
          {unread > 0 && (
            <span className="absolute top-1 right-1 h-1.5 w-1.5 rounded-full bg-[var(--accent-solid)]" />
          )}
        </IconButton>
      }
    >
      <div className="flex items-center justify-between border-b border-[var(--border-subtle)] px-3 py-2">
        <span className="text-[12px] font-medium">{t('notifications.title')}</span>
        <Button variant="ghost" size="sm" onClick={clearAll}>
          {t('notifications.clear')}
        </Button>
      </div>
      <ul className="max-h-80 overflow-y-auto">
        {items.length === 0 && (
          <li className="px-3 py-6 text-center text-[12px] text-[var(--text-muted)]">
            {t('notifications.empty')}
          </li>
        )}
        {items.map((n) => (
          <li key={n.id} className="border-b border-[var(--border-subtle)] px-3 py-2 last:border-0">
            <div className="flex items-baseline justify-between gap-2">
              <span className="text-[13px]">{n.title}</span>
              <time className="shrink-0 text-[11px] text-[var(--text-muted)]">
                {new Date(n.at).toLocaleTimeString()}
              </time>
            </div>
            {n.description && (
              <p className="mt-0.5 text-[12px] text-[var(--text-secondary)]">{n.description}</p>
            )}
          </li>
        ))}
      </ul>
    </Popover>
  )
}
