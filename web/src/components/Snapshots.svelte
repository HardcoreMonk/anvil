<script>
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { toast } from '../lib/store.js'
  import { apiJSON } from '../lib/api.js'
  import RestoreModal from './RestoreModal.svelte'

  let snapshots = []
  let loading = true
  let timer = null
  let restoreTarget = null // snapshot to restore (opens RestoreModal)
  let deleteTarget = null // snapshot pending delete confirmation
  let deleting = false

  async function refresh() {
    try {
      const data = await apiJSON('/snapshots')
      const arr = Array.isArray(data) ? data : []
      // GET /snapshots iterates a map (unsorted) — sort newest-first so the 4s
      // poll does not reshuffle rows.
      snapshots = arr.sort((a, b) => new Date(b.created_at) - new Date(a.created_at))
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  async function confirmDelete() {
    deleting = true
    try {
      await apiJSON('/snapshots/' + encodeURIComponent(deleteTarget.snapshot_id), { method: 'DELETE' })
      toast(get(_)('snapshots.deletedToast', { values: { id: deleteTarget.snapshot_id } }), 'ok')
      deleteTarget = null
      refresh()
    } catch (e) {
      // Surfaces the 409 base-dependency error ("delete the diff first") verbatim.
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      deleting = false
    }
  }

  onMount(() => {
    refresh()
    timer = setInterval(refresh, 4000)
  })
  onDestroy(() => clearInterval(timer))
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('snapshots.title')}</h1>
  <button class="ghost" on:click={refresh}>{$_('common.refresh')}</button>
</div>

<div class="panel">
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if snapshots.length === 0}
    <span class="muted">{$_('snapshots.empty')}</span>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{$_('snapshots.colId')}</th>
          <th>{$_('snapshots.colSource')}</th>
          <th>{$_('snapshots.colType')}</th>
          <th>{$_('snapshots.colBase')}</th>
          <th>{$_('snapshots.colCreated')}</th>
          <th>{$_('snapshots.colActions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each snapshots as s (s.snapshot_id)}
          <tr>
            <td class="mono">{s.snapshot_id}</td>
            <td class="mono">{s.source_vm_id}</td>
            <td>
              <span class="pill" class:diff={s.snapshot_type === 'diff'}>
                {s.snapshot_type === 'diff' ? $_('snapshots.typeDiff') : $_('snapshots.typeFull')}
              </span>
            </td>
            <td class="mono">{s.base_snapshot_id || '—'}</td>
            <td>{new Date(s.created_at).toLocaleString()}</td>
            <td>
              <div class="row" style="gap:8px;">
                <button class="ghost" on:click={() => (restoreTarget = s)}>{$_('snapshots.restoreBtn')}</button>
                <button class="danger" on:click={() => (deleteTarget = s)}>{$_('snapshots.deleteBtn')}</button>
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if restoreTarget}
  <RestoreModal snapshot={restoreTarget} on:close={() => (restoreTarget = null)} on:restored={refresh} />
{/if}

{#if deleteTarget}
  <div class="modal-backdrop" role="presentation" on:click|self={() => (deleteTarget = null)} on:keydown={(e) => e.key === 'Escape' && (deleteTarget = null)}>
    <div class="modal">
      <h2>{$_('snapshots.deleteTitle')}</h2>
      <p class="muted">{$_('snapshots.deleteConfirm', { values: { id: deleteTarget.snapshot_id } })}</p>
      <div class="warn-box">{$_('snapshots.deleteWarnRestored')}</div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (deleteTarget = null)} disabled={deleting}>{$_('common.cancel')}</button>
        <button class="danger" on:click={confirmDelete} disabled={deleting}>
          {deleting ? $_('snapshots.deleting') : $_('snapshots.deleteBtn')}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.diff { color: var(--accent); border-color: var(--accent); }
</style>
