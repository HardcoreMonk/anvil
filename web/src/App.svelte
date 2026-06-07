<script>
  import { onMount } from 'svelte'
  import { _, locale } from 'svelte-i18n'
  import { auth, view, toasts } from './lib/store.js'
  import { loadStoredToken, clearToken } from './lib/api.js'
  import { setLocale } from './lib/i18n.js'
  import Login from './components/Login.svelte'
  import VMList from './components/VMList.svelte'
  import VMDetail from './components/VMDetail.svelte'
  import Snapshots from './components/Snapshots.svelte'
  import Settings from './components/Settings.svelte'
  import Toasts from './components/Toasts.svelte'

  // Bootstrap: figure out whether auth is enabled and whether we already hold a
  // valid token, by probing GET /vms (the same check the API enforces).
  async function bootstrap() {
    try {
      const probe = await fetch('/vms')
      if (probe.ok) {
        // No clients configured server-side → auth disabled, no token needed.
        auth.set({ ready: true, disabled: true, token: null })
        view.set({ name: 'list' })
        return
      }
    } catch (e) {
      // Network error — fall through to the login screen.
    }

    const stored = loadStoredToken()
    if (stored) {
      try {
        const r = await fetch('/vms', { headers: { Authorization: 'Bearer ' + stored } })
        if (r.ok) {
          auth.set({ ready: true, disabled: false, token: stored })
          view.set({ name: 'list' })
          return
        }
      } catch (e) {
        /* ignore, drop to login */
      }
      clearToken()
    }
    auth.set({ ready: true, disabled: false, token: null })
    view.set({ name: 'login' })
  }

  function logout() {
    clearToken()
    auth.update((s) => ({ ...s, token: null }))
    view.set({ name: 'login' })
  }

  onMount(bootstrap)
</script>

{#if !$auth.ready}
  <div class="center"><span class="muted">{$_('common.loading')}</span></div>
{:else if $view.name === 'login'}
  <Login />
{:else}
  <div class="nav">
    <span class="brand">EPHEMERA</span>
    <span class="badge">v0.5.1</span>
    <button class="ghost" on:click={() => view.set({ name: 'list' })}>{$_('nav.vms')}</button>
    <button class="ghost" on:click={() => view.set({ name: 'snapshots' })}>{$_('nav.snapshots')}</button>
    <button class="ghost" on:click={() => view.set({ name: 'settings' })}>{$_('nav.settings')}</button>
    <span class="spacer"></span>
    <div class="lang-switch">
      <button class="lang-btn" class:active={$locale === 'en'} on:click={() => setLocale('en')}>EN</button>
      <button class="lang-btn" class:active={$locale === 'ko'} on:click={() => setLocale('ko')}>한국어</button>
    </div>
    {#if $auth.disabled}
      <span class="badge">{$_('nav.authDisabled')}</span>
    {:else}
      <button class="ghost" on:click={logout}>{$_('nav.logout')}</button>
    {/if}
  </div>
  <div class="container">
    {#if $view.name === 'list'}
      <VMList />
    {:else if $view.name === 'detail'}
      <VMDetail vm={$view.vm} />
    {:else if $view.name === 'snapshots'}
      <Snapshots />
    {:else if $view.name === 'settings'}
      <Settings />
    {/if}
  </div>
{/if}

<Toasts />

<style>
  .lang-switch { display: flex; gap: 4px; }
  .lang-btn {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--muted);
    border-radius: 6px;
    padding: 4px 10px;
    font-size: 12px;
  }
  .lang-btn:hover { background: var(--panel-2); }
  .lang-btn.active { color: var(--text); border-color: var(--accent); }
</style>
