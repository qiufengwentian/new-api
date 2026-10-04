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
import { KeyRound } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'

import {
  CONTRIBUTION_REASON_LABELS,
  CONTRIBUTION_STATUS_LABELS,
  CONTRIBUTION_STATUS_VARIANTS,
  contributionSubscriptionStatusBadge,
} from '../constants'
import { useMyContributions } from '../hooks/use-my-contributions'
import type { ContributionSummary } from '../types'
import { RevokeContributionDialog } from './revoke-contribution-dialog'

/**
 * The contributor's own list: what they brought in, whether it is still live, why
 * it ended, and what their reward is doing. Every record is rendered from the mask
 * the backend stores - the plaintext key is never part of any response - and only a
 * live record can be withdrawn, through the existing confirmation dialog.
 */
export function MyContributions() {
  const { t } = useTranslation()
  const query = useMyContributions()
  const [revokeTarget, setRevokeTarget] =
    useState<ContributionSummary | null>(null)
  const items = query.data?.items ?? []
  const channelTypeCount = query.data?.summary.channel_type_count ?? 0

  const columns: StaticDataTableColumn<ContributionSummary>[] = [
    {
      id: 'upstream',
      header: t('Upstream'),
      cellClassName: 'font-medium',
      cell: (contribution) =>
        contribution.channel_type_name || `#${contribution.channel_type}`,
    },
    {
      id: 'status',
      header: t('Status'),
      cell: (contribution) => (
        <StatusBadge
          label={t(
            CONTRIBUTION_STATUS_LABELS[contribution.status] ??
              contribution.status
          )}
          variant={CONTRIBUTION_STATUS_VARIANTS[contribution.status] ?? 'neutral'}
          copyable={false}
        />
      ),
    },
    {
      id: 'reason',
      header: t('Reason'),
      cell: (contribution) =>
        contribution.reason
          ? t(
              CONTRIBUTION_REASON_LABELS[contribution.reason] ??
                contribution.reason
            )
          : null,
    },
    {
      id: 'reward',
      header: t('Reward'),
      cell: (contribution) => {
        if (!contribution.subscription) {
          return <span className='text-muted-foreground'>{t('No reward')}</span>
        }
        const badge = contributionSubscriptionStatusBadge(
          contribution.subscription.status
        )
        return (
          <div className='flex flex-col items-start gap-1'>
            <span className='text-sm'>{contribution.subscription.plan_title}</span>
            <StatusBadge
              label={t(badge.labelKey)}
              variant={badge.variant}
              copyable={false}
            />
          </div>
        )
      },
    },
    {
      id: 'key_mask',
      header: t('Key mask'),
      cellClassName: 'font-mono',
      cell: (contribution) => contribution.key_mask,
    },
    {
      id: 'actions',
      header: t('Actions'),
      className: 'text-right',
      cellClassName: 'text-right',
      cell: (contribution) => {
        const revocable = contribution.status === 'active'
        return (
          <Button
            variant='destructive'
            size='sm'
            disabled={!revocable}
            title={
              revocable
                ? undefined
                : t('Only a live contribution can be revoked.')
            }
            onClick={() => setRevokeTarget(contribution)}
          >
            {t('Revoke')}
          </Button>
        )
      },
    },
  ]

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('My contributions')}</CardTitle>
        <CardDescription>
          {t('You have brought in {{types}} upstream channel types.', {
            types: channelTypeCount,
          })}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {query.isLoading ? <LoadingState /> : null}
        {query.isError ? (
          <ErrorState
            title={t('Failed to load')}
            onRetry={() => void query.refetch()}
          />
        ) : null}
        {query.data && items.length === 0 ? (
          <EmptyState
            icon={KeyRound}
            bordered
            title={t('No contributions yet')}
            description={t('Contribute an upstream key and it will show up here.')}
          />
        ) : null}
        {query.data && items.length > 0 ? (
          <StaticDataTable
            data={items}
            getRowKey={(contribution) => contribution.id}
            columns={columns}
          />
        ) : null}
      </CardContent>
      <RevokeContributionDialog
        contribution={revokeTarget}
        onClose={() => setRevokeTarget(null)}
      />
    </Card>
  )
}
