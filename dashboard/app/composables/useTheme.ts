type Theme = 'system' | 'light' | 'dark'

const theme = ref<Theme>('system')

export function useTheme() {
  function init() {
    if (!import.meta.client) return
    const saved = localStorage.getItem('reach-theme') as Theme | null
    if (saved) theme.value = saved
    apply()
  }

  function setTheme(t: Theme) {
    theme.value = t
    if (import.meta.client) {
      localStorage.setItem('reach-theme', t)
    }
    apply()
  }

  function toggle() {
    const current = resolved()
    setTheme(current === 'dark' ? 'light' : 'dark')
  }

  function resolved(): 'light' | 'dark' {
    if (theme.value !== 'system') return theme.value
    if (!import.meta.client) return 'dark'
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }

  function apply() {
    if (!import.meta.client) return
    if (theme.value === 'system') {
      document.documentElement.removeAttribute('data-theme')
    } else {
      document.documentElement.setAttribute('data-theme', theme.value)
    }
  }

  return { theme, setTheme, toggle, resolved, init }
}
