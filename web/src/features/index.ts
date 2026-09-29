/*
 * Feature registry entry point (SPEC §10.2): importing a feature runs its self-registration (tab kinds, commands,
 * panels, ...). Every feature folder exposes index.ts; keep this list in sync with docs/SPEC.md §10.2.
 * Order does not matter — registries are reactive.
 */
import './home'
import './terminal'
import './sessions'
import './files'
import './editor'
import './tunnels'
import './keys'
import './vnc'
import './rdp'
import './monitor'
import './tools'
import './servers'
import './automation'
import './recordings'
import './admin'
import './security'
import './importer'
import './ai'
import './settings'
import './webproxy'
import './protocols'
import './termtransfer'
