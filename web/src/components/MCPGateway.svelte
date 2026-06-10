<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'

  // GET /config/mcp → { enabled, endpoint, server_count }
  let status = { enabled: false, endpoint: '', server_count: 0 }
  // GET /config/mcp/servers → [{ id, namespace, url, profiles, has_credential, up, error }]
  let servers = []
  let loading = true
  let timer = null

  async function refresh() {
    try {
      const [st, srv] = await Promise.all([apiJSON('/config/mcp'), apiJSON('/config/mcp/servers')])
      status = st || status
      servers = Array.isArray(srv) ? srv : []
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  onMount(() => {
    refresh()
    // Health is probed server-side per request, so poll less aggressively.
    timer = setInterval(refresh, 8000)
  })
  onDestroy(() => clearInterval(timer))
</script>

<div class="panel">
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if !status.enabled}
    <p class="muted">{$_('mcp.disabled')}</p>
    <pre class="hint">EPHEMERA_MCP_ENABLED=1 sudo ./ephemera-daemon</pre>
  {:else}
    <div class="row" style="gap:24px; margin-bottom:16px; flex-wrap:wrap;">
      <div>
        <div class="muted lbl">{$_('mcp.endpoint')}</div>
        <div class="mono">{status.endpoint}</div>
      </div>
      <div>
        <div class="muted lbl">{$_('mcp.serverCount')}</div>
        <div>{status.server_count}</div>
      </div>
    </div>
    {#if servers.length === 0}
      <span class="muted">{$_('mcp.empty')}</span>
    {:else}
      <table>
        <thead>
          <tr>
            <th>{$_('mcp.colId')}</th>
            <th>{$_('mcp.colNamespace')}</th>
            <th>{$_('mcp.colUrl')}</th>
            <th>{$_('mcp.colProfiles')}</th>
            <th>{$_('mcp.colCredential')}</th>
            <th>{$_('mcp.colHealth')}</th>
          </tr>
        </thead>
        <tbody>
          {#each servers as s (s.id)}
            <tr>
              <td class="mono">{s.id}</td>
              <td class="mono">{s.namespace}</td>
              <td class="mono url">{s.url}</td>
              <td>{s.profiles && s.profiles.length ? s.profiles.join(', ') : $_('mcp.allProfiles')}</td>
              <td>{s.has_credential ? $_('mcp.credYes') : $_('mcp.credNo')}</td>
              <td>
                {#if s.up}
                  <span class="pill active">{$_('mcp.up')}</span>
                {:else}
                  <span class="pill expired" title={s.error || ''}>{$_('mcp.down')}</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  {/if}
</div>

<style>
  .lbl { font-size: 12px; margin-bottom: 2px; }
  .hint { background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; padding: 8px 10px; font-size: 13px; color: var(--text); }
  .url { max-width: 360px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.active { color: var(--ok); border-color: var(--ok); }
  .pill.expired { color: var(--err); border-color: var(--err); }
</style>
