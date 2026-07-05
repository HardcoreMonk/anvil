<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'

  let status = null
  let loading = true
  let timer = null

  async function refresh() {
    try {
      status = await apiJSON('/watchdog/status')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  onMount(() => {
    refresh()
    timer = setInterval(refresh, 4000)
  })
  onDestroy(() => clearInterval(timer))

  $: failEntries = status && status.vm_fail_counts ? Object.entries(status.vm_fail_counts) : []
  $: deadList = status && status.vm_dead_marked ? status.vm_dead_marked : []
</script>

{#if loading}
  <div class="panel"><span class="muted">{$_('common.loading')}</span></div>
{:else if !status}
  <div class="panel"><span class="muted">{$_('system.watchdogEmpty')}</span></div>
{:else}
  <div class="panel" style="margin-bottom:16px;">
    <h2>{$_('system.watchdogTunables')}</h2>
    <div class="row" style="gap:40px; flex-wrap:wrap;">
      <div><div class="muted">{$_('system.wdInterval')}</div><div>{status.interval_sec}s</div></div>
      <div><div class="muted">{$_('system.wdTimeout')}</div><div>{status.timeout_sec}s</div></div>
      <div><div class="muted">{$_('system.wdThreshold')}</div><div>{status.dying_threshold}</div></div>
      <div>
        <div class="muted">{$_('system.wdAutoHeal')}</div>
        <div><span class="pill" class:on={status.auto_heal}>{status.auto_heal ? $_('system.on') : $_('system.off')}</span></div>
      </div>
    </div>
    <div class="muted" style="margin-top:12px; font-size:12px;">{$_('system.watchdogEnvHint')}</div>
  </div>

  <div class="panel" style="margin-bottom:16px;">
    <h2>{$_('system.watchdogFailCounts')}</h2>
    {#if failEntries.length === 0}
      <span class="muted">{$_('system.watchdogNoFailures')}</span>
    {:else}
      <table>
        <thead><tr><th>{$_('system.colVm')}</th><th>{$_('system.colCount')}</th></tr></thead>
        <tbody>
          {#each failEntries as [vmId, count] (vmId)}
            <tr><td class="mono">{vmId}</td><td>{count}</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>

  <div class="panel">
    <h2>{$_('system.watchdogDeadMarked')}</h2>
    {#if deadList.length === 0}
      <span class="muted">{$_('system.watchdogNoDead')}</span>
    {:else}
      <div class="row" style="gap:8px; flex-wrap:wrap;">
        {#each deadList as vmId (vmId)}
          <span class="pill dead mono">{vmId}</span>
        {/each}
      </div>
    {/if}
  </div>
{/if}

<style>
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.on { color: var(--ok); border-color: var(--ok); }
  .pill.dead { color: var(--err); border-color: var(--err); }
</style>
