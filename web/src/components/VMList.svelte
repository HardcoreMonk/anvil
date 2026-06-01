<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { view, toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'
  import SpawnModal from './SpawnModal.svelte'

  let vms = []
  let loading = true
  let showSpawn = false
  let timer = null

  async function refresh() {
    try {
      const data = await apiJSON('/vms?stats=true')
      vms = Array.isArray(data) ? data : []
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  function open(vm) {
    view.set({ name: 'detail', vm })
  }

  function pct(n) {
    return (n == null ? 0 : n).toFixed(1) + '%'
  }

  onMount(() => {
    refresh()
    timer = setInterval(refresh, 4000)
  })
  onDestroy(() => clearInterval(timer))
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('vmlist.title')}</h1>
  <div class="row" style="gap:8px;">
    <button class="ghost" on:click={refresh}>{$_('common.refresh')}</button>
    <button on:click={() => (showSpawn = true)}>{$_('vmlist.create')}</button>
  </div>
</div>

<div class="panel">
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if vms.length === 0}
    <span class="muted">{$_('vmlist.empty')}</span>
  {:else}
    <table>
      <thead>
        <tr><th>{$_('vmlist.colId')}</th><th>{$_('vmlist.colIp')}</th><th>{$_('vmlist.colProfile')}</th><th>{$_('vmlist.colModel')}</th><th>{$_('vmlist.colCpu')}</th><th>{$_('vmlist.colMem')}</th><th>{$_('vmlist.colUptime')}</th></tr>
      </thead>
      <tbody>
        {#each vms as vm (vm.vm_id)}
          <tr class="clickable" on:click={() => open(vm)}>
            <td class="mono">{vm.vm_id}</td>
            <td class="mono">{vm.guest_ip}</td>
            <td>{vm.profile || '—'}</td>
            <td class="mono">{vm.model || '—'}</td>
            <td>{pct(vm.stats?.cpu_percent)}</td>
            <td>{vm.stats?.mem_used_mib ?? 0} / {vm.stats?.mem_total_mib ?? 0}</td>
            <td>{vm.stats?.uptime_seconds ?? 0}s</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if showSpawn}
  <SpawnModal on:close={() => (showSpawn = false)} on:spawned={refresh} />
{/if}
