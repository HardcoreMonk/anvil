// streamFrames reads a fetch Response body as newline-delimited JSON and calls
// onFrame(obj) for each complete line. It is used for the daemon's NDJSON task
// stream (POST /vms/{id}/tasks?stream=1). The fetch-based SSE Activity Feed reader
// uses streamSSE (below) instead — SSE frames each payload as `data: {json}`,
// which is not valid NDJSON, so streamFrames would skip every frame.
//
// Resolves when the stream ends. Malformed/partial lines are skipped silently;
// the caller distinguishes frame kinds via the parsed object's own fields
// (e.g. {type:"progress",text} vs {type:"result",output,error}).
export async function streamFrames(resp, onFrame) {
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (value) buffer += decoder.decode(value, { stream: true })

    let nl
    while ((nl = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, nl).trim()
      buffer = buffer.slice(nl + 1)
      if (!line) continue
      try {
        onFrame(JSON.parse(line))
      } catch {
        /* ignore non-JSON / partial line */
      }
    }
    if (done) break
  }

  const last = buffer.trim()
  if (last) {
    try {
      onFrame(JSON.parse(last))
    } catch {
      /* ignore */
    }
  }
}

// streamSSE reads a fetch Response body as Server-Sent Events and calls
// onMessage(obj) for each `data:` frame's parsed JSON. The daemon's Town Wall
// stream (GET /flocks/{id}/wall) emits one-line `data: {json}\n\n` frames
// (sseEmit), so we strip the `data:` prefix and JSON.parse the remainder.
//
// EventSource cannot send the Authorization header, so the Activity Feed reads
// the SSE stream by hand over fetch — apiFetch injects the bearer, then this
// parser runs over the response body. Resolves when the stream ends (the caller
// aborts via the fetch signal on unmount). Blank lines, `:` comments, and
// malformed frames are skipped silently.
export async function streamSSE(resp, onMessage) {
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (value) buffer += decoder.decode(value, { stream: true })

    let nl
    while ((nl = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, nl).trim()
      buffer = buffer.slice(nl + 1)
      if (!line || line.startsWith(':')) continue // blank or comment
      if (!line.startsWith('data:')) continue // ignore event:/id:/retry: fields
      const payload = line.slice(5).trim()
      if (!payload) continue
      try {
        onMessage(JSON.parse(payload))
      } catch {
        /* ignore non-JSON / partial frame */
      }
    }
    if (done) break
  }
}
