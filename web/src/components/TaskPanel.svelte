<script>
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiFetch } from '../lib/api.js'
  import { streamFrames } from '../lib/stream.js'
  import { toast } from '../lib/store.js'

  export let vmId

  // messages: { role:'user', text } | { role:'assistant', progress:[], output, error, done }
  let messages = []
  let prompt = ''
  let running = false
  let session = '' // goose session name; the agent resumes it across turns
  let prevOutput = '' // last turn's cumulative goose output, to show only the delta
  let controller = null
  let elapsed = 0
  let elapsedTimer = null
  let logEl

  function newSession() {
    // A fresh goose session per conversation. Date.now keeps it unique per VM.
    session = vmId + '-' + Date.now()
    prevOutput = ''
  }

  // goose --resume returns the WHOLE session transcript each turn (every prior
  // assistant reply concatenated). Show only the newest reply by stripping the
  // previous cumulative output as a prefix.
  function takeDelta(full) {
    let reply = full
    if (prevOutput && full.startsWith(prevOutput)) {
      reply = full.slice(prevOutput.length).replace(/^\n+/, '')
    }
    if (full) prevOutput = full
    return reply
  }

  function newConversation() {
    if (running) return
    newSession()
    messages = []
  }

  function scrollDown() {
    queueMicrotask(() => {
      if (logEl) logEl.scrollTop = logEl.scrollHeight
    })
  }

  function startElapsed() {
    const start = Date.now()
    elapsed = 0
    elapsedTimer = setInterval(() => {
      elapsed = Math.floor((Date.now() - start) / 1000)
    }, 1000)
  }

  function stopElapsed() {
    clearInterval(elapsedTimer)
    elapsedTimer = null
  }

  async function send() {
    const text = prompt.trim()
    if (!text || running) return
    prompt = ''
    messages = [...messages, { role: 'user', text }]
    const a = { role: 'assistant', progress: [], output: '', error: '', done: false }
    messages = [...messages, a]
    scrollDown()

    running = true
    controller = new AbortController()
    startElapsed()
    try {
      const resp = await apiFetch('/vms/' + encodeURIComponent(vmId) + '/tasks?stream=1', {
        method: 'POST',
        body: JSON.stringify({ prompt: text, session }),
        signal: controller.signal,
      })
      const ct = resp.headers.get('content-type') || ''

      if (ct.includes('application/x-ndjson')) {
        await streamFrames(resp, (frame) => {
          if (frame.type === 'progress') {
            if (frame.text) {
              a.progress = [...a.progress, frame.text]
              messages = messages // trigger reactivity
              scrollDown()
            }
          } else if (frame.type === 'result') {
            a.output = takeDelta(frame.output || '')
            a.error = frame.error || ''
          }
        })
      } else {
        const data = await resp.json()
        a.output = takeDelta(data.output || '')
        a.error = data.error || ''
      }
    } catch (e) {
      if (e.name === 'AbortError') {
        a.error = get(_)('taskpanel.canceled')
      } else if (e.message !== 'unauthorized') {
        a.error = e.message
        toast(e.message, 'error')
      }
    } finally {
      a.done = true
      messages = messages
      running = false
      stopElapsed()
      controller = null
      scrollDown()
    }
  }

  function cancel() {
    if (controller) controller.abort()
  }

  function onKey(e) {
    // Enter sends; Shift+Enter inserts a newline.
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
    }
  }

  onMount(newSession)
  onDestroy(() => {
    if (controller) controller.abort()
    stopElapsed()
  })
</script>

<div class="panel" style="margin-bottom:16px;">
  <div class="row between" style="margin-bottom:12px;">
    <h2>{$_('taskpanel.title')}</h2>
    <button class="ghost" on:click={newConversation} disabled={running || messages.length === 0}>
      {$_('taskpanel.newConversation')}
    </button>
  </div>

  {#if messages.length}
    <div class="chat" bind:this={logEl}>
      {#each messages as m}
        {#if m.role === 'user'}
          <div class="msg user">
            <div class="who">{$_('taskpanel.you')}</div>
            <div class="bubble">{m.text}</div>
          </div>
        {:else}
          <div class="msg assistant">
            <div class="who">{$_('taskpanel.assistant')}</div>
            {#if m.done}
              {#if m.error}
                <div class="bubble err"><pre>{m.error}</pre></div>
              {:else}
                <div class="bubble"><pre>{m.output || $_('taskpanel.empty')}</pre></div>
              {/if}
            {:else if m.progress.length}
              <div class="bubble log">{#each m.progress as line}<div>{line}</div>{/each}</div>
            {:else}
              <div class="bubble muted">{$_('taskpanel.thinking')}</div>
            {/if}
          </div>
        {/if}
      {/each}
    </div>
  {/if}

  <textarea
    bind:value={prompt}
    rows="3"
    placeholder={$_('taskpanel.placeholder')}
    disabled={running}
    on:keydown={onKey}
  ></textarea>
  <div class="row between" style="margin-top:10px;">
    <span class="muted">{running ? $_('taskpanel.runningFor', { values: { sec: elapsed } }) : $_('taskpanel.hint')}</span>
    {#if running}
      <button class="ghost" on:click={cancel}>{$_('common.cancel')}</button>
    {:else}
      <button on:click={send} disabled={!prompt.trim()}>{$_('taskpanel.send')}</button>
    {/if}
  </div>
</div>

<style>
  .chat {
    max-height: 360px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 12px;
    margin-bottom: 12px;
    padding-right: 4px;
  }
  .msg {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .msg.user {
    align-items: flex-end;
  }
  .who {
    font-size: 11px;
    color: var(--muted);
  }
  .bubble {
    max-width: 85%;
    border-radius: 8px;
    padding: 8px 12px;
    border: 1px solid var(--border);
    background: var(--panel-2);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .msg.user .bubble {
    background: rgba(59, 158, 255, 0.12);
    border-color: var(--accent);
  }
  .bubble.log {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--muted);
    background: var(--bg);
  }
  .bubble.err {
    border-color: var(--err);
    background: rgba(248, 81, 73, 0.1);
  }
  .bubble.muted {
    color: var(--muted);
  }
  .bubble pre {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: var(--mono);
    font-size: 13px;
    color: var(--text);
  }
  textarea {
    width: 100%;
    background: var(--panel-2);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--text);
    padding: 10px;
    font-size: 14px;
    font-family: inherit;
    resize: vertical;
  }
</style>
