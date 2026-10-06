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

/**
 * An admin catalog entry as the admin API stores and returns it. It deliberately
 * does not extend the user-side ContributionCatalogEntry (which is keyed by the
 * user-facing code): the admin shape carries its own internal id, the durable
 * upstream code, and the binding to the host channel and reward plan.
 */
export interface AdminContributionEntry {
  id: number
  /** Durable user-facing upstream identity, auto-assigned by the backend. */
  code: string
  /** Provider channel type of the bound host channel (display only, not identity).
   * The admin form no longer picks one; the backend derives it from the host channel. */
  channel_type?: number
  name: string
  register_url: string
  key_placeholder: string
  enabled: boolean
  host_channel_id: number
  plan_id: number
  /**
   * Host channel name resolved server-side; empty when the bound channel no
   * longer exists, in which case the table falls back to a localized
   * deleted/missing marker.
   */
  host_channel_name?: string
  /** Reward plan title resolved server-side; empty when the bound plan is gone. */
  plan_title?: string
  /** How many contributed keys are currently active under this entry's code. */
  contributed_keys?: number
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