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
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'

import { useRevokeContribution } from '../hooks/use-revoke-contribution'
import type { ContributionSummary } from '../types'

type RevokeContributionDialogProps = {
  /**
   * The contribution the confirmation is about; null keeps the dialog closed.
   * The owned list renders this per row and only has to pass the row.
   */
  contribution: ContributionSummary | null
  onClose: () => void
}

/**
 * The confirmation of an irreversible withdrawal, built on the shared
 * ConfirmDialog. Nothing is sent until the user confirms, and the dialog closes
 * once the request settles so the row can render whatever the backend answered.
 */
export function RevokeContributionDialog(props: RevokeContributionDialogProps) {
  const { t } = useTranslation()
  const revokeContribution = useRevokeContribution()

  return (
    <ConfirmDialog
      open={props.contribution !== null}
      onOpenChange={(open) => {
        if (!open) {
          props.onClose()
        }
      }}
      title={t('Revoke contribution')}
      desc={t(
        'Revoking disables this key in the shared channel and cancels its reward subscription immediately. This cannot be undone; you can contribute the key again later.'
      )}
      confirmText={t('Revoke')}
      destructive
      isLoading={revokeContribution.isPending}
      handleConfirm={() => {
        if (!props.contribution) {
          return
        }
        revokeContribution.mutate(props.contribution.id, {
          onSettled: () => props.onClose(),
        })
      }}
    />
  )
}
