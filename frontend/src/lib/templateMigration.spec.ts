import { describe, it, expect } from 'vitest'
import { defaultSelectedIds, describeFailures, toggleId, type MigrationPreviewItem } from './templateMigration'

const item = (overrides: Partial<MigrationPreviewItem> & { id: string }): MigrationPreviewItem => ({
  name: `tpl_${overrides.id}`,
  language: 'en',
  category: 'UTILITY',
  status: 'APPROVED',
  quality_rating: 'GREEN',
  eligible: true,
  exists_on_destination: false,
  ...overrides
})

describe('defaultSelectedIds', () => {
  it('selects eligible templates that do not already exist on the destination', () => {
    const items = [
      item({ id: 'a' }),
      item({ id: 'b', eligible: false, reason: 'Only APPROVED templates can be migrated' }),
      item({ id: 'c', exists_on_destination: true }),
      item({ id: 'd' })
    ]
    expect(defaultSelectedIds(items)).toEqual(['a', 'd'])
  })

  it('returns an empty list when nothing is eligible', () => {
    expect(defaultSelectedIds([item({ id: 'a', eligible: false })])).toEqual([])
    expect(defaultSelectedIds([])).toEqual([])
  })
})

describe('toggleId', () => {
  it('adds an id once and removes it without mutating the input', () => {
    const start = ['a']
    const added = toggleId(start, 'b', true)
    expect(added).toEqual(['a', 'b'])
    expect(toggleId(added, 'b', true)).toEqual(['a', 'b'])
    expect(toggleId(added, 'a', false)).toEqual(['b'])
    expect(start).toEqual(['a'])
  })
})

describe('describeFailures', () => {
  it('resolves failed ids to template names and sorts by name', () => {
    const items = [item({ id: '1', name: 'zeta' }), item({ id: '2', name: 'alpha', language: 'hi' })]
    const failures = describeFailures(
      {
        migrated_count: 0,
        failed_count: 3,
        migrated_templates: [],
        failed_templates: { '1': 'Incorrect category', '2': 'Formatting error', '9': 'Unknown template' },
        synced_count: 0
      },
      items
    )
    expect(failures).toEqual([
      { id: '9', name: '9', reason: 'Unknown template' },
      { id: '2', name: 'alpha (hi)', reason: 'Formatting error' },
      { id: '1', name: 'zeta (en)', reason: 'Incorrect category' }
    ])
  })

  it('handles a missing failed_templates map', () => {
    const result = { migrated_count: 1, failed_count: 0, migrated_templates: ['x'], synced_count: 1 } as any
    expect(describeFailures(result, [])).toEqual([])
  })
})
