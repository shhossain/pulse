import { mount } from 'svelte'
import './lib/theme.css'
import { installTooltips } from './lib/tooltip.js'
import Popover from './Popover.svelte'

installTooltips()
mount(Popover, { target: document.getElementById('app') })
