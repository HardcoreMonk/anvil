<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiFetch } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  let { flockId } = $props()

  const dispatch = createEventDispatcher()

  let body = $state('')
  let busy = $state(false)
  let controller = null
  let result = $state(null) // { agents, sent, skipped, failed, results{agent_id → {status, output, error}} }

  async function send() {
    if (!body.trim()) {
      toast(get(_)('broadcastModal.bodyRequired'), 'error')
      return
    }
    busy = true
    controller = new AbortController()
    try {
      // Broadcast blocks until every agent's task finishes — can be slow, so it
      // is cancelable. It returns 200 even if every agent failed; the truth is in
      // the per-agent results, not the HTTP status.
      const resp = await apiFetch('/flocks/' + encodeURIComponent(flockId) + '/broadcast', {
        method: 'POST',
        body: JSON.stringify({ body: body.trim() }),
        signal: controller.signal,
      })
      const data = await resp.json()
      if (!resp.ok) {
        toast(data && data.error ? data.error : 'HTTP ' + resp.status, 'error')
        return
      }
      result = data
    } catch (e) {
      if (e.name === 'AbortError') toast(get(_)('broadcastModal.canceled'), 'info')
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

  let resultRows = $derived(result ? Object.entries(result.results || {}) : [])
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal wide">
    {#if !result}
      <h2>{$_('broadcastModal.title')}</h2>
      <p class="muted">{$_('broadcastModal.intro')}</p>
      <textarea bind:value={body} rows="4" placeholder={$_('broadcastModal.placeholder')} disabled={busy}></textarea>
      <div class="row between" style="margin-top:18px;">
        {#if busy}
          <span class="muted">{$_('broadcastModal.sending')}</span>
          <button class="ghost" on:click={cancel}>{$_('common.cancel')}</button>
        {:else}
          <button class="ghost" on:click={close}>{$_('common.cancel')}</button>
          <button on:click={send} disabled={!body.trim()}>{$_('broadcastModal.send')}</button>
        {/if}
      </div>
    {:else}
      <h2>{$_('broadcastModal.resultTitle')}</h2>
      <div class="row" style="gap:10px; margin-bottom:12px; flex-wrap:wrap;">
        <span class="pill ok">{$_('broadcastModal.sent')}: {result.sent}</span>
        <span class="pill busy">{$_('broadcastModal.skipped')}: {result.skipped}</span>
        <span class="pill error">{$_('broadcastModal.failed')}: {result.failed}</span>
      </div>
      <div class="results">
        <table>
          <thead>
            <tr>
              <th>{$_('broadcastModal.colAgent')}</th>
              <th>{$_('broadcastModal.colStatus')}</th>
              <th>{$_('broadcastModal.colOutput')}</th>
            </tr>
          </thead>
          <tbody>
            {#each resultRows as [agentId, res] (agentId)}
              <tr>
                <td class="mono">{agentId}</td>
                <td><span class="pill {res.status}">{res.status}</span></td>
                <td class="out"><pre>{res.error || res.output || '—'}</pre></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="row between" style="margin-top:18px;">
        <span></span>
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
  .results { max-height: 340px; overflow-y: auto; }
  .pill { font-size: 12px; border: 1px solid var(--border); border-radius: 12px; padding: 2px 10px; color: var(--muted); }
  .pill.ok { color: var(--ok); border-color: var(--ok); }
  .pill.busy { color: var(--warn); border-color: var(--warn); }
  .pill.error { color: var(--err); border-color: var(--err); }
  .out { max-width: 360px; }
  .out pre { margin: 0; white-space: pre-wrap; word-break: break-word; font-family: var(--mono); font-size: 12px; }
</style>
