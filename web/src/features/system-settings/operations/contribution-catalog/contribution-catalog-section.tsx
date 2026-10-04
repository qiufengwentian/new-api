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

import { SettingsSection } from '../../components/settings-section'
import { SettingsSwitchField } from '../../components/settings-form-layout'
import { CatalogEntryDialog } from './components/catalog-entry-dialog'
import { CatalogTable } from './components/catalog-table'
import { useAdminContributionCatalog } from './hooks/use-contribution-catalog'
import { useUpdateContributionGlobal } from './hooks/use-contribution-catalog-mutations'
import type { AdminContributionEntry } from './types'

export function ContributionCatalogSection() {
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

  return (
    <SettingsSection title={t('Contributed Upstreams')}>
      <SettingsSwitchField
        controlId='contribution-global-switch'
        checked={catalogQuery.data?.enabled ?? false}
        onCheckedChange={(value) => updateGlobal.mutate(value === true)}
        label={t('Open for contribution')}
        description={t(
          'While off, no upstream is offered and the user page stays empty.'
        )}
        disabled={updateGlobal.isPending || !catalogQuery.data}
      />

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

      <CatalogEntryDialog
        open={dialogOpen}
        onOpenChange={handleDialogChange}
        entry={editingEntry}
      />
    </SettingsSection>
  )
}
