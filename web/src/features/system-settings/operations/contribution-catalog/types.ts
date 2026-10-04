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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import type { ContributionCatalogEntry } from '@/features/contribution/types'

/** An admin catalog entry adds the fields only the admin API exposes. */
export interface AdminContributionEntry extends ContributionCatalogEntry {
  id: number
  enabled: boolean
  host_channel_id: number
  plan_id: number
}

export interface ContributionCatalogData {
  enabled: boolean
  entries: AdminContributionEntry[]
}

export interface ContributionCatalogMutationResponse {
  success: boolean
  message?: string
  code?: string
  data?: {
    entry?: AdminContributionEntry
    entries?: AdminContributionEntry[]
    enabled?: boolean
  }
}

export function getContributionEntryFormSchema(t: TFunction) {
  return z.object({
    channel_type: z.number().int().positive(t('Channel type is required')),
    name: z.string().trim().min(1, t('Name is required')),
    register_url: z.string().trim(),
    key_placeholder: z.string().trim(),
    host_channel_id: z.number().int().positive(t('Host channel is required')),
    plan_id: z.number().int().positive(t('Subscription plan is required')),
    enabled: z.boolean(),
  })
}

export type ContributionEntryFormValues = z.infer<
  ReturnType<typeof getContributionEntryFormSchema>
>
