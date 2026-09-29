/*
 * Vim mode (monaco-vim, loaded on first use). Ex commands act on the tab of the editor they were typed in:
 * :w saves, :q closes the tab, :wq / :x save and close. The mode line and the ":" command line render into a status
 * element that the tab shows in its own status bar.
 */
import type * as Monaco from 'monaco-editor/editor'
import { closeTab } from '@/stores/workspace'
import { getController } from '../controllers'
import type { VimAdapter } from './vendor/monaco-vim.js'

type VimModule = typeof import('./vendor/monaco-vim.js')

/** editor → tab id, for ex commands. */
const editorTabs = new WeakMap<Monaco.editor.ICodeEditor, string>()
let vimModule: Promise<VimModule> | null = null

function loadVim(): Promise<VimModule> {
  if (!vimModule) {
    vimModule = import('./vendor/monaco-vim.js').then((m) => {
      const tabOf = (cm: VimAdapter) => editorTabs.get(cm.editor)
      m.VimMode.Vim.defineEx('write', 'w', (cm) => {
        const id = tabOf(cm)
        if (id) void getController(id)?.save()
      })
      m.VimMode.Vim.defineEx('quit', 'q', (cm) => {
        const id = tabOf(cm)
        if (id) void closeTab(id)
      })
      const writeQuit = (cm: VimAdapter) => {
        const id = tabOf(cm)
        if (!id) return
        void getController(id)
          ?.save()
          .then((ok) => ok && closeTab(id))
      }
      m.VimMode.Vim.defineEx('wq', 'wq', writeQuit)
      m.VimMode.Vim.defineEx('xit', 'x', writeQuit)
      return m
    })
    vimModule.catch(() => {
      vimModule = null
    })
  }
  return vimModule
}

/** Turn vim mode on for an editor; resolves the adapter (dispose() turns it off again). */
export async function attachVim(editor: Monaco.editor.IStandaloneCodeEditor, tabId: string, statusNode: HTMLElement): Promise<VimAdapter> {
  const m = await loadVim()
  editorTabs.set(editor, tabId)
  statusNode.replaceChildren()
  return m.initVimMode(editor, statusNode)
}
