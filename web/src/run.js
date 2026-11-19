import { mount } from 'svelte'
import './lib/theme.css'
import { installTooltips } from './lib/tooltip.js'
import Run from './Run.svelte'

installTooltips()
mount(Run, { target: document.getElementById('app') })
