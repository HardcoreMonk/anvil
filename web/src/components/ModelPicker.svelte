<script>
  import { _ } from 'svelte-i18n'

  // models: suggested model ids for the current provider.
  // value: the chosen model id (two-way bound by the parent).
  // id: optional, so a parent <label for> can target the select
  let { models = [], value = $bindable(''), id = undefined } = $props()

  const CUSTOM = '__custom__'

  // custom = show the free-text input because the value is not a suggested model.
  let custom = $state(!!value && !models.includes(value))

  // When the suggested list changes so the value IS a suggested model, drop back
  // to the dropdown. (Svelte 4 ran this pre-render via `$:`; $effect runs post-render
  // — behavior-equivalent here: it only flips custom→false, is idempotent, reads
  // value/models and writes custom, so it cannot loop. Verified by the /ui smoke.)
  $effect(() => {
    if (value && models.includes(value)) custom = false
  })

  function onSelect(e) {
    if (e.target.value === CUSTOM) {
      custom = true
      value = ''
    } else {
      custom = false
      value = e.target.value
    }
  }
</script>

<select {id} value={custom ? CUSTOM : value} on:change={onSelect}>
  {#each models as m}
    <option value={m}>{m}</option>
  {/each}
  <option value={CUSTOM}>{$_('profileModal.modelCustom')}</option>
</select>
{#if custom}
  <input bind:value placeholder={$_('profileModal.modelCustomPlaceholder')} style="margin-top:6px;" />
{/if}
