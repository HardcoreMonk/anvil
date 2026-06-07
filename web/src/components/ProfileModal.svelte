<script>
  import { createEventDispatcher } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  // Available providers only: [{ id, label, default_model, suggested_models }].
  export let providers = []

  const dispatch = createEventDispatcher()

  let name = ''
  let provider = providers.length ? providers[0].id : ''
  let model = providers.length ? providers[0].default_model : ''
  let vcpu = 2
  let mem = 2048
  let busy = false

  // Suggested models track the selected provider.
  $: suggested = (providers.find((p) => p.id === provider) || {}).suggested_models || []

  // Pre-fill the model with the provider's default when the provider changes.
  function onProviderChange() {
    const p = providers.find((x) => x.id === provider)
    model = p ? p.default_model : ''
  }

  async function create() {
    busy = true
    try {
      const body = JSON.stringify({ name: name.trim(), provider, model: model.trim(), vcpu_count: vcpu, mem_size_mib: mem })
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
        <input id="pm-model" list="pm-models" bind:value={model} />
        <datalist id="pm-models">
          {#each suggested as m}<option value={m}></option>{/each}
        </datalist>
        <div class="muted" style="margin-top:6px; font-size:12px;">{$_('profileModal.modelHint')}</div>
      </div>
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
      <div class="row between" style="margin-top:18px;">
        <button class="ghost" on:click={close} disabled={busy}>{$_('common.cancel')}</button>
        <button on:click={create} disabled={busy || !name.trim() || !model.trim()}>
          {busy ? $_('profileModal.creating') : $_('profileModal.create')}
        </button>
      </div>
    {/if}
  </div>
</div>
