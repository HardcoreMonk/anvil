// streamFrames reads a fetch Response body as newline-delimited JSON and calls
// onFrame(obj) for each complete line. It is used for the daemon's NDJSON task
// stream (POST /vms/{id}/tasks?stream=1) and is generic enough to reuse for the
// fetch-based SSE Activity Feed reader in a later cycle (EventSource cannot send
// the Authorization header, so we parse the stream by hand over fetch instead).
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
