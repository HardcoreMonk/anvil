<script>
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'
  import BuiltinPicker from './BuiltinPicker.svelte'

  // The profile whose builtin extensions are being edited (from the Settings row).
  // Builtin extension registry from GET /config/builtins (loaded once by Settings).
  let { name, options = [], onclose } = $props()

  let selected = $state([])
  let loading = $state(true)
  let busy = $state(false)

  const path = '/config/profiles/' + encodeURIComponent(name) + '/builtins'

  onMount(async () => {
    try {
      const data = await apiJSON(path)
      selected = (data && Array.isArray(data.builtins)) ? data.builtins : []
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  })

  async function save() {
    busy = true
    try {
      await apiJSON(path, { method: 'PUT', body: JSON.stringify({ builtins: selected }) })
      toast(get(_)('builtinsModal.savedToast', { values: { name } }), 'ok')
      onclose?.()
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    onclose?.()
  }
</script>

<div class="modal-backdrop" role="presentation" onclick={(e) => e.target === e.currentTarget && close()} onkeydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    <h2>{$_('builtinsModal.title', { values: { name } })}</h2>
    <p class="muted">{$_('builtinsModal.intro')}</p>
    {#if loading}
      <span class="muted">{$_('common.loading')}</span>
    {:else}
      <BuiltinPicker {options} bind:selected disabled={busy} />
      <div class="muted" style="margin-top:6px; font-size:12px;">{$_('builtins.tokenHint')}</div>
    {/if}
    <div class="row between" style="margin-top:18px;">
      <button class="ghost" onclick={close} disabled={busy}>{$_('common.cancel')}</button>
      <button onclick={save} disabled={busy || loading}>{busy ? $_('builtinsModal.saving') : $_('builtinsModal.save')}</button>
    </div>
  </div>
</div>
