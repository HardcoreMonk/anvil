<script>
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  // agent: AgentInfo { agent_id, role, profile, ... }
  let { flockId, agent, onclose, onchanged } = $props()

  let role = $state('')
  let profile = $state('')
  let profiles = $state([])
  let busy = $state(false)

  onMount(async () => {
    try {
      const data = await apiJSON('/config/profiles')
      profiles = Array.isArray(data) ? data : []
      // Default to the agent's current profile (fall back to its role / first profile).
      profile = agent.profile || agent.role || (profiles[0] && profiles[0].name) || 'default'
    } catch (e) {
      // Non-fatal: leave empty; the role name then serves as the profile.
    }
  })

  async function change() {
    if (!role.trim()) {
      toast(get(_)('changeRoleModal.roleRequired'), 'error')
      return
    }
    busy = true
    try {
      // Recreates the agent's VM under the new role (returns VMInfo); the parent
      // refetches the flock to pick up the new vm_id / role.
      await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/agents/' + encodeURIComponent(agent.agent_id), {
        method: 'PATCH',
        body: JSON.stringify({ role: role.trim(), profile }),
      })
      toast(get(_)('changeRoleModal.changedToast', { values: { id: agent.agent_id } }), 'ok')
      onchanged?.()
      onclose?.()
    } catch (e) {
      // Surfaces 400 "already has role" / spawn failure verbatim.
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
    <h2>{$_('changeRoleModal.title')}</h2>
    <p class="muted">{$_('changeRoleModal.intro', { values: { id: agent.agent_id, role: agent.role } })}</p>
    <div class="field">
      <label for="role">{$_('changeRoleModal.roleLabel')}</label>
      <input id="role" bind:value={role} placeholder={$_('changeRoleModal.rolePlaceholder')} />
    </div>
    <div class="field">
      <label for="cr-profile">{$_('changeRoleModal.profileLabel')}</label>
      <select id="cr-profile" bind:value={profile}>
        {#each (profiles.length ? profiles : [{ name: 'default' }]) as p (p.name)}
          <option value={p.name}>{p.name}</option>
        {/each}
      </select>
    </div>
    <div class="warn-box">{$_('changeRoleModal.warn')}</div>
    <div class="row between" style="margin-top:18px;">
      <button class="ghost" onclick={close} disabled={busy}>{$_('common.cancel')}</button>
      <button onclick={change} disabled={busy}>{busy ? $_('changeRoleModal.changing') : $_('changeRoleModal.change')}</button>
    </div>
  </div>
</div>
