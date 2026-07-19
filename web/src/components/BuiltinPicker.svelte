<script>
  import { _ } from 'svelte-i18n'

  // Registry from GET /config/builtins: [{ id, label, description, default }].
  // Two-way bound array of selected builtin ids: selected.
  let { options = [], selected = $bindable([]), disabled = false } = $props()

  // Localized description for a builtin, falling back to the API's English text
  // for any id without an i18n entry (e.g. a builtin added to the registry later).
  function descFor(ext) {
    return $_('builtins.items.' + ext.id + '.desc', { default: ext.description || '' })
  }
</script>

<!-- Self-contained styles (no reliance on the global .opt): when this picker sits
     inside a .field, the global `.field label { display:block }` and
     `input { width:100% }` would otherwise win on specificity and stack the
     checkbox above the name. The scoped class selectors below outrank them. -->
<div class="builtins">
  {#each options as ext (ext.id)}
    <div class="bi">
      <label class="brow">
        <input type="checkbox" bind:group={selected} value={ext.id} {disabled} />
        <span class="bname">{ext.label}</span>
      </label>
      <div class="muted bdesc">{descFor(ext)}</div>
    </div>
  {/each}
  {#if options.length === 0}
    <span class="muted">{$_('builtins.none')}</span>
  {/if}
</div>

<style>
  .builtins {
    display: flex;
    flex-direction: column;
    gap: 12px;
    background: var(--panel-2);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 12px;
  }
  .bi { display: flex; flex-direction: column; gap: 2px; }
  /* The row: checkbox and name on one line, vertically centered. */
  .brow {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 0;
    color: var(--text);
    cursor: pointer;
  }
  /* Keep the checkbox its natural size — the global `input { width:100% }` would
     otherwise stretch it to a full-width block that stacks above the name. */
  .brow input {
    width: auto;
    margin: 0;
    flex: 0 0 auto;
  }
  .bname { color: var(--text); }
  /* Indent the description under the name, past the checkbox + gap. */
  .bdesc { font-size: 12px; margin-left: 26px; }
</style>
