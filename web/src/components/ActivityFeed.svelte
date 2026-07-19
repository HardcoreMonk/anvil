<script>
  import { onMount, onDestroy } from 'svelte'
  import { _ } from 'svelte-i18n'
  import { apiFetch, apiJSON } from '../lib/api.js'
  import { streamSSE } from '../lib/stream.js'
  import { toast } from '../lib/store.js'

  let { flockId } = $props()

  let messages = $state([])
  let status = $state('connecting') // connecting | connected | disconnected
  let controller = null
  let gen = 0 // generation token: ignore frames from a superseded stream
  let logEl = $state()

  const postAgent = 'operator' // UI posts are always operator-authored (no agent impersonation)
  let postBody = $state('')
  let posting = $state(false)

  function scrollDown() {
    queueMicrotask(() => {
      if (logEl) logEl.scrollTop = logEl.scrollHeight
    })
  }

  async function connect() {
    const myGen = ++gen
    status = 'connecting'
    messages = []
    controller = new AbortController()
    try {
      // EventSource cannot send the bearer, so we read the SSE stream over fetch
      // (apiFetch injects the token). The stream replays the full history first,
      // then live messages; it resolves when the server closes it or we abort.
      const resp = await apiFetch('/flocks/' + encodeURIComponent(flockId) + '/wall', { signal: controller.signal })
      if (myGen !== gen) return
      if (!resp.ok) {
        status = 'disconnected'
        return
      }
      status = 'connected'
      await streamSSE(resp, (m) => {
        if (myGen !== gen) return // late frame from a superseded reconnect
        messages = [...messages, m]
        scrollDown()
      })
      if (myGen === gen) status = 'disconnected'
    } catch (e) {
      if (e.name === 'AbortError') return // unmount or manual reconnect
      if (myGen === gen && e.message !== 'unauthorized') status = 'disconnected'
    }
  }

  function reconnect() {
    if (controller) controller.abort()
    connect()
  }

  async function post() {
    if (!postBody.trim()) return
    posting = true
    try {
      // The posted message echoes back over the SSE subscription, so we do not
      // append it locally here.
      await apiJSON('/flocks/' + encodeURIComponent(flockId) + '/post', {
        method: 'POST',
        body: JSON.stringify({ agent_id: postAgent, body: postBody.trim() }),
      })
      postBody = ''
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      posting = false
    }
  }

  function onKey(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      post()
    }
  }

  onMount(() => {
    connect()
  })
  onDestroy(() => {
    gen++ // invalidate any in-flight stream
    if (controller) controller.abort()
  })
</script>

<div class="panel">
  <div class="row between" style="margin-bottom:12px;">
    <h2 style="margin:0;">{$_('activityFeed.title')}</h2>
    <div class="row" style="gap:8px;">
      <span class="conn {status}">{$_('activityFeed.' + status)}</span>
      {#if status === 'disconnected'}
        <button class="ghost sm" on:click={reconnect}>{$_('activityFeed.reconnect')}</button>
      {/if}
    </div>
  </div>

  {#if messages.length === 0}
    <div class="muted" style="padding:12px 0;">{$_('activityFeed.empty')}</div>
  {:else}
    <div class="feed" bind:this={logEl}>
      {#each messages as m (m.seq)}
        <div class="msg">
          <span class="ts mono">{new Date(m.timestamp).toLocaleTimeString()}</span>
          <span class="who mono">{m.agent_id}</span>
          <span class="body">{m.body}</span>
        </div>
      {/each}
    </div>
  {/if}

  <div class="composer row" style="gap:8px; margin-top:12px;">
    <span class="who-static mono" title={$_('activityFeed.postAs')}>operator</span>
    <input bind:value={postBody} placeholder={$_('activityFeed.composerBody')} on:keydown={onKey} disabled={posting} />
    <button on:click={post} disabled={posting || !postBody.trim()}>{posting ? $_('activityFeed.posting') : $_('activityFeed.post')}</button>
  </div>
</div>

<style>
  .conn { font-size: 12px; }
  .conn.connecting { color: var(--warn); }
  .conn.connected { color: var(--ok); }
  .conn.disconnected { color: var(--err); }
  button.sm { padding: 4px 8px; font-size: 12px; }
  .feed {
    max-height: 320px; overflow-y: auto;
    display: flex; flex-direction: column; gap: 6px;
    background: var(--bg); border: 1px solid var(--border); border-radius: 6px;
    padding: 10px;
  }
  .msg { display: flex; gap: 8px; align-items: baseline; font-size: 13px; }
  .ts { color: var(--muted); font-size: 11px; flex-shrink: 0; }
  .who { color: var(--accent); flex-shrink: 0; }
  .body { white-space: pre-wrap; word-break: break-word; }
  .who-static { color: var(--accent); font-size: 13px; align-self: center; flex-shrink: 0; }
  .composer input { flex: 1; }
</style>
