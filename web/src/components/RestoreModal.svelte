<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast, view } from '../lib/store.js'

  let { snapshot } = $props() // the SnapshotInfo to restore

  const dispatch = createEventDispatcher()

  let busy = $state(false)
  let result = $state(null) // VMRestoreResult once restored (carries the reused agent_token)

  async function restore() {
    busy = true
    try {
      result = await apiJSON('/snapshots/' + encodeURIComponent(snapshot.snapshot_id) + '/restore', { method: 'POST' })
      toast(get(_)('restoreModal.restoredToast', { values: { id: result.vm_id } }), 'ok')
      dispatch('restored')
    } catch (e) {
      // Surfaces the daemon guards verbatim: 409 "source VM still running", 404, etc.
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }

  function goToVm() {
    // VMRestoreResult is a flat VMInfo + token, so it doubles as the detail vm.
    view.set({ name: 'detail', vm: result })
    dispatch('close')
  }

  async function copyToken() {
    try {
      await navigator.clipboard.writeText(result.agent_token)
      toast(get(_)('restoreModal.tokenCopied'), 'ok')
    } catch (e) {
      toast(get(_)('restoreModal.copyFailed'), 'error')
    }
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    {#if !result}
      <h2>{$_('restoreModal.title')}</h2>
      <p class="muted">{$_('restoreModal.confirmIntro', { values: { id: snapshot.snapshot_id } })}</p>
      <div class="warn-box">
        <div>{$_('restoreModal.warnSourceStopped')}</div>
        <div style="margin-top:6px;">{$_('restoreModal.warnNewVm')}</div>
        <div style="margin-top:6px;">{$_('restoreModal.warnResetOnRestart')}</div>
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={restore} disabled={busy}>{busy ? $_('restoreModal.restoring') : $_('restoreModal.restore')}</button>
      </div>
    {:else}
      <h2>{$_('restoreModal.restoredTitle')}</h2>
      <p class="muted">{$_('restoreModal.vmId')}</p>
      <div class="token-box">{result.vm_id}</div>
      <p class="muted" style="margin-top:12px;">{$_('restoreModal.agentToken')}</p>
      <div class="token-box">{result.agent_token}</div>
      <div class="warn-box">
        {$_('restoreModal.tokenWarnPre')}<strong>{$_('restoreModal.tokenWarnBold')}</strong>{$_('restoreModal.tokenWarnPost')}
      </div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={copyToken}>{$_('restoreModal.copyToken')}</button>
        <button on:click={goToVm}>{$_('restoreModal.goToVm')}</button>
      </div>
    {/if}
  </div>
</div>
