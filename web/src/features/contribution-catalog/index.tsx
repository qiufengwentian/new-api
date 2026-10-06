/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { SectionPageLayout } from '@/components/layout'
import { Switch } from '@/components/ui/switch'

import { CatalogEntryDialog } from './components/catalog-entry-dialog'
import { CatalogTable } from './components/catalog-table'
import { useAdminContributionCatalog } from './hooks/use-contribution-catalog'
import { useUpdateContributionGlobal } from './hooks/use-contribution-catalog-mutations'
import type { AdminContributionEntry } from './types'

export function ContributionCatalogPage() {
  const { t } = useTranslation()
  const catalogQuery = useAdminContributionCatalog()
  const updateGlobal = useUpdateContributionGlobal()
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingEntry, setEditingEntry] =
    useState<AdminContributionEntry | null>(null)

  const handleCreate = () => {
    setEditingEntry(null)
    setDialogOpen(true)
  }

  const handleEdit = (entry: AdminContributionEntry) => {
    setEditingEntry(entry)
    setDialogOpen(true)
  }

  const handleDialogChange = (open: boolean) => {
    setDialogOpen(open)
    if (!open) {
      setEditingEntry(null)
    }
  }

  const globalSwitch = (
    <label className='flex min-w-0 flex-row items-center justify-between gap-4 py-2.5'>
      <span className='min-w-0 space-y-0.5'>
        <span className='text-sm font-medium'>{t('Open for contribution')}</span>
        <span className='text-muted-foreground block text-xs'>
          {t('While off, no upstream is offered and the user page stays empty.')}
        </span>
      </span>
      <Switch
        checked={catalogQuery.data?.enabled ?? false}
        onCheckedChange={(value) => updateGlobal.mutate(value === true)}
        disabled={updateGlobal.isPending || !catalogQuery.data}
        aria-label={t('Open for contribution')}
      />
    </label>
  )

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Contribution Catalog')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          {globalSwitch}

          {catalogQuery.isLoading ? <LoadingState /> : null}
          {catalogQuery.isError ? (
            <ErrorState
              title={t('Failed to load')}
              onRetry={() => void catalogQuery.refetch()}
            />
          ) : null}
          {catalogQuery.data ? (
            <CatalogTable
              entries={catalogQuery.data.entries}
              onCreate={handleCreate}
              onEdit={handleEdit}
            />
          ) : null}
        </div>

        <CatalogEntryDialog
          open={dialogOpen}
          onOpenChange={handleDialogChange}
          entry={editingEntry}
        />
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
