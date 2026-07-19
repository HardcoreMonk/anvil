<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  let { vmId } = $props() // the VM to snapshot

  const dispatch = createEventDispatcher()

  let type = $state('auto') // "auto" | "full" | "diff"
  let stopAfter = $state(false)
  let busy = $state(false)

  async function create() {
    busy = true
    try {
      // "auto" maps to the empty type so the daemon picks full vs diff itself.
      const body = JSON.stringify({ type: type === 'auto' ? '' : type, stop_after: stopAfter })
      const res = await apiJSON('/vms/' + encodeURIComponent(vmId) + '/snapshot', { method: 'POST', body })
      toast(get(_)('snapshotModal.createdToast', { values: { id: res.snapshot_id } }), 'ok')
      // stop_after destroys the source VM — let the parent leave the detail page.
      dispatch('created', { stopAfter })
      dispatch('close')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    <h2>{$_('snapshotModal.title')}</h2>
    <div class="field">
      <div class="muted" style="margin-bottom:8px;">{$_('snapshotModal.typeLabel')}</div>
      <div class="row" style="gap:18px;">
        <label class="opt"><input type="radio" bind:group={type} value="auto" />{$_('snapshotModal.typeAuto')}</label>
        <label class="opt"><input type="radio" bind:group={type} value="full" />{$_('snapshotModal.typeFull')}</label>
        <label class="opt"><input type="radio" bind:group={type} value="diff" />{$_('snapshotModal.typeDiff')}</label>
      </div>
      <div class="muted" style="margin-top:6px; font-size:12px;">{$_('snapshotModal.typeHint')}</div>
    </div>
    <div class="field">
      <label class="opt"><input type="checkbox" bind:checked={stopAfter} />{$_('snapshotModal.stopAfter')}</label>
      <div class="muted" style="margin-top:6px; font-size:12px;">{$_('snapshotModal.stopAfterHint')}</div>
    </div>
    <div class="row between" style="margin-top:18px;">
      <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
      <button on:click={create} disabled={busy}>{busy ? $_('snapshotModal.creating') : $_('snapshotModal.create')}</button>
    </div>
  </div>
</div>

<style>
  /* Override the global `.field label` block/muted rule for inline option rows. */
  .opt { display: inline-flex; align-items: center; gap: 6px; color: var(--text); cursor: pointer; }
  .opt input { width: auto; margin: 0; }
</style>
