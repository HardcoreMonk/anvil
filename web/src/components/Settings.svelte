<script>
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'
  import ProfileModal from './ProfileModal.svelte'

  let profiles = [] // [{ name, provider, model }]
  let providers = [] // [{ id, label, available, default_model, suggested_models }]
  let loading = true
  let savingName = null
  let deletingName = null
  let confirmName = null // profile whose Delete is awaiting in-row confirmation
  let showCreate = false

  // Only providers whose API key is configured may be selected.
  $: availableProviders = providers.filter((p) => p.available)

  async function load() {
    loading = true
    try {
      const [profileData, providerData] = await Promise.all([
        apiJSON('/config/profiles'),
        apiJSON('/config/providers'),
      ])
      profiles = Array.isArray(profileData) ? profileData : []
      providers = Array.isArray(providerData) ? providerData : []
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
  }

  // Suggested models for the provider currently chosen on a row.
  function modelsFor(providerId) {
    const p = providers.find((x) => x.id === providerId)
    return p ? p.suggested_models : []
  }

  async function save(p) {
    savingName = p.name
    try {
      await apiJSON('/config/profiles/' + encodeURIComponent(p.name), {
        method: 'PUT',
        body: JSON.stringify({ provider: p.provider, model: p.model }),
      })
      toast(get(_)('settings.saved', { values: { name: p.name } }), 'ok')
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      savingName = null
    }
  }

  async function remove(p) {
    deletingName = p.name
    try {
      await apiJSON('/config/profiles/' + encodeURIComponent(p.name), { method: 'DELETE' })
      toast(get(_)('settings.deletedToast', { values: { name: p.name } }), 'ok')
      confirmName = null
      await load()
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      deletingName = null
    }
  }

  onMount(load)
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('settings.title')}</h1>
  <button on:click={() => (showCreate = true)} disabled={loading || availableProviders.length === 0}>
    {$_('settings.createProfile')}
  </button>
</div>

<div class="panel">
  <p class="muted" style="margin-bottom:16px;">{$_('settings.note')}</p>
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if availableProviders.length === 0}
    <span class="muted">{$_('settings.noProviders')}</span>
  {:else}
    <table>
      <thead>
        <tr><th>{$_('settings.colProfile')}</th><th>{$_('settings.colProvider')}</th><th>{$_('settings.colModel')}</th><th></th></tr>
      </thead>
      <tbody>
        {#each profiles as p (p.name)}
          <tr>
            <td class="mono">{p.name}</td>
            <td>
              <select bind:value={p.provider}>
                {#if p.provider && !availableProviders.some((x) => x.id === p.provider)}
                  <!-- Current provider's key is gone — keep it visible (disabled) so the row isn't silently changed. -->
                  <option value={p.provider} disabled>{p.provider} ({$_('settings.providerUnavailable')})</option>
                {/if}
                {#each availableProviders as prov (prov.id)}
                  <option value={prov.id}>{prov.label}</option>
                {/each}
              </select>
            </td>
            <td>
              <input list={'models-' + p.name} bind:value={p.model} />
              <datalist id={'models-' + p.name}>
                {#each modelsFor(p.provider) as m}<option value={m}></option>{/each}
              </datalist>
            </td>
            <td>
              <div class="row" style="gap:6px;">
                <button class="ghost" on:click={() => save(p)} disabled={savingName === p.name}>
                  {savingName === p.name ? $_('settings.saving') : $_('settings.save')}
                </button>
                {#if p.name !== 'default'}
                  {#if confirmName === p.name}
                    <button class="danger" on:click={() => remove(p)} disabled={deletingName === p.name}>
                      {deletingName === p.name ? $_('settings.deleting') : $_('settings.confirmDeleteBtn')}
                    </button>
                    <button class="ghost" on:click={() => (confirmName = null)} disabled={deletingName === p.name}>
                      {$_('common.cancel')}
                    </button>
                  {:else}
                    <button class="danger" on:click={() => (confirmName = p.name)}>{$_('settings.delete')}</button>
                  {/if}
                {/if}
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

{#if showCreate}
  <ProfileModal providers={availableProviders}
    on:created={() => { showCreate = false; load() }}
    on:close={() => (showCreate = false)} />
{/if}
