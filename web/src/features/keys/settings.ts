/*
 * Settings section "keys" (Settings → SSH keys & agent). The agent* keys are read by the backend (internal/keys
 * settings.go): the built-in agent's autostart, confirmations, key selection, auto-lock and key lifetime.
 */
import { defineSettings } from '@/stores/settings'
import type { GeneratedKeyType } from './types'

export const keysSettings = defineSettings('keys', {
  /** Generator defaults. */
  defaultType: 'ed25519' as GeneratedKeyType,
  defaultRsaBits: 4096 as 2048 | 3072 | 4096,
  defaultEcdsaBits: 256 as 256 | 384 | 521,
  /** PuTTY key format used by exports. */
  ppkVersion: 3 as 2 | 3,
  /** "Remember passphrase in the vault" default of generate / import. */
  rememberPassphrase: true,

  /** Start the built-in agent when AstraTerm starts (desktop mode). */
  agentAutostart: false,
  /** Ask before a local program uses a key through the agent socket. */
  agentConfirm: false,
  /** Ask before a remote host uses a key through agent forwarding. */
  agentForwardConfirm: false,
  /** Stored keys the agent (and agent forwarding) must not offer. */
  agentExclude: [] as string[],
  /** Lock the agent after that many idle minutes (0 = never). */
  agentAutoLockMin: 0,
  /** Forget unlocked keys after that many minutes (0 = until the agent stops). */
  agentKeyLifetimeMin: 0,
})
