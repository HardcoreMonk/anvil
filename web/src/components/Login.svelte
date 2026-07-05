<script>
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { auth, view, toast } from '../lib/store.js'
  import { storeToken } from '../lib/api.js'

  let token = ''
  let remember = false
  let busy = false

  async function submit() {
    if (!token.trim()) return
    busy = true
    try {
      // Validate by hitting the same endpoint the API protects.
      const r = await fetch('/vms', { headers: { Authorization: 'Bearer ' + token.trim() } })
      if (r.status === 401) {
        toast(get(_)('login.invalidToken'), 'error')
        return
      }
      if (!r.ok) {
        toast(get(_)('login.unexpected', { values: { status: r.status } }), 'error')
        return
      }
      storeToken(token.trim(), remember)
      auth.set({ ready: true, disabled: false, token: token.trim() })
      view.set({ name: 'list' })
    } catch (e) {
      toast(get(_)('login.unreachable'), 'error')
    } finally {
      busy = false
    }
  }
</script>

<div class="center">
  <div class="panel" style="width: 380px;">
    <h1>Ephemera</h1>
    <p class="muted" style="margin-top:6px;">{$_('login.subtitle')}</p>
    <form on:submit|preventDefault={submit}>
      <div class="field" style="margin-top:16px;">
        <label for="tok">{$_('login.tokenLabel')}</label>
        <input id="tok" type="password" bind:value={token} placeholder={$_('login.tokenPlaceholder')} autocomplete="off" />
      </div>
      <label class="row" style="gap:8px; margin-bottom:14px;">
        <input type="checkbox" bind:checked={remember} style="width:auto;" />
        <span class="muted">{$_('login.remember')}</span>
      </label>
      <button type="submit" disabled={busy} style="width:100%;">
        {busy ? $_('login.checking') : $_('login.signIn')}
      </button>
    </form>
  </div>
</div>
