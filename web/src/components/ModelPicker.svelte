<script>
  import { _ } from 'svelte-i18n'

  // models: suggested model ids for the current provider.
  // value: the chosen model id (two-way bound by the parent).
  export let models = []
  export let value = ''
  export let id = undefined // optional, so a parent <label for> can target the select

  const CUSTOM = '__custom__'

  // custom = show the free-text input because the value is not a suggested model.
  // Seeded from the initial value, then kept in sync below.
  let custom = !!value && !models.includes(value)

  // When the suggested list changes (e.g. a provider switch sets value to a model
  // that IS in the new list), drop back to the dropdown. Selecting "custom" sets
  // value='' which leaves custom untouched here, so the input stays open.
  $: if (value && models.includes(value)) custom = false

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
