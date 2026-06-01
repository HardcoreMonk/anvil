<script>
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import { _ } from 'svelte-i18n'
  import { apiJSON } from '../lib/api.js'
  import { toast } from '../lib/store.js'

  let profiles = [] // [{ name, provider, model }]
  let loading = true
  let savingName = null

  async function load() {
    try {
      const data = await apiJSON('/config/profiles')
      profiles = Array.isArray(data) ? data : []
    } catch (e) {
      if (e.message !== 'unauthorized') toast(e.message, 'error')
    } finally {
      loading = false
    }
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

  onMount(load)
</script>

<div class="row between" style="margin-bottom:16px;">
  <h1>{$_('settings.title')}</h1>
</div>

<div class="panel">
  <p class="muted" style="margin-bottom:16px;">{$_('settings.note')}</p>
  {#if loading}
    <span class="muted">{$_('common.loading')}</span>
  {:else if profiles.length === 0}
    <span class="muted">{$_('settings.empty')}</span>
  {:else}
    <table>
      <thead>
        <tr><th>{$_('settings.colProfile')}</th><th>{$_('settings.colProvider')}</th><th>{$_('settings.colModel')}</th><th></th></tr>
      </thead>
      <tbody>
        {#each profiles as p (p.name)}
          <tr>
            <td class="mono">{p.name}</td>
            <td><input bind:value={p.provider} /></td>
            <td><input bind:value={p.model} /></td>
            <td>
              <button class="ghost" on:click={() => save(p)} disabled={savingName === p.name}>
                {savingName === p.name ? $_('settings.saving') : $_('settings.save')}
              </button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
