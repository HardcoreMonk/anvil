<script>
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { view, toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'
  import ActivityFeed from './ActivityFeed.svelte'
  import AddAgentModal from './AddAgentModal.svelte'
  import ChangeRoleModal from './ChangeRoleModal.svelte'
  import BroadcastModal from './BroadcastModal.svelte'

  export let flock // selected from the list; refetched for live state

  let detail = flock
  const flockId = flock.flock_id
  let timer = null
  let busyAction = false // pause/resume/remove/restart/delete in flight

  let showAddAgent = false
  let showBroadcast = false
  let changeRoleTarget = null
  let confirmingRemove = null // agent pending remove
  let confirmingRestart = null // agent pending restart
  let confirmingDelete = false

  // agents is a map agent_id → AgentInfo on GET responses (Flock.MarshalJSON).
  $: agents = detail && detail.agents ? Object.values(detail.agents) : []

  async function refreshDetail() {
    try {
      detail = await apiJSON('/flocks/' + encodeURIComponent(flockId))
    } catch (e) {
      if (e.message !== 'unauthorized') {
        // The flock was deleted (404) or is unreachable — return to the list.
        stopPolling()
        view.set({ name: 'flocks' })
      }
    }
  }

  function stopPolling() {
    clearInterval(timer)
  }

  async function lifecycle(action) {
    busyAction = true
    try {
      await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/' + action, { method: 'POST' })
      await refreshDetail()
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busyAction = false
    }
  }

  async function confirmRemove() {
    const a = confirmingRemove
    busyAction = true
    try {
      await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/agents/' + encodeURIComponent(a.agent_id), { method: 'DELETE' })
      toast(get(_)('flockDetail.removedToast', { values: { id: a.agent_id } }), 'ok')
      confirmingRemove = null
      await refreshDetail()
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busyAction = false
    }
  }

  async function confirmRestart() {
    const a = confirmingRestart
    busyAction = true
    try {
      await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/agents/' + encodeURIComponent(a.agent_id) + '/restart', { method: 'POST' })
      toast(get(_)('flockDetail.restartedToast', { values: { id: a.agent_id } }), 'ok')
      confirmingRestart = null
      await refreshDetail()
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busyAction = false
    }
  }

  async function confirmDeleteFlock() {
    busyAction = true
    try {
      await apiJSON('/flocks/' + encodeURIComponent(flockId), { method: 'DELETE' })
      toast(get(_)('flockDetail.deletedToast', { values: { id: flockId } }), 'ok')
      stopPolling()
      view.set({ name: 'flocks' })
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
      confirmingDelete = false
    } finally {
      busyAction = false
    }
  }

  function back() {
    view.set({ name: 'flocks' })
  }

  onMount(() => {
    refreshDetail()
    timer = setInterval(refreshDetail, 4000)
  })
  onDestroy(stopPolling)
</script>

<div class="row" style="gap:10px; margin-bottom:16px;">
  <button class="ghost" on:click={back}>← {$_('common.back')}</button>
  <h1 class="mono">{flockId}</h1>
  <span class="pill" class:paused={detail.paused}>
    {detail.paused ? $_('orchestration.paused') : $_('orchestration.active')}
  </span>
</div>

<div class="panel" style="margin-bottom:16px;">
  <h2>{$_('flockDetail.info')}</h2>
  <div class="row" style="gap:40px; flex-wrap:wrap;">
    <div><div class="muted">{$_('flockDetail.task')}</div><div>{detail.task || '—'}</div></div>
    <div><div class="muted">{$_('flockDetail.agentCount')}</div><div>{agents.length}{detail.max_agents ? ' / ' + detail.max_agents : ''}</div></div>
  </div>
  <div class="row" style="gap:8px; margin-top:16px; flex-wrap:wrap;">
    {#if detail.paused}
      <button on:click={() => lifecycle('resume')} disabled={busyAction}>{$_('flockDetail.resume')}</button>
    {:else}
      <button class="ghost" on:click={() => lifecycle('pause')} disabled={busyAction}>{$_('flockDetail.pause')}</button>
    {/if}
    <button class="ghost" on:click={() => (showAddAgent = true)} disabled={busyAction}>{$_('flockDetail.addAgent')}</button>
    <button class="ghost" on:click={() => (showBroadcast = true)} disabled={busyAction || agents.length === 0}>{$_('flockDetail.broadcast')}</button>
  </div>
</div>

<div class="panel" style="margin-bottom:16px;">
  <h2>{$_('flockDetail.agentsSection')}</h2>
  {#if agents.length === 0}
    <span class="muted">{$_('flockDetail.noAgents')}</span>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{$_('flockDetail.colAgentId')}</th>
          <th>{$_('flockDetail.colRole')}</th>
          <th>{$_('flockDetail.colStatus')}</th>
          <th>{$_('flockDetail.colVm')}</th>
          <th>{$_('flockDetail.colActions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each agents as a (a.agent_id)}
          <tr>
            <td class="mono">{a.agent_id}</td>
            <td>{a.role}</td>
            <td><span class="pill status-{a.status}">{$_('status.' + a.status)}</span></td>
            <td class="mono">{a.vm_id}</td>
            <td>
              <div class="row" style="gap:8px;">
                <button class="ghost sm" on:click={() => (changeRoleTarget = a)} disabled={busyAction}>{$_('flockDetail.changeRole')}</button>
                <button class="ghost sm" on:click={() => (confirmingRestart = a)} disabled={busyAction}>{$_('flockDetail.restart')}</button>
                <button class="danger sm" on:click={() => (confirmingRemove = a)} disabled={busyAction}>{$_('flockDetail.remove')}</button>
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<ActivityFeed {flockId} {agents} />

<div class="panel" style="margin-top:16px;">
  <h2>{$_('flockDetail.dangerZone')}</h2>
  <button class="danger" on:click={() => (confirmingDelete = true)} disabled={busyAction}>{$_('flockDetail.deleteGroup')}</button>
</div>

{#if showAddAgent}
  <AddAgentModal {flockId} on:close={() => (showAddAgent = false)} on:added={refreshDetail} />
{/if}
{#if showBroadcast}
  <BroadcastModal {flockId} on:close={() => (showBroadcast = false)} />
{/if}
{#if changeRoleTarget}
  <ChangeRoleModal {flockId} agent={changeRoleTarget} on:close={() => (changeRoleTarget = null)} on:changed={refreshDetail} />
{/if}

{#if confirmingRemove}
  <div class="modal-backdrop" role="presentation" on:click|self={() => (confirmingRemove = null)} on:keydown={(e) => e.key === 'Escape' && (confirmingRemove = null)}>
    <div class="modal">
      <h2>{$_('flockDetail.remove')}</h2>
      <p class="muted">{$_('flockDetail.removeConfirm', { values: { id: confirmingRemove.agent_id } })}</p>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (confirmingRemove = null)} disabled={busyAction}>{$_('common.cancel')}</button>
        <button class="danger" on:click={confirmRemove} disabled={busyAction}>{$_('flockDetail.remove')}</button>
      </div>
    </div>
  </div>
{/if}

{#if confirmingRestart}
  <div class="modal-backdrop" role="presentation" on:click|self={() => (confirmingRestart = null)} on:keydown={(e) => e.key === 'Escape' && (confirmingRestart = null)}>
    <div class="modal">
      <h2>{$_('flockDetail.restart')}</h2>
      <p class="muted">{$_('flockDetail.restartConfirm', { values: { id: confirmingRestart.agent_id } })}</p>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (confirmingRestart = null)} disabled={busyAction}>{$_('common.cancel')}</button>
        <button on:click={confirmRestart} disabled={busyAction}>{$_('flockDetail.restart')}</button>
      </div>
    </div>
  </div>
{/if}

{#if confirmingDelete}
  <div class="modal-backdrop" role="presentation" on:click|self={() => (confirmingDelete = false)} on:keydown={(e) => e.key === 'Escape' && (confirmingDelete = false)}>
    <div class="modal">
      <h2>{$_('flockDetail.deleteGroup')}</h2>
      <p class="muted">{$_('flockDetail.deleteConfirm', { values: { id: flockId } })}</p>
      <div class="warn-box">{$_('flockDetail.deleteWarn')}</div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (confirmingDelete = false)} disabled={busyAction}>{$_('common.cancel')}</button>
        <button class="danger" on:click={confirmDeleteFlock} disabled={busyAction}>{$_('flockDetail.deleteGroup')}</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--ok); border-color: var(--ok); }
  .pill.paused { color: var(--warn); border-color: var(--warn); }
  .pill.status-spawning { color: var(--accent); border-color: var(--accent); }
  .pill.status-ready { color: var(--ok); border-color: var(--ok); }
  .pill.status-busy { color: var(--warn); border-color: var(--warn); }
  .pill.status-done { color: var(--muted); border-color: var(--border); }
  .pill.status-dead { color: var(--err); border-color: var(--err); }
  .pill.status-paused { color: var(--warn); border-color: var(--warn); }
  button.sm { padding: 4px 8px; font-size: 12px; }
</style>
