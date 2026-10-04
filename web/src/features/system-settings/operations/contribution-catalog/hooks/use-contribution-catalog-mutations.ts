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

import {
  createContributionEntry,
  deleteContributionEntry,
  updateContributionEntry,
  updateContributionGlobalEnabled,
} from '../api'
import { contributionRejectionText } from '../constants'
import type {
  AdminContributionEntry,
  ContributionCatalogMutationResponse,
} from '../types'

function useInvalidateContributionCatalog() {
  const queryClient = useQueryClient()
  return () => {
    void queryClient.invalidateQueries({
      queryKey: ['contribution-admin-catalog'],
    })
    void queryClient.invalidateQueries({ queryKey: ['contribution-catalog'] })
  }
}

function reportRefusedMutation(response: ContributionCatalogMutationResponse) {
  const text = contributionRejectionText(response)
  if (text === '') {
    handleServerError(response)
    return
  }
  toast.error(i18next.t(text))
}

export function useCreateContributionEntry() {
  const invalidate = useInvalidateContributionCatalog()
  return useMutation({
    mutationFn: (entry: Omit<AdminContributionEntry, 'id'>) =>
      createContributionEntry(entry),
    onSuccess: (res) => {
      if (res.success) {
        toast.success(i18next.t('Contribution entry created successfully'))
        invalidate()
        return
      }
      reportRefusedMutation(res)
    },
    onError: (error: Error) => {
      handleServerError(error, i18next.t('Failed to create contribution entry'))
    },
  })
}

export function useUpdateContributionEntry() {
  const invalidate = useInvalidateContributionCatalog()
  return useMutation({
    mutationFn: (entry: AdminContributionEntry) => updateContributionEntry(entry),
    onSuccess: (res) => {
      if (res.success) {
        toast.success(i18next.t('Contribution entry updated successfully'))
        invalidate()
        return
      }
      reportRefusedMutation(res)
    },
    onError: (error: Error) => {
      handleServerError(error, i18next.t('Failed to update contribution entry'))
    },
  })
}

export function useDeleteContributionEntry() {
  const invalidate = useInvalidateContributionCatalog()
  return useMutation({
    mutationFn: (id: number) => deleteContributionEntry(id),
    onSuccess: (res) => {
      if (res.success) {
        toast.success(i18next.t('Contribution entry deleted successfully'))
        invalidate()
        return
      }
      reportRefusedMutation(res)
    },
    onError: (error: Error) => {
      handleServerError(error, i18next.t('Failed to delete contribution entry'))
    },
  })
}

export function useUpdateContributionGlobal() {
  const invalidate = useInvalidateContributionCatalog()
  return useMutation({
    mutationFn: (enabled: boolean) => updateContributionGlobalEnabled(enabled),
    onSuccess: (res) => {
      if (res.success) {
        invalidate()
        return
      }
      reportRefusedMutation(res)
    },
    onError: (error: Error) => {
      handleServerError(error, i18next.t('Failed to update setting'))
    },
  })
}
