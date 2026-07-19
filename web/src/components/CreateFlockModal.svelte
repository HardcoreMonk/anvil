<script>
  import { createEventDispatcher, onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  const dispatch = createEventDispatcher()

  let task = $state('')
  // Each row: { role: free-text label, profile: profile name }. One VM per row.
  let roles = $state([{ role: '', profile: 'default' }])
  let profiles = $state([]) // [{ name, provider, model, ... }] from GET /config/profiles
  let maxAgents = $state('') // optional per-flock cap; blank → daemon default (20)
  let busy = $state(false)
  let result = $state(null) // FlockCreateResponse once created (carries one-time agent_tokens)

  onMount(async () => {
    try {
      const data = await apiJSON('/config/profiles')
      profiles = Array.isArray(data) ? data : []
    } catch (e) {
      // Non-fatal: leave the list empty; the role name then serves as the profile.
    }
  })

  // New rows default to the first listed profile ("default" if present).
  let defaultProfile = $derived(profiles.length ? profiles[0].name : 'default')

  function addRole() {
    roles = [...roles, { role: '', profile: defaultProfile }]
  }
  function removeRole(i) {
    roles = roles.filter((_, idx) => idx !== i)
  }

  async function create() {
    const rows = roles.filter((r) => r.role.trim())
    if (!rows.length) {
      toast(get(_)('createFlockModal.rolesRequired'), 'error')
      return
    }
    busy = true
    try {
      const body = {
        task: task.trim(),
        roles: rows.map((r) => r.role.trim()),
        profiles: rows.map((r) => r.profile),
      }
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
  let tokenRows = $derived(result ? Object.entries(result.agent_tokens || {}) : [])

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
        {#each roles as row, i}
          <div class="row" style="gap:8px; margin-bottom:8px;">
            <input bind:value={roles[i].role} placeholder={$_('createFlockModal.rolePlaceholder')} style="flex:1;" />
            <select bind:value={roles[i].profile} title={$_('createFlockModal.profileLabel')} style="flex:1;">
              {#each (profiles.length ? profiles : [{ name: 'default' }]) as p (p.name)}
                <option value={p.name}>{p.name}</option>
              {/each}
            </select>
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
