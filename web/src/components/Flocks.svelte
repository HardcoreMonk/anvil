<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { view, toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'
  import CreateFlockModal from './CreateFlockModal.svelte'

  let flocks = []
  let loading = true
  let showCreate = false
  let timer = null

  async function refresh() {
    try {
      const data = await apiJSON('/flocks')
      const arr = Array.isArray(data) ? data : []
      // GET /flocks iterates a map (unsorted) — sort newest-first so the 4s poll
      // does not reshuffle rows.
      flocks = arr.sort((a, b) => new Date(b.created_at) - new Date(a.created_at))
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  function open(flock) {
    view.set({ name: 'flockDetail', flock })
  }

  // agents is a map agent_id → AgentInfo on GET responses (see Flock.MarshalJSON).
  function agentCount(f) {
    return f.agents ? Object.keys(f.agents).length : 0
  }

  onMount(() => {
    refresh()
    timer = setInterval(refresh, 4000)
  })
  onDestroy(() => clearInterval(timer))
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('orchestration.title')}</h1>
  <div class="row" style="gap:8px;">
    <button class="ghost" onclick={refresh}>{$_('common.refresh')}</button>
    <button onclick={() => (showCreate = true)}>{$_('orchestration.create')}</button>
  </div>
</div>

<div class="panel">
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if flocks.length === 0}
    <span class="muted">{$_('orchestration.empty')}</span>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{$_('orchestration.colId')}</th>
          <th>{$_('orchestration.colTask')}</th>
          <th>{$_('orchestration.colAgents')}</th>
          <th>{$_('orchestration.colStatus')}</th>
          <th>{$_('orchestration.colCreated')}</th>
        </tr>
      </thead>
      <tbody>
        {#each flocks as f (f.flock_id)}
          <tr class="clickable" onclick={() => open(f)}>
            <td class="mono">{f.flock_id}</td>
            <td class="task">{f.task || '—'}</td>
            <td>{agentCount(f)}</td>
            <td>
              <span class="pill" class:paused={f.paused}>
                {f.paused ? $_('orchestration.paused') : $_('orchestration.active')}
              </span>
            </td>
            <td>{new Date(f.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if showCreate}
  <CreateFlockModal onclose={() => (showCreate = false)} oncreated={refresh} />
{/if}

<style>
  .task { max-width: 360px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--ok); border-color: var(--ok); }
  .pill.paused { color: var(--warn); border-color: var(--warn); }
</style>
