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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { Main } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { requireServerSuccess } from '@/lib/server-error-message'

import { getContributionCatalog } from './api'
import { ContributePanel } from './components/contribute-panel'
import { MyContributions } from './components/my-contributions'

export function Contribution() {
  const { t } = useTranslation()
  const catalogQuery = useQuery({
    queryKey: ['contribution-catalog'],
    queryFn: async () =>
      requireServerSuccess(await getContributionCatalog()).data,
  })

  return (
    <Main>
      <div className='min-h-0 flex-1 overflow-auto px-3 py-3 sm:px-4 sm:py-6'>
        <div className='mx-auto flex w-full max-w-3xl flex-col gap-4'>
          <header className='flex flex-col gap-1'>
            <h1 className='text-xl font-semibold'>
              {t('Contributed Upstream Keys')}
            </h1>
            <p className='text-muted-foreground text-sm'>
              {t(
                'Contribute your spare upstream quota and get a subscription reward.'
              )}
            </p>
          </header>

          {catalogQuery.isLoading ? <LoadingState /> : null}
          {catalogQuery.isError ? (
            <ErrorState
              title={t('Failed to load')}
              onRetry={() => void catalogQuery.refetch()}
            />
          ) : null}
          {catalogQuery.data ? (
            <ContributePanel catalog={catalogQuery.data} />
          ) : null}

          <MyContributions />
        </div>
      </div>
    </Main>
  )
}
