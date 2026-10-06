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
import { Plus } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { StaticRowActions } from '@/components/data-table/static/static-row-actions'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'

import {
  useDeleteContributionEntry,
  useUpdateContributionEntry,
} from '../hooks/use-contribution-catalog-mutations'
import type { AdminContributionEntry } from '../types'

type CatalogTableProps = {
  entries: AdminContributionEntry[]
  onCreate: () => void
  onEdit: (entry: AdminContributionEntry) => void
}

export function CatalogTable(props: CatalogTableProps) {
  const { t } = useTranslation()
  const deleteEntry = useDeleteContributionEntry()
  const updateEntry = useUpdateContributionEntry()
  const [deleteTarget, setDeleteTarget] =
    useState<AdminContributionEntry | null>(null)

  const handleDelete = async () => {
    if (deleteTarget === null) {
      return
    }
    await deleteEntry.mutateAsync(deleteTarget.id)
    setDeleteTarget(null)
  }

  return (
    <>
      <div className='flex items-center justify-between gap-3'>
        <p className='text-muted-foreground text-sm'>
          {t('Manage which upstream providers users can contribute keys for')}
        </p>
        <Button size='sm' onClick={props.onCreate}>
          <Plus className='mr-1.5 h-4 w-4' />
          {t('Add upstream')}
        </Button>
      </div>

      <StaticDataTable
        data={props.entries}
        getRowKey={(entry) => entry.id}
        emptyClassName='text-sm'
        emptyContent={t('No upstream is open for contribution right now.')}
        columns={[
          {
            id: 'id',
            header: t('ID'),
            cell: (entry) => entry.id,
          },
          {
            id: 'name',
            header: t('Name'),
            cellClassName: 'font-medium',
            cell: (entry) => entry.name,
          },
          {
            id: 'register-url',
            header: t('Register URL'),
            cellClassName: 'text-muted-foreground max-w-[220px] truncate',
            cell: (entry) => entry.register_url,
          },
          {
            id: 'host-channel',
            header: t('Host Channel'),
            cell: (entry) => entry.host_channel_id,
          },
          {
            id: 'plan',
            header: t('Subscription Plan'),
            cell: (entry) => entry.plan_id,
          },
          {
            id: 'status',
            header: t('Status'),
            cell: (entry) => (
              <div className='flex items-center gap-2'>
                <StatusBadge
                  label={entry.enabled ? t('Enabled') : t('Disabled')}
                  variant={entry.enabled ? 'success' : 'neutral'}
                  copyable={false}
                />
                <Switch
                  size='sm'
                  checked={entry.enabled}
                  disabled={updateEntry.isPending}
                  aria-label={t('Enabled')}
                  onCheckedChange={(value) =>
                    updateEntry.mutate({ ...entry, enabled: value === true })
                  }
                />
              </div>
            ),
          },
          {
            id: 'actions',
            header: t('Actions'),
            className: 'text-right',
            cellClassName: 'text-right',
            cell: (entry) => (
              <StaticRowActions
                editLabel={t('Edit')}
                deleteLabel={t('Delete')}
                menuLabel={t('Open menu')}
                onEdit={() => props.onEdit(entry)}
                onDelete={() => setDeleteTarget(entry)}
              />
            ),
          },
        ]}
      />

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={t('Delete upstream')}
        desc={t(
          'Are you sure you want to delete this contributable upstream? Users will no longer be able to contribute keys for it.'
        )}
        confirmText={t('Delete')}
        destructive
        handleConfirm={handleDelete}
        isLoading={deleteEntry.isPending}
      />
    </>
  )
}
