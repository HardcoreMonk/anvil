import './app.css'
import './lib/i18n.js'
import { mount } from 'svelte'
import App from './App.svelte'

// Svelte 5 replaces the `new Component({ target })` class API with `mount()`.
// The app renders client-side only (no SSR), so mounting into #app is all that
// changes; every component keeps its Svelte 4 (legacy-mode) authoring syntax.
const app = mount(App, { target: document.getElementById('app') })

export default app
