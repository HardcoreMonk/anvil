<script>
  import { onMount } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'

  let grafanaUrl = $state('')
  let enabled = $state(false)
  let loading = $state(true)

  // Dashboard uid is configs/observability/dashboards/ephemera-overview.json.
  // kiosk hides Grafana's own chrome so only the panels render inside the iframe.
  let src = $derived(enabled
    ? grafanaUrl.replace(/\/$/, '') + '/d/ephemera-overview/ephemera-overview?kiosk&refresh=5s&theme=dark'
    : '')

  onMount(async () => {
    try {
      const data = await apiJSON('/config/monitoring')
      grafanaUrl = (data && data.grafana_url) || ''
      enabled = !!(data && data.enabled)
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  })
</script>

{#if loading}
  <div class="panel"><span class="muted">{$_('common.loading')}</span></div>
{:else if !enabled}
  <div class="panel"><p class="muted">{$_('system.monitoringDisabled')}</p></div>
{:else}
  <div class="frame-wrap">
    <iframe title="Grafana" src={src}></iframe>
  </div>
{/if}

<style>
  .frame-wrap { border: 1px solid var(--border); border-radius: 8px; overflow: hidden; background: var(--panel); }
  iframe { width: 100%; height: 80vh; border: 0; display: block; }
</style>
