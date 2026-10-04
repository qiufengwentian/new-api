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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import i18next from 'i18next'
import { toast } from 'sonner'

import { handleServerError } from '@/lib/handle-server-error'

import { revokeContribution } from '../api'
import {
  CONTRIBUTION_REVOKE_REJECTION_MESSAGES,
  contributionRejectionText,
} from '../constants'
import type { ContributionRevokeResponse } from '../types'

/**
 * Withdraws one of the signed-in user's contributions. The caller reads the
 * returned summary to render the row's new state, so a business refusal - a record
 * that is already over, a record that is not the caller's - is toasted here while
 * every outcome stays visible on the mutation.
 */
export function useRevokeContribution() {
  const queryClient = useQueryClient()
  return useMutation<ContributionRevokeResponse, Error, number>({
    mutationFn: (id) => revokeContribution(id),
    onSuccess: (response) => {
      if (response.success) {
        void queryClient.invalidateQueries({ queryKey: ['contribution-mine'] })
        void queryClient.invalidateQueries({ queryKey: ['contribution-catalog'] })
        toast.success(i18next.t('Contribution revoked'))
        return
      }
      const text = contributionRejectionText(
        response,
        CONTRIBUTION_REVOKE_REJECTION_MESSAGES
      )
      if (text === '') {
        handleServerError(response)
        return
      }
      toast.error(text)
    },
    onError: (error) => {
      handleServerError(error, i18next.t('Failed to revoke the contribution'))
    },
  })
}
