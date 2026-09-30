/*
 * Personal UI preferences of the AI assistant (settings section `ai`). The provider configuration is NOT stored
 * here: it lives server-side (settings key `aiProvider` + vault-sealed keys) and is edited through /api/ai/config.
 */
import { defineSettings } from '@/stores/settings'

export interface AiUiSettings {
  /** Typing `# <what you want>` + Enter at a shell prompt opens the command bar with a suggestion. */
  hashTrigger: boolean
  /** Offer "Explain" next to failed commands (OSC 133 exit codes, or recognisable error output). */
  offerOnError: boolean
  /** Also recognise errors from the output when the shell reports no exit codes. */
  errorHeuristics: boolean
  /** Attach the active terminal's recent output to new chats automatically. */
  autoAttachTerminal: boolean
  /** How many terminal lines a terminal context chip includes. */
  terminalLines: number
  /** Model chosen in the chat picker ('' = the configured default). */
  chatModel: string
}

const AI_UI_DEFAULTS: AiUiSettings = {
  hashTrigger: true,
  offerOnError: true,
  errorHeuristics: true,
  autoAttachTerminal: true,
  terminalLines: 100,
  chatModel: '',
}

export const aiSettings = defineSettings<AiUiSettings>('ai', AI_UI_DEFAULTS)
