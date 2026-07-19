<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'

  let records = []
  let loading = true
  let timer = null

  // Backend (GET /audit) supports method/client/status/limit filters — NOT path.
  let fMethod = ''
  let fClient = ''
  let fStatus = ''
  let fLimit = '100'

  async function refresh() {
    const p = new URLSearchParams()
    if (fMethod) p.set('method', fMethod)
    if (fClient.trim()) p.set('client', fClient.trim())
    if (fStatus.trim()) p.set('status', fStatus.trim())
    p.set('limit', fLimit)
    try {
      const data = await apiJSON('/audit?' + p.toString())
      records = Array.isArray(data) ? data : [] // server returns newest-first
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
</script>

<div class="panel">
  <div class="filters row" style="gap:8px; margin-bottom:14px; flex-wrap:wrap;">
    <select bind:value={fMethod} onchange={refresh}>
      <option value="">{$_('system.methodAny')}</option>
      <option value="GET">GET</option>
      <option value="POST">POST</option>
      <option value="PUT">PUT</option>
      <option value="DELETE">DELETE</option>
      <option value="PATCH">PATCH</option>
    </select>
    <input bind:value={fClient} oninput={refresh} placeholder={$_('system.filterClient')} />
    <input bind:value={fStatus} oninput={refresh} placeholder={$_('system.filterStatus')} style="width:110px;" />
    <select bind:value={fLimit} onchange={refresh}>
      <option value="100">100</option>
      <option value="250">250</option>
      <option value="1000">1000</option>
    </select>
    <button class="ghost" onclick={refresh}>{$_('common.refresh')}</button>
  </div>

  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if records.length === 0}
    <span class="muted">{$_('system.auditEmpty')}</span>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{$_('system.colTs')}</th>
          <th>{$_('system.colClient')}</th>
          <th>{$_('system.colMethod')}</th>
          <th>{$_('system.colPath')}</th>
          <th>{$_('system.colStatus')}</th>
          <th>{$_('system.colDuration')}</th>
          <th>{$_('system.colRemote')}</th>
          <th>{$_('system.colBytes')}</th>
        </tr>
      </thead>
      <tbody>
        {#each records as r, i (r.ts + '-' + i)}
          <tr>
            <td>{new Date(r.ts).toLocaleString()}</td>
            <td>{r.client || '—'}</td>
            <td>{r.method}</td>
            <td class="mono">{r.path}</td>
            <td><span class="pill" class:err={r.status >= 400}>{r.status}</span></td>
            <td>{r.duration_ms}</td>
            <td class="mono">{r.remote_addr}</td>
            <td>{r.bytes}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .filters :global(input), .filters :global(select) {
    background: var(--panel-2); border: 1px solid var(--border); color: var(--text);
    border-radius: 6px; padding: 6px 10px; font-size: 13px;
  }
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.err { color: var(--err); border-color: var(--err); }
</style>
