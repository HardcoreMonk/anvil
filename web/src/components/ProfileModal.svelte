<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'
  import ModelPicker from './ModelPicker.svelte'
  import BuiltinPicker from './BuiltinPicker.svelte'

  // Available providers only: [{ id, label, default_model, suggested_models }].
  export let providers = []
  // VM sizing presets from GET /config/presets: [{ id, label, vcpu_count, mem_size_mib }].
  export let presets = []
  // Builtin extension registry from GET /config/builtins: [{ id, label, description, default }].
  export let builtins = []

  const dispatch = createEventDispatcher()

  let name = ''
  let provider = providers.length ? providers[0].id : ''
  let model = providers.length ? providers[0].default_model : ''
  let vcpu = 1
  let mem = 1024
  // Pre-select the registry defaults (currently "developer"), all removable.
  let selectedBuiltins = builtins.filter((b) => b.default).map((b) => b.id)
  let busy = false

  // Suggested models track the selected provider.
  $: suggested = (providers.find((p) => p.id === provider) || {}).suggested_models || []

  // The preset whose sizing matches the current vcpu/mem, or null for a custom mix.
  $: activePreset = presets.find((p) => p.vcpu_count === vcpu && p.mem_size_mib === mem) || null

  // Apply a preset's sizing to the form. Users may still fine-tune the fields after.
  function applyPreset(p) {
    vcpu = p.vcpu_count
    mem = p.mem_size_mib
  }

  // Pre-fill the model with the provider's default when the provider changes.
  function onProviderChange() {
    const p = providers.find((x) => x.id === provider)
    model = p ? p.default_model : ''
  }

  async function create() {
    busy = true
    try {
      const body = JSON.stringify({ name: name.trim(), provider, model: model.trim(), vcpu_count: vcpu, mem_size_mib: mem, builtins: selectedBuiltins })
      await apiJSON('/config/profiles', { method: 'POST', body })
      toast(get(_)('profileModal.createdToast', { values: { name: name.trim() } }), 'ok')
      dispatch('created')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      busy = false
    }
  }

  function close() {
    dispatch('close')
  }
</script>

<div class="modal-backdrop" role="presentation" on:click|self={close} on:keydown={(e) => e.key === 'Escape' && close()}>
  <div class="modal">
    <h2>{$_('profileModal.title')}</h2>
    {#if providers.length === 0}
      <p class="muted">{$_('profileModal.noProviders')}</p>
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close}>{$_('common.cancel')}</button>
      </div>
    {:else}
      <div class="field">
        <label for="pm-name">{$_('profileModal.nameLabel')}</label>
        <input id="pm-name" bind:value={name} placeholder="my-profile" />
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('profileModal.nameHint')}</div>
      </div>
      <div class="field">
        <label for="pm-provider">{$_('profileModal.providerLabel')}</label>
        <select id="pm-provider" bind:value={provider} on:change={onProviderChange}>
          {#each providers as prov (prov.id)}
            <option value={prov.id}>{prov.label}</option>
          {/each}
        </select>
      </div>
      <div class="field">
        <label for="pm-model">{$_('profileModal.modelLabel')}</label>
        <ModelPicker id="pm-model" models={suggested} bind:value={model} />
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('profileModal.modelHint')}</div>
      </div>
      {#if presets.length}
        <div class="field">
          <span class="preset-label">{$_('profileModal.presetLabel')}</span>
          <div class="row" style="gap:8px; flex-wrap:wrap;">
            {#each presets as p (p.id)}
              <button type="button" class="ghost preset-chip" class:active={activePreset && activePreset.id === p.id}
                on:click={() => applyPreset(p)}>
                {p.label} <small>{p.vcpu_count} vCPU · {p.mem_size_mib} MiB</small>
              </button>
            {/each}
            {#if !activePreset}
              <span class="preset-custom">{$_('profileModal.presetCustom')}</span>
            {/if}
          </div>
        </div>
      {/if}
      <div class="row" style="gap:12px;">
        <div class="field" style="flex:1;">
          <label for="pm-vcpu">{$_('profileModal.vcpuLabel')}</label>
          <input id="pm-vcpu" type="number" min="1" max="8" bind:value={vcpu} />
        </div>
        <div class="field" style="flex:1;">
          <label for="pm-mem">{$_('profileModal.memLabel')}</label>
          <input id="pm-mem" type="number" min="256" max="16384" step="256" bind:value={mem} />
        </div>
      </div>
      <div class="muted" style="margin-top:-6px; margin-bottom:6px; font-size:12px;">{$_('profileModal.sizingHint')}</div>
      {#if builtins.length}
        <div class="field">
          <span class="preset-label">{$_('builtins.label')}</span>
          <BuiltinPicker options={builtins} bind:selected={selectedBuiltins} disabled={busy} />
          <div class="muted" style="margin-top:6px; font-size:12px;">{$_('builtins.tokenHint')}</div>
        </div>
      {/if}
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={create} disabled={busy || !name.trim() || !model.trim()}>
          {busy ? $_('profileModal.creating') : $_('profileModal.create')}
        </button>
      </div>
    {/if}
  </div>
</div>

<style>
  .preset-label { display: block; margin-bottom: 6px; color: var(--muted); }
  .preset-chip {
    padding: 6px 10px;
    font-size: 13px;
    display: inline-flex;
    gap: 6px;
    align-items: baseline;
  }
  .preset-chip.active {
    border-color: var(--accent);
    color: var(--accent);
  }
  .preset-chip small { color: var(--muted); font-size: 11px; }
  .preset-chip.active small { color: var(--accent); opacity: 0.85; }
  .preset-custom { font-size: 12px; color: var(--muted); align-self: center; }
</style>
