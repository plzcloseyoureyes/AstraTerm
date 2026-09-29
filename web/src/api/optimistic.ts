import type { QueryClient, QueryKey } from '@tanstack/react-query'

export interface OptimisticSnapshot {
  snapshots: [QueryKey, unknown][]
}

/**
 * Builds onMutate/onError/onSettled callbacks for an optimistic list update:
 * cancel in-flight fetches, snapshot, apply `update` to the cached value, roll back on error, refetch when settled.
 */
export function optimisticList<TItem, TVars>(
  qc: QueryClient,
  key: QueryKey,
  update: (items: TItem[], vars: TVars) => TItem[],
  extraInvalidate: QueryKey[] = [],
) {
  return {
    onMutate: async (vars: TVars): Promise<OptimisticSnapshot> => {
      await qc.cancelQueries({ queryKey: key, exact: true })
      const prev = qc.getQueryData<TItem[]>(key)
      if (prev) qc.setQueryData<TItem[]>(key, update(prev, vars))
      return { snapshots: [[key, prev]] }
    },
    onError: (_err: unknown, _vars: TVars, ctx: OptimisticSnapshot | undefined) => {
      for (const [k, v] of ctx?.snapshots ?? []) qc.setQueryData(k, v)
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: key })
      for (const k of extraInvalidate) qc.invalidateQueries({ queryKey: k })
    },
  }
}

/** Replace (by id) or append an item in a cached list. */
export function upsertById<T extends { id: string }>(items: T[] | undefined, item: T): T[] {
  if (!items) return [item]
  const i = items.findIndex((x) => x.id === item.id)
  if (i < 0) return [...items, item]
  const next = items.slice()
  next[i] = item
  return next
}

/** Remove items by id from a cached list. */
export function removeByIds<T extends { id: string }>(items: T[] | undefined, ids: string[] | string): T[] {
  if (!items) return []
  const set = new Set(Array.isArray(ids) ? ids : [ids])
  return items.filter((x) => !set.has(x.id))
}
