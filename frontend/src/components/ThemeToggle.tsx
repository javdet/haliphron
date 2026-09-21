import { useEffect, useState } from 'react'

type Theme = 'system' | 'light' | 'dark'

const KEY = 'haliphron.theme'

function read(): Theme {
  try {
    const stored = window.localStorage.getItem(KEY)
    return stored === 'light' || stored === 'dark' ? stored : 'system'
  } catch {
    return 'system'
  }
}

/** A per-viewer convenience, so localStorage is the right place for it. */
export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(read)

  useEffect(() => {
    const root = document.documentElement
    if (theme === 'system') root.removeAttribute('data-theme')
    else root.setAttribute('data-theme', theme)
    try {
      if (theme === 'system') window.localStorage.removeItem(KEY)
      else window.localStorage.setItem(KEY, theme)
    } catch {
      /* the choice lasts for this page only */
    }
  }, [theme])

  const next: Record<Theme, Theme> = { system: 'light', light: 'dark', dark: 'system' }
  const label: Record<Theme, string> = { system: 'Theme: auto', light: 'Theme: light', dark: 'Theme: dark' }

  return (
    <button className="ghost sm" onClick={() => setTheme(next[theme])}>
      {label[theme]}
    </button>
  )
}
