<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'

  // The bearer token is intentionally absent from GET /config/clients and is
  // therefore never displayed — only name + expiry come over the wire.
  let clients = []
  let loading = true
  let timer = null

  async function refresh() {
    try {
      const data = await apiJSON('/config/clients')
      clients = Array.isArray(data) ? data : []
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
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if clients.length === 0}
    <span class="muted">{$_('system.clientsEmpty')}</span>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{$_('system.colName')}</th>
          <th>{$_('system.colExpires')}</th>
          <th>{$_('system.colStatus')}</th>
        </tr>
      </thead>
      <tbody>
        {#each clients as c (c.name)}
          <tr>
            <td class="mono">{c.name}</td>
            <td>{c.expires ? new Date(c.expires).toLocaleString() : $_('system.never')}</td>
            <td>
              {#if c.expired}
                <span class="pill expired">{$_('system.expired')}</span>
              {:else}
                <span class="pill active">{$_('system.active')}</span>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.active { color: var(--ok); border-color: var(--ok); }
  .pill.expired { color: var(--err); border-color: var(--err); }
</style>
