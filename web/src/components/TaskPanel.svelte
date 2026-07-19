<script>
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiFetch, apiJSON } from '../lib/api.js'
  import { streamFrames } from '../lib/stream.js'
  import { toast } from '../lib/store.js'

  let { vmId } = $props()

  // messages: { role:'user', text } | { role:'assistant', progress:[], output, error, done }
  let messages = $state([])
  let prompt = $state('')
  let running = $state(false)
  let session = '' // goose session name; the agent resumes it across turns
  let controller = null
  let elapsed = $state(0)
  let elapsedTimer = null
  let logEl = $state()
  let sessionList = $state([]) // [{ name, created_at, title, turns, last_output }] from the agent
  let selectedKey = $state('__new__') // conversation picker: '__new__' or a session name
  let resuming = $state(false) // true when the active session is a resumed prior conversation
  let resumeTurns = $state(0)

  function newSession() {
    // A fresh goose session per conversation. Date.now keeps it unique per VM.
    session = vmId + '-' + Date.now()
    resuming = false
    selectedKey = '__new__'
  }

  function newConversation() {
    if (running) return
    newSession()
    messages = []
  }

  async function loadSessions() {
    try {
      const data = await apiJSON('/vms/' + encodeURIComponent(vmId) + '/sessions')
      sessionList = Array.isArray(data) ? data : []
      // Once the agent knows the active session, reflect it in the picker.
      if (session && sessionList.some((s) => s.name === session)) selectedKey = session
    } catch (e) {
      sessionList = [] // listing is best-effort; the panel still works without it
    }
  }

  async function resumeSession(s) {
    // Continue a prior conversation: point the agent at its session (it returns
    // only the latest turn's reply going forward) and repaint the earlier turns
    // from the agent's transcript endpoint (cache, or a cold-restart export
    // fallback). Best-effort: on failure the chat still resumes, just without the
    // prior turns shown.
    session = s.name
    selectedKey = s.name
    resumeTurns = s.turns || 0
    resuming = true
    messages = []
    try {
      const data = await apiJSON(
        '/vms/' + encodeURIComponent(vmId) + '/sessions/' + encodeURIComponent(s.name) + '/transcript'
      )
      const turns = data && Array.isArray(data.turns) ? data.turns : []
      if (turns.length) {
        messages = turns.map((t) =>
          t.role === 'user'
            ? { role: 'user', text: t.text }
            : { role: 'assistant', progress: [], output: t.text, error: '', done: true }
        )
      }
    } catch (e) {
      // transcript is best-effort; leave messages empty and let the chat continue
    }
  }

  function onPickSession() {
    if (selectedKey === '__new__') newConversation()
    else {
      const s = sessionList.find((x) => x.name === selectedKey)
      if (s) resumeSession(s)
    }
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
    messages = [...messages, { role: 'assistant', progress: [], output: '', error: '', done: false }]
    // Mutate the assistant message THROUGH the $state array (by index) so deep
    // reactivity fires. A captured raw ref + `messages = messages` is a strict-`===`
    // no-op under runes and would freeze streamed progress/output (legacy Svelte 4 idiom).
    const aIdx = messages.length - 1
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
              messages[aIdx].progress = [...messages[aIdx].progress, frame.text]
              scrollDown()
            }
          } else if (frame.type === 'result') {
            messages[aIdx].output = frame.output || ''
            messages[aIdx].error = frame.error || ''
          }
        })
      } else {
        const data = await resp.json()
        messages[aIdx].output = data.output || ''
        messages[aIdx].error = data.error || ''
      }
    } catch (e) {
      if (e.name === 'AbortError') {
        messages[aIdx].error = get(_)('taskpanel.canceled')
      } else if (e.message !== 'unauthorized') {
        messages[aIdx].error = e.message
        toast(e.message, 'error')
      }
    } finally {
      messages[aIdx].done = true
      running = false
      stopElapsed()
      controller = null
      scrollDown()
      loadSessions() // refresh the picker: new session appears; turn count updates
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

  onMount(async () => {
    await loadSessions()
    // Newest-first: continue the latest conversation (seamless after a restore);
    // a freshly-spawned VM has none, so start a new one.
    if (sessionList.length) await resumeSession(sessionList[0])
    else newSession()
  })
  onDestroy(() => {
    if (controller) controller.abort()
    stopElapsed()
  })
</script>

<div class="panel" style="margin-bottom:16px;">
  <div class="row between" style="margin-bottom:12px;">
    <h2>{$_('taskpanel.title')}</h2>
    {#if sessionList.length}
      <select class="conv" bind:value={selectedKey} onchange={onPickSession} disabled={running}>
        <option value="__new__">{$_('taskpanel.newConversation')}</option>
        {#each sessionList as s (s.name)}
          <option value={s.name}>{s.title || s.name}</option>
        {/each}
      </select>
    {:else}
      <button class="ghost" onclick={newConversation} disabled={running || messages.length === 0}>
        {$_('taskpanel.newConversation')}
      </button>
    {/if}
  </div>

  {#if resuming && !messages.length}
    <div class="resume-note muted">{$_('taskpanel.resumingNote', { values: { turns: resumeTurns } })}</div>
  {/if}

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
    onkeydown={onKey}
  ></textarea>
  <div class="row between" style="margin-top:10px;">
    <span class="muted">{running ? $_('taskpanel.runningFor', { values: { sec: elapsed } }) : $_('taskpanel.hint')}</span>
    {#if running}
      <button class="ghost" onclick={cancel}>{$_('common.cancel')}</button>
    {:else}
      <button onclick={send} disabled={!prompt.trim()}>{$_('taskpanel.send')}</button>
    {/if}
  </div>
</div>

<style>
  .conv { width: auto; max-width: 260px; }
  .resume-note { font-size: 12px; margin-bottom: 12px; }
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
