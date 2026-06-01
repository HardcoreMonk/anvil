<script>
  import { onMount, createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  const dispatch = createEventDispatcher()

  let profiles = [] // [{ name, provider, model }]
  let selected = '' // chosen profile name
  let busy = false
  let result = null // VMSpawnResult once created (carries the one-time agent_token)

  onMount(async () => {
    try {
      const data = await apiJSON('/config/profiles')
      profiles = Array.isArray(data) ? data : []
      if (profiles.length) selected = profiles[0].name
    } catch (e) {
      // Fetch error: leave the list empty; Create still works with the default.
    }
  })

  async function spawn() {
    busy = true
    try {
      // "default" maps to no profile (the daemon's default config).
      const body = JSON.stringify(selected && selected !== 'default' ? { profile: selected } : {})
      result = await apiJSON('/vms', { method: 'POST', body })
      toast(get(_)('createModal.createdToast', { values: { id: result.vm_id } }), 'ok')
      dispatch('spawned')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }

  async function copyToken() {
    try {
      await navigator.clipboard.writeText(result.agent_token)
      toast(get(_)('createModal.tokenCopied'), 'ok')
    } catch (e) {
      toast(get(_)('createModal.copyFailed'), 'error')
    }
  }

  function label(p) {
    return p.provider || p.model ? `${p.name} — ${p.provider} / ${p.model}` : p.name
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    {#if !result}
      <h2>{$_('createModal.title')}</h2>
      <div class="field">
        <label for="prof">{$_('createModal.profileLabel')}</label>
        {#if profiles.length}
          <select id="prof" bind:value={selected}>
            {#each profiles as p (p.name)}
              <option value={p.name}>{label(p)}</option>
            {/each}
          </select>
        {:else}
          <select id="prof" disabled><option>{$_('common.loading')}</option></select>
        {/if}
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('createModal.profileHint')}</div>
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close}>{$_('common.cancel')}</button>
        <button on:click={spawn} disabled={busy}>{busy ? $_('createModal.creating') : $_('createModal.create')}</button>
      </div>
    {:else}
      <h2>{$_('createModal.createdTitle')}</h2>
      <p class="muted">{$_('createModal.vmId')}</p>
      <div class="token-box">{result.vm_id}</div>
      <p class="muted" style="margin-top:12px;">{$_('createModal.agentToken')}</p>
      <div class="token-box">{result.agent_token}</div>
      <div class="warn-box">
        {$_('createModal.tokenWarnPre')}<strong>{$_('createModal.tokenWarnBold')}</strong>{$_('createModal.tokenWarnPost')}
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={copyToken}>{$_('createModal.copyToken')}</button>
        <button on:click={close}>{$_('common.done')}</button>
      </div>
    {/if}
  </div>
</div>
