<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  export let flockId
  export let agent // AgentInfo { agent_id, role, ... }

  const dispatch = createEventDispatcher()

  let role = ''
  let busy = false

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
        body: JSON.stringify({ role: role.trim() }),
      })
      toast(get(_)('changeRoleModal.changedToast', { values: { id: agent.agent_id } }), 'ok')
      dispatch('changed')
      dispatch('close')
    } catch (e) {
      // Surfaces 400 "already has role" / spawn failure verbatim.
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
  <div class="modal">
    <h2>{$_('changeRoleModal.title')}</h2>
    <p class="muted">{$_('changeRoleModal.intro', { values: { id: agent.agent_id, role: agent.role } })}</p>
    <div class="field">
      <label for="role">{$_('changeRoleModal.roleLabel')}</label>
      <input id="role" bind:value={role} placeholder={$_('changeRoleModal.rolePlaceholder')} />
    </div>
    <div class="warn-box">{$_('changeRoleModal.warn')}</div>
    <div class="row between" style="margin-top:18px;">
      <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
      <button on:click={change} disabled={busy}>{busy ? $_('changeRoleModal.changing') : $_('changeRoleModal.change')}</button>
    </div>
  </div>
</div>
