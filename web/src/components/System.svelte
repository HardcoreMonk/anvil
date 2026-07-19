<script>
  import { _ } from 'svelte-i18n'
  import AuditLog from './AuditLog.svelte'
  import WatchdogPanel from './WatchdogPanel.svelte'
  import ConfiguredClients from './ConfiguredClients.svelte'
  import Monitoring from './Monitoring.svelte'
  import MCPGateway from './MCPGateway.svelte'

  let tab = 'audit' // audit | watchdog | clients | monitoring | mcp
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('system.title')}</h1>
</div>

<div class="tabs">
  <button class="tab" class:active={tab === 'audit'} onclick={() => (tab = 'audit')}>{$_('system.tabAudit')}</button>
  <button class="tab" class:active={tab === 'watchdog'} onclick={() => (tab = 'watchdog')}>{$_('system.tabWatchdog')}</button>
  <button class="tab" class:active={tab === 'clients'} onclick={() => (tab = 'clients')}>{$_('system.tabClients')}</button>
  <button class="tab" class:active={tab === 'mcp'} onclick={() => (tab = 'mcp')}>{$_('system.tabMCP')}</button>
  <button class="tab" class:active={tab === 'monitoring'} onclick={() => (tab = 'monitoring')}>{$_('system.tabMonitoring')}</button>
</div>

<!-- Mounting only the active sub-view means its onMount starts a fresh poll and
     onDestroy clears the timer on tab switch — no manual coordination. -->
{#if tab === 'audit'}
  <AuditLog />
{:else if tab === 'watchdog'}
  <WatchdogPanel />
{:else if tab === 'clients'}
  <ConfiguredClients />
{:else if tab === 'mcp'}
  <MCPGateway />
{:else}
  <Monitoring />
{/if}

<style>
  .tabs { display: flex; gap: 8px; margin-bottom: 16px; }
  .tab {
    background: transparent; border: 1px solid var(--border); color: var(--muted);
    border-radius: 6px; padding: 6px 14px; font-size: 13px;
  }
  .tab:hover { background: var(--panel-2); }
  .tab.active { color: var(--text); border-color: var(--accent); }
</style>
