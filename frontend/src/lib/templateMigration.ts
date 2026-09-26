// Pure helpers for the "Migrate from another WABA" dialog. Kept free of Vue so
// the selection and summary rules can be unit tested.

export interface MigrationPreviewItem {
  id: string
  name: string
  language: string
  category: string
  status: string
  quality_rating: string
  eligible: boolean
  reason?: string
  exists_on_destination: boolean
}

export interface MigrationResult {
  migrated_count: number
  failed_count: number
  migrated_templates: string[]
  failed_templates: Record<string, string>
  synced_count: number
  sync_error?: string
}

export interface MigrationFailure {
  id: string
  name: string
  reason: string
}

/**
 * Templates that should be ticked when a preview loads: eligible per Meta's
 * rules and not already present (same name + language) on the destination.
 * Duplicates stay selectable because Meta's docs do not say how it treats
 * them, but they should be an explicit choice.
 */
export function defaultSelectedIds(items: MigrationPreviewItem[]): string[] {
  return items.filter(item => item.eligible && !item.exists_on_destination).map(item => item.id)
}

/**
 * Toggle an id in a selection without mutating the input.
 */
export function toggleId(selected: string[], id: string, checked: boolean): string[] {
  if (checked) {
    return selected.includes(id) ? selected : [...selected, id]
  }
  return selected.filter(existing => existing !== id)
}

/**
 * Meta reports failures keyed by source template id. Resolve them to names
 * using the preview list so the summary reads "order_update (en): reason"
 * instead of a bare id. Unknown ids fall back to the id itself.
 */
export function describeFailures(result: MigrationResult, items: MigrationPreviewItem[]): MigrationFailure[] {
  const byId = new Map(items.map(item => [item.id, item]))
  return Object.entries(result.failed_templates || {})
    .map(([id, reason]) => {
      const item = byId.get(id)
      return {
        id,
        name: item ? `${item.name} (${item.language})` : id,
        reason
      }
    })
    .sort((a, b) => a.name.localeCompare(b.name))
}
