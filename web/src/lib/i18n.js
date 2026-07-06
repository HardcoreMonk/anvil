// svelte-i18n setup: English + Korean, bundled synchronously so the $_ store
// resolves immediately (no async loader / $isLoading gating needed).
import { addMessages, init, getLocaleFromNavigator, locale } from 'svelte-i18n'
import en from '../locales/en.json'
import ko from '../locales/ko.json'

const LOCALE_KEY = 'ephemera_locale'

addMessages('en', en)
addMessages('ko', ko)

// Initial locale: a previously saved choice wins; otherwise fall back to the
// browser language (Korean when it starts with "ko", English for everything else).
function initialLocale() {
  try {
    const saved = localStorage.getItem(LOCALE_KEY)
    if (saved === 'en' || saved === 'ko') return saved
  } catch (e) {
    // localStorage may be unavailable (e.g. private mode); fall through to detection.
  }
  const nav = (getLocaleFromNavigator() || 'en').toLowerCase()
  return nav.startsWith('ko') ? 'ko' : 'en'
}

init({ fallbackLocale: 'en', initialLocale: initialLocale() })

// Keep the document's <html lang> attribute in sync for accessibility.
locale.subscribe((l) => {
  if (l && typeof document !== 'undefined') document.documentElement.lang = l
})

// setLocale switches the active language and remembers the choice for next time.
export function setLocale(l) {
  locale.set(l)
  try {
    localStorage.setItem(LOCALE_KEY, l)
  } catch (e) {
    // Ignore persistence failures; the in-memory switch still applies.
  }
}
