<script>
  import { createEventDispatcher, onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  // The profile whose system.md is being edited (passed from the Settings row).
  export let name

  const dispatch = createEventDispatcher()

  let value = ''
  let loading = true
  let busy = false
  let confirmClear = false // Clear awaiting confirmation, mirroring the Delete flow

  const path = '/config/profiles/' + encodeURIComponent(name) + '/system'

  onMount(async () => {
    try {
      const data = await apiJSON(path)
      value = (data && data.system_md) || ''
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  })

  async function save() {
    busy = true
    try {
      await apiJSON(path, { method: 'PUT', body: JSON.stringify({ system_md: value }) })
      toast(get(_)('systemPrompt.savedToast', { values: { name } }), 'ok')
      dispatch('close')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  async function clear() {
    busy = true
    try {
      await apiJSON(path, { method: 'DELETE' })
      toast(get(_)('systemPrompt.clearedToast', { values: { name } }), 'ok')
      value = ''
      confirmClear = false
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal wide">
    <h2>{$_('systemPrompt.title', { values: { name } })}</h2>
    <p class="muted">{$_('systemPrompt.intro')}</p>
    {#if loading}
      <span class="muted">{$_('common.loading')}</span>
    {:else}
      <textarea bind:value rows="14" placeholder={$_('systemPrompt.placeholder')} disabled={busy}></textarea>
    {/if}
    <div class="row between" style="margin-top:18px;">
      <div class="row" style="gap:8px;">
        {#if confirmClear}
          <button class="danger" on:click={clear} disabled={busy}>{$_('systemPrompt.clearConfirm')}</button>
          <button class="ghost" on:click={() => (confirmClear = false)} disabled={busy}>{$_('common.cancel')}</button>
        {:else}
          <button class="danger" on:click={() => (confirmClear = true)} disabled={busy || loading}>{$_('systemPrompt.clear')}</button>
        {/if}
      </div>
      <div class="row" style="gap:8px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={save} disabled={busy || loading}>{busy ? $_('systemPrompt.saving') : $_('systemPrompt.save')}</button>
      </div>
    </div>
  </div>
</div>

<style>
  .modal.wide { width: 640px; }
  textarea {
    width: 100%; background: var(--panel-2); border: 1px solid var(--border);
    border-radius: 6px; color: var(--text); padding: 10px; font-size: 14px;
    font-family: inherit; resize: vertical;
  }
</style>
