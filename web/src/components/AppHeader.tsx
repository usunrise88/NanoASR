import { Link } from '@tanstack/react-router'
import { Bell, HalfMoon, SunLight, Translate } from 'iconoir-react'

import { Inline } from '@/components/layout'
import { cn } from '@/lib/cn'
import { currentLanguage, setLanguage, useT } from '@/lib/i18n'
import { clearAll, markAllRead, useNotifications, useUnreadCount } from '@/lib/notifications'
import { setTheme, useTheme } from '@/lib/theme'
import { useState } from 'react'

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

function IconButton({
  onClick,
  label,
  children,
  className,
}: {
  onClick: () => void
  label: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className={cn(
        'relative flex h-7 w-7 items-center justify-center rounded-[var(--radius-md)]',
        'text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] hover:text-[var(--text-primary)]',
        className,
      )}
    >
      {children}
    </button>
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
 */
function NotificationBell() {
  const t = useT()
  const items = useNotifications()
  const unread = useUnreadCount()
  const [open, setOpen] = useState(false)

  return (
    <div className="relative">
      <IconButton
        label={t('notifications.title')}
        onClick={() => {
          setOpen((v) => !v)
          if (!open) markAllRead()
        }}
      >
        <Bell width={15} height={15} />
        {unread > 0 && (
          <span className="absolute right-1 top-1 h-1.5 w-1.5 rounded-full bg-[var(--accent-solid)]" />
        )}
      </IconButton>

      {open && (
        <div
          data-motion="enter"
          className="absolute right-0 mt-1 w-80 overflow-hidden rounded-[var(--radius-lg)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] shadow-lg"
        >
          <div className="flex items-center justify-between border-b border-[var(--border-subtle)] px-3 py-2">
            <span className="text-[12px] font-medium">{t('notifications.title')}</span>
            <button
              type="button"
              onClick={clearAll}
              className="text-[12px] text-[var(--text-muted)] hover:text-[var(--text-primary)]"
            >
              {t('notifications.clear')}
            </button>
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
        </div>
      )}
    </div>
  )
}
