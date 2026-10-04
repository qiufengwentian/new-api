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

import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { formatQuota, formatTimestamp } from '@/lib/format'

import { contributionSubscriptionStatusBadge } from '../constants'
import type { ContributionSubmitData } from '../types'

type ContributionResultProps = {
  result: ContributionSubmitData
}

/**
 * Confirms an accepted contribution. The key is only ever shown masked, and the
 * reward reports exactly what was granted: the channel type it was earned for,
 * the plan, the quota consumed and the expiry. A redundant submission renders no
 * reward, because the server grants none.
 */
export function ContributionResult(props: ContributionResultProps) {
  const { t } = useTranslation()
  const contribution = props.result.contribution
  const reward = props.result.reward
  const rewardStatus = contributionSubscriptionStatusBadge(reward?.status ?? '')

  return (
    <Alert>
      <AlertTitle>{t('Contribution submitted')}</AlertTitle>
      <AlertDescription className='space-y-2 text-sm'>
        <p>
          {t('Key mask')}: {contribution.key_mask}
        </p>
        {props.result.redundant ? (
          <p>
            {t(
              'This upstream already grants you a reward, so the new key was accepted as redundant.'
            )}
          </p>
        ) : null}
        {reward ? (
          <div className='space-y-1'>
            <p className='font-medium'>{t('Reward')}</p>
            <p>
              {t('Channel Type')}:{' '}
              {contribution.channel_type_name ||
                `#${contribution.channel_type}`}
            </p>
            <p>
              {t('Plan')}: {reward.plan_title} (
              {formatQuota(reward.amount_used)} /{' '}
              {formatQuota(reward.amount_total)})
            </p>
            <p>
              {t('Expires at')}: {formatTimestamp(reward.end_time)}
            </p>
            <StatusBadge
              label={t(rewardStatus.labelKey)}
              variant={rewardStatus.variant}
              copyable={false}
            />
          </div>
        ) : null}
      </AlertDescription>
    </Alert>
  )
}
