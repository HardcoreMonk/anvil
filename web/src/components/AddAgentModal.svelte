<script>
  import { createEventDispatcher, onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  export let flockId

  const dispatch = createEventDispatcher()

  let role = ''
  let profile = 'default'
  let profiles = []
  let busy = false
  let result = null // { agent_id, role, vm_id, agent_url, agent_token }

  onMount(async () => {
    try {
      const data = await apiJSON('/config/profiles')
      profiles = Array.isArray(data) ? data : []
      if (profiles.length) profile = profiles[0].name
    } catch (e) {
      // Non-fatal: leave default; the role name then serves as the profile.
    }
  })

  async function add() {
    if (!role.trim()) {
      toast(get(_)('addAgentModal.roleRequired'), 'error')
      return
    }
    busy = true
    try {
      result = await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/agents', {
        method: 'POST',
        body: JSON.stringify({ role: role.trim(), profile }),
      })
      toast(get(_)('addAgentModal.addedToast', { values: { id: result.agent_id } }), 'ok')
      dispatch('added')
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
      toast(get(_)('addAgentModal.tokenCopied'), 'ok')
    } catch (e) {
      toast(get(_)('addAgentModal.copyFailed'), 'error')
    }
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    {#if !result}
      <h2>{$_('addAgentModal.title')}</h2>
      <div class="field">
        <label for="role">{$_('addAgentModal.roleLabel')}</label>
        <input id="role" bind:value={role} placeholder={$_('addAgentModal.rolePlaceholder')} />
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('addAgentModal.roleHint')}</div>
      </div>
      <div class="field">
        <label for="aa-profile">{$_('addAgentModal.profileLabel')}</label>
        <select id="aa-profile" bind:value={profile}>
          {#each (profiles.length ? profiles : [{ name: 'default' }]) as p (p.name)}
            <option value={p.name}>{p.name}</option>
          {/each}
        </select>
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={add} disabled={busy}>{busy ? $_('addAgentModal.adding') : $_('addAgentModal.add')}</button>
      </div>
    {:else}
      <h2>{$_('addAgentModal.addedTitle')}</h2>
      <p class="muted">{$_('addAgentModal.agentId')}</p>
      <div class="token-box">{result.agent_id}</div>
      <p class="muted" style="margin-top:12px;">{$_('addAgentModal.agentToken')}</p>
      <div class="token-box">{result.agent_token}</div>
      <div class="warn-box">
        {$_('addAgentModal.tokenWarnPre')}<strong>{$_('addAgentModal.tokenWarnBold')}</strong>{$_('addAgentModal.tokenWarnPost')}
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={copyToken}>{$_('addAgentModal.copyToken')}</button>
        <button on:click={close}>{$_('common.done')}</button>
      </div>
    {/if}
  </div>
</div>
