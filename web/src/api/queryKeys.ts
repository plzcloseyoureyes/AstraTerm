/*
 * Central react-query key factory. Every feature should build keys from here (or extend it in its own module with a
 * distinct first segment) so cache updates from the events socket and mutations stay consistent.
 */
export const queryKeys = {
  authState: ['auth', 'state'] as const,
  authTokens: ['auth', 'tokens'] as const,
  authSessions: ['auth', 'sessions'] as const,
  settings: ['settings'] as const,
  adminSettings: ['admin', 'settings'] as const,
  vaultStatus: ['vault', 'status'] as const,

  folders: ['folders'] as const,
  connections: ['connections'] as const,
  connection: (id: string) => ['connections', id] as const,
  identities: ['identities'] as const,

  sessions: ['sessions'] as const,
  sessionsAll: ['sessions', 'all'] as const,
  session: (id: string) => ['sessions', 'detail', id] as const,
  localShells: ['local', 'shells'] as const,
  serialPorts: ['serial', 'ports'] as const,

  keys: ['keys'] as const,
  knownHosts: ['known-hosts'] as const,
  tunnels: ['tunnels'] as const,
  snippets: ['snippets'] as const,
  macros: ['macros'] as const,
  transfers: ['transfers'] as const,
  recordings: ['recordings'] as const,
  servers: ['servers'] as const,
} as const
