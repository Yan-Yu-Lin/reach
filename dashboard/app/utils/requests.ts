import type { RequestRow } from '~/composables/useReachApi'

export type BoundValue = { value: string; bound: boolean }

/**
 * The name the machine will get. Newer servers return a top-level `slug` that provision
 * must match ("bound"). Older servers only have the requester's own metadata.slug, which
 * the target can still change at provision time.
 */
export function requestSlug(req: RequestRow): BoundValue {
  if (typeof req.slug === 'string' && req.slug) return { value: req.slug, bound: true }
  const claimed = req.metadata?.slug
  return { value: typeof claimed === 'string' ? claimed : '', bound: false }
}

export function requestTargetUser(req: RequestRow): BoundValue {
  if (typeof req.target_user === 'string' && req.target_user) return { value: req.target_user, bound: true }
  const claimed = req.metadata?.target_user
  return { value: typeof claimed === 'string' ? claimed : '', bound: false }
}

/** Reasons to look twice before approving. Shown on the card and in the confirm dialog. */
export function requestWarnings(req: RequestRow, existingSlugs: Set<string>): string[] {
  const out: string[] = []
  const slug = requestSlug(req)
  const user = requestTargetUser(req)
  if (!user.value || user.value === 'root') out.push('Logs in as root')
  if (slug.value && existingSlugs.has(slug.value)) out.push(`Name "${slug.value}" is already used by another machine`)
  if (!slug.value) out.push('Final ssh name is not known until setup runs')
  else if (!slug.bound) out.push('Name is self-reported and not bound by this server version')
  if (!req.observed_ip) out.push('Source IP is self-reported; the server did not confirm it')
  else {
    const claimed = req.metadata?.source_ip
    if (typeof claimed === 'string' && claimed && claimed !== req.observed_ip) {
      out.push('Claimed source IP differs from the one the server saw')
    }
  }
  return out
}
