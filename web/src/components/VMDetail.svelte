<script>
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { view, toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'
  import TaskPanel from './TaskPanel.svelte'
  import SnapshotModal from './SnapshotModal.svelte'

  export let vm // the VMInfo selected from the list

  let stats = null
  let health = null // { status: "idle" | "busy" }
  let destroying = false
  let confirmingDelete = false
  let showSnapshot = false
  let statsTimer = null
  let healthTimer = null

  async function refreshStats() {
    try {
      stats = await apiJSON('/vms/' + encodeURIComponent(vm.vm_id) + '/stats')
    } catch (e) {
      if (e.message !== 'unauthorized') {
        // The VM may have been destroyed; stop polling.
        stopPolling()
      }
    }
  }

  async function refreshHealth() {
    try {
      health = await apiJSON('/vms/' + encodeURIComponent(vm.vm_id) + '/health')
    } catch (e) {
      if (e.message !== 'unauthorized') health = null
    }
  }

  function stopPolling() {
    clearInterval(statsTimer)
    clearInterval(healthTimer)
  }

  async function confirmDelete() {
    destroying = true
    try {
      await apiJSON('/vms/' + encodeURIComponent(vm.vm_id), { method: 'DELETE' })
      toast(get(_)('vmdetail.deletedToast', { values: { id: vm.vm_id } }), 'ok')
      view.set({ name: 'list' })
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
      confirmingDelete = false
    } finally {
      destroying = false
    }
  }

  function back() {
    view.set({ name: 'list' })
  }

  function onSnapshotCreated(e) {
    // stop_after destroyed the VM — leave the now-dead detail page.
    if (e.detail && e.detail.stopAfter) {
      stopPolling()
      view.set({ name: 'list' })
    }
  }

  onMount(() => {
    refreshStats()
    refreshHealth()
    statsTimer = setInterval(refreshStats, 2000)
    healthTimer = setInterval(refreshHealth, 3000)
  })
  onDestroy(stopPolling)
</script>

<div class="row" style="gap:10px; margin-bottom:16px;">
  <button class="ghost" on:click={back}>← {$_('common.back')}</button>
  <h1 class="mono">{vm.vm_id}</h1>
  {#if health}
    <span class="badge">{$_('vmdetail.agentBadge', { values: { status: $_('status.' + health.status) } })}</span>
  {/if}
</div>

<div class="panel" style="margin-bottom:16px;">
  <h2>{$_('vmdetail.info')}</h2>
  <div class="row" style="gap:40px;">
    <div><div class="muted">{$_('vmdetail.guestIp')}</div><div class="mono">{vm.guest_ip}</div></div>
    <div><div class="muted">{$_('vmdetail.profile')}</div><div>{vm.profile || 'default'}</div></div>
    <div><div class="muted">{$_('vmdetail.model')}</div><div class="mono">{vm.provider ? vm.provider + ' / ' : ''}{vm.model || '—'}</div></div>
    <div><div class="muted">{$_('vmdetail.agentUrl')}</div><div class="mono">{vm.agent_url}</div></div>
  </div>
</div>

<div class="panel" style="margin-bottom:16px;">
  <h2>{$_('vmdetail.liveStats')}</h2>
  {#if !stats}
    <span class="muted">{$_('vmdetail.waitingStats')}</span>
  {:else}
    <div class="row" style="gap:40px; flex-wrap:wrap;">
      <div><div class="muted">{$_('vmdetail.cpu')}</div><div>{(stats.cpu_percent ?? 0).toFixed(1)}%</div></div>
      <div><div class="muted">{$_('vmdetail.memory')}</div><div>{stats.mem_used_mib} / {stats.mem_total_mib} MiB</div></div>
      <div><div class="muted">{$_('vmdetail.uptime')}</div><div>{stats.uptime_seconds}s</div></div>
      <div><div class="muted">{$_('vmdetail.net')}</div><div>{stats.network_rx_bytes} / {stats.network_tx_bytes} B</div></div>
      <div><div class="muted">{$_('vmdetail.agentStat')}</div><div>{stats.agent_busy ? $_('status.busy') : $_('status.idle')}</div></div>
    </div>
  {/if}
</div>

<TaskPanel vmId={vm.vm_id} />

<div class="panel" style="margin-bottom:16px;">
  <h2>{$_('vmdetail.snapshotsSection')}</h2>
  <p class="muted" style="margin-bottom:12px;">{$_('vmdetail.snapshotsHint')}</p>
  <button on:click={() => (showSnapshot = true)}>{$_('vmdetail.createSnapshotBtn')}</button>
</div>

<div class="panel">
  <h2>{$_('vmdetail.dangerZone')}</h2>
  <button class="danger" on:click={() => (confirmingDelete = true)} disabled={destroying}>
    {$_('vmdetail.deleteBtn')}
  </button>
</div>

{#if confirmingDelete}
  <div class="modal-backdrop" role="presentation" on:click|self={() => (confirmingDelete = false)} on:keydown={(e) => e.key === 'Escape' && (confirmingDelete = false)}>
    <div class="modal">
      <h2>{$_('vmdetail.deleteBtn')}</h2>
      <p class="muted">{$_('vmdetail.deleteConfirm', { values: { id: vm.vm_id } })}</p>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (confirmingDelete = false)} disabled={destroying}>{$_('common.cancel')}</button>
        <button class="danger" on:click={confirmDelete} disabled={destroying}>
          {destroying ? $_('vmdetail.deleting') : $_('vmdetail.deleteBtn')}
        </button>
      </div>
    </div>
  </div>
{/if}

{#if showSnapshot}
  <SnapshotModal vmId={vm.vm_id} on:close={() => (showSnapshot = false)} on:created={onSnapshotCreated} />
{/if}
