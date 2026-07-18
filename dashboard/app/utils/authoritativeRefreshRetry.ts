export function createAuthoritativeRefreshRetry(
  retry: () => void,
  options: {
    baseMs?: number
    maxMs?: number
    setTimer?: typeof setTimeout
    clearTimer?: typeof clearTimeout
  } = {},
) {
  const baseMs = options.baseMs ?? 1_000
  const maxMs = options.maxMs ?? 30_000
  const setTimer = options.setTimer ?? setTimeout
  const clearTimer = options.clearTimer ?? clearTimeout
  let timer: ReturnType<typeof setTimeout> | null = null
  let attempt = 0
  let disposed = false

  function failed() {
    if (disposed || timer) return
    const delay = Math.min(maxMs, baseMs * 2 ** attempt++)
    timer = setTimer(() => {
      timer = null
      if (!disposed) retry()
    }, delay)
  }

  function succeeded() {
    attempt = 0
    if (timer) clearTimer(timer)
    timer = null
  }

  function dispose() {
    disposed = true
    succeeded()
  }

  return { failed, succeeded, dispose }
}
