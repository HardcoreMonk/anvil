<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiFetch } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  // The single agent to dispatch a one-shot task to (from a FlockDetail row).
  export let agent // { agent_id, vm_id, role }

  const dispatch = createEventDispatcher()

  let body = ''
  let busy = false
  let controller = null
  let result = null // { status: 'ok'|'busy'|'error', output, error }

  async function send() {
    if (!body.trim()) {
      toast(get(_)('sendTask.bodyRequired'), 'error')
      return
    }
    busy = true
    controller = new AbortController()
    try {
      // One-shot (non-streaming) task on this agent's VM — the targeted
      // counterpart to group Broadcast. A busy agent answers 503.
      const resp = await apiFetch('/vms/' + encodeURIComponent(agent.vm_id) + '/tasks', {
        method: 'POST',
        body: JSON.stringify({ prompt: body.trim() }),
        signal: controller.signal,
      })
      if (resp.status === 503) {
        result = { status: 'busy' }
        return
      }
      const data = await resp.json().catch(() => null)
      if (!resp.ok) {
        result = { status: 'error', output: data && data.output, error: (data && data.error) || 'HTTP ' + resp.status }
        return
      }
      result = { status: 'ok', output: data && data.output, error: data && data.error }
    } catch (e) {
      if (e.name === 'AbortError') toast(get(_)('sendTask.canceled'), 'info')
      else if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
      controller = null
    }
  }

  function cancel() {
    if (controller) controller.abort()
  }
  function close() {
    dispatch('close')
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal wide">
    {#if !result}
      <h2>{$_('sendTask.title', { values: { id: agent.agent_id } })}</h2>
      <p class="muted">{$_('sendTask.intro', { values: { role: agent.role } })}</p>
      <textarea bind:value={body} rows="5" placeholder={$_('sendTask.placeholder')} disabled={busy}></textarea>
      <div class="row between" style="margin-top:18px;">
        {#if busy}
          <span class="muted">{$_('sendTask.sending')}</span>
          <button class="ghost" on:click={cancel}>{$_('common.cancel')}</button>
        {:else}
          <button class="ghost" on:click={close}>{$_('common.cancel')}</button>
          <button on:click={send} disabled={!body.trim()}>{$_('sendTask.send')}</button>
        {/if}
      </div>
    {:else}
      <h2>{$_('sendTask.resultTitle', { values: { id: agent.agent_id } })}</h2>
      <div class="row" style="margin-bottom:12px;">
        <span class="pill {result.status}">{result.status}</span>
      </div>
      <div class="out"><pre>{result.error || result.output || '—'}</pre></div>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={() => (result = null)}>{$_('sendTask.again')}</button>
        <button on:click={close}>{$_('common.done')}</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .modal.wide { width: 640px; }
  textarea {
    width: 100%; background: var(--panel-2); border: 1px solid var(--border);
    border-radius: 6px; color: var(--text); padding: 10px; font-size: 14px;
    font-family: inherit; resize: vertical;
  }
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.ok { color: var(--ok); border-color: var(--ok); }
  .pill.busy { color: var(--warn); border-color: var(--warn); }
  .pill.error { color: var(--err); border-color: var(--err); }
  .out { max-height: 340px; overflow-y: auto; }
  .out pre { margin: 0; white-space: pre-wrap; word-break: break-word; font-family: var(--mono); font-size: 12px; }
</style>
