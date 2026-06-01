import { get } from 'svelte/store'
import { auth, view } from './store.js'

const TOKEN_KEY = 'ephemera_token'

export function loadStoredToken() {
  return sessionStorage.getItem(TOKEN_KEY) || localStorage.getItem(TOKEN_KEY) || null
}

export function storeToken(token, remember) {
  // sessionStorage by default (cleared on tab close — fits an ephemeral product);
  // localStorage only when the user opts into "remember me".
  if (remember) localStorage.setItem(TOKEN_KEY, token)
  else sessionStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(TOKEN_KEY)
}

// apiFetch wraps fetch: it injects the bearer header (unless auth is disabled)
// and centralizes the 401 → login redirect so every caller stays simple.
export async function apiFetch(path, opts = {}) {
  const a = get(auth)
  const headers = new Headers(opts.headers || {})
  if (!a.disabled && a.token) headers.set('Authorization', 'Bearer ' + a.token)
  if (opts.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')

  const resp = await fetch(path, { ...opts, headers })
  if (resp.status === 401) {
    clearToken()
    auth.update((s) => ({ ...s, token: null }))
    view.set({ name: 'login' })
    throw new Error('unauthorized')
  }
  return resp
}

// apiJSON parses the JSON body and surfaces the daemon's {"error":"..."} shape.
export async function apiJSON(path, opts) {
  const resp = await apiFetch(path, opts)
  const text = await resp.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!resp.ok) {
    const msg = data && data.error ? data.error : 'HTTP ' + resp.status
    throw new Error(msg)
  }
  return data
}
