<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  const dispatch = createEventDispatcher()

  let task = ''
  let roles = [''] // dynamic role inputs; the daemon spawns one VM per role
  let maxAgents = '' // optional per-flock cap; blank → daemon default (20)
  let busy = false
  let result = null // FlockCreateResponse once created (carries one-time agent_tokens)

  function addRole() {
    roles = [...roles, '']
  }
  function removeRole(i) {
    roles = roles.filter((_, idx) => idx !== i)
  }

  async function create() {
    const cleanRoles = roles.map((r) => r.trim()).filter(Boolean)
    if (!cleanRoles.length) {
      toast(get(_)('createFlockModal.rolesRequired'), 'error')
      return
    }
    busy = true
    try {
      const body = { task: task.trim(), roles: cleanRoles }
      const n = parseInt(maxAgents, 10)
      if (!Number.isNaN(n)) body.max_agents = n
      // One VM per role spawns sequentially server-side, so a large group can
      // take minutes; the button stays in its "creating" state until it returns.
      result = await apiJSON('/flocks', { method: 'POST', body: JSON.stringify(body) })
      toast(get(_)('createFlockModal.createdToast', { values: { id: result.flock_id } }), 'ok')
      dispatch('created')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }

  // agent_tokens maps agent_id → token; turn it into rows for one-time display.
  $: tokenRows = result ? Object.entries(result.agent_tokens || {}) : []

  async function copyToken(token) {
    try {
      await navigator.clipboard.writeText(token)
      toast(get(_)('createFlockModal.tokenCopied'), 'ok')
    } catch (e) {
      toast(get(_)('createFlockModal.copyFailed'), 'error')
    }
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    {#if !result}
      <h2>{$_('createFlockModal.title')}</h2>
      <div class="field">
        <label for="task">{$_('createFlockModal.taskLabel')}</label>
        <input id="task" bind:value={task} placeholder={$_('createFlockModal.taskPlaceholder')} />
      </div>
      <div class="field">
        <div class="muted" style="margin-bottom:6px;">{$_('createFlockModal.rolesLabel')}</div>
        {#each roles as role, i}
          <div class="row" style="gap:8px; margin-bottom:8px;">
            <input bind:value={roles[i]} placeholder={$_('createFlockModal.rolePlaceholder')} />
            <button class="ghost" on:click={() => removeRole(i)} disabled={roles.length === 1} title={$_('createFlockModal.removeRole')}>✕</button>
          </div>
        {/each}
        <button class="ghost" on:click={addRole}>{$_('createFlockModal.addRole')}</button>
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('createFlockModal.rolesHint')}</div>
      </div>
      <div class="field">
        <label for="max">{$_('createFlockModal.maxAgentsLabel')}</label>
        <input id="max" type="number" min="1" bind:value={maxAgents} placeholder={$_('createFlockModal.maxAgentsPlaceholder')} />
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={create} disabled={busy}>{busy ? $_('createFlockModal.creating') : $_('createFlockModal.create')}</button>
      </div>
    {:else}
      <h2>{$_('createFlockModal.createdTitle')}</h2>
      <p class="muted">{$_('createFlockModal.flockId')}</p>
      <div class="token-box">{result.flock_id}</div>
      <p class="muted" style="margin-top:14px;">{$_('createFlockModal.tokensIntro')}</p>
      {#each tokenRows as [agentId, token] (agentId)}
        <div class="mono" style="margin-top:10px; font-size:12px; color:var(--muted);">{agentId}</div>
        <div class="token-box">{token}</div>
        <button class="ghost" style="margin-top:6px;" on:click={() => copyToken(token)}>{$_('createFlockModal.copyToken')}</button>
      {/each}
      <div class="warn-box">
        {$_('createFlockModal.tokenWarnPre')}<strong>{$_('createFlockModal.tokenWarnBold')}</strong>{$_('createFlockModal.tokenWarnPost')}
      </div>
      <div class="row between" style="margin-top:18px;">
        <span></span>
        <button on:click={close}>{$_('common.done')}</button>
      </div>
    {/if}
  </div>
</div>
