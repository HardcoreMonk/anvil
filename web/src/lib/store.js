import { writable } from 'svelte/store'

// auth: ready=bootstrap finished, disabled=server has no clients configured
// (no token needed), token=the current bearer when auth is enabled.
export const auth = writable({ ready: false, disabled: false, token: null })

// view: simple client-side router state. name is one of login|list|detail.
// detail carries the selected vm object (lost on hard reload — bootstrap then
// returns to list, which is acceptable for v0.5.0).
export const view = writable({ name: 'login' })

export const toasts = writable([])

let nextId = 0

export function toast(message, kind = 'info') {
  const id = ++nextId
  toasts.update((t) => [...t, { id, message, kind }])
  setTimeout(() => toasts.update((t) => t.filter((x) => x.id !== id)), 5000)
}
