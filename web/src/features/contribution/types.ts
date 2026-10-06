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

/** One upstream an administrator opened for contribution. */
export interface ContributionCatalogEntry {
  /** The 8-character upstream code that identifies this entry user-side. */
  entry_id: string
  name: string
  register_url: string
  key_placeholder: string
}

/** What the user catalog endpoint returns; `agreement` is versioned by the backend. */
export interface ContributionCatalog {
  enabled: boolean
  entries: ContributionCatalogEntry[]
  agreement: string
}

export interface ContributionCatalogResponse {
  success: boolean
  message?: string
  data?: ContributionCatalog
}

/** The subscription instance a contribution produced; null when it granted none. */
export interface ContributionReward {
  /** The entry code the reward was earned for; present when one was granted. */
  entry_code: string
  subscription_id: number
  plan_title: string
  amount_total: number
  amount_used: number
  end_time: number
  /** Subscription status: active / cancelled / expired. */
  status: string
}

/** The reward instance a contribution points at; null when it granted none. */
export interface ContributionSubscription {
  plan_title: string
  amount_total: number
  amount_used: number
  end_time: number
  /** Subscription status: active / cancelled / expired. */
  status: string
}

/** One contribution record, as every contribution response returns it. */
export interface ContributionSummary {
  id: number
  /** The 8-character upstream code of the catalog entry the key was contributed to. */
  entry_code: string
  /** The provider channel-type number the entry's host channel binds to; kept for
   *  display fallback only, never used as the user-facing identifier. */
  channel_type: number
  channel_type_name: string
  status: string
  reason: string
  reason_time: number
  key_mask: string
  subscription_id: number
  /** The reward instance's status, or an empty string when the record granted none. */
  subscription_status: string
  /** The reward instance itself, or null when the record granted none or it is gone. */
  subscription: ContributionSubscription | null
  reward_granted: boolean
  created_time: number
}

/** The account's contribution tally: distinct upstream codes that still reward it. */
export interface ContributionAccountSummary {
  entry_code_count: number
}

/** The signed-in user's own contributions plus the account summary. */
export interface ContributionMineData {
  items: ContributionSummary[]
  summary: ContributionAccountSummary
}

export interface ContributionMineResponse {
  success: boolean
  message?: string
  data?: ContributionMineData
}

export interface ContributionSubmitData {
  contribution: ContributionSummary
  reward: ContributionReward | null
  redundant: boolean
}

export interface ContributionSubmitResponse {
  success: boolean
  message?: string
  code?: string
  retry_after_seconds?: number
  data?: ContributionSubmitData
}

// The upstream code alphabet shared with the backend: the 23 uppercase letters
// left of the RFC 4648 Base32 alphabet after the confusable characters I, O and L
// are removed (0 and 1 are not Base32 letters and therefore already absent). Every
// symbol reads back without guessing whether a character is a digit or a letter.
const ENTRY_CODE_ALPHABET = 'ABCDEFGHJKMNPQRSTUVWXYZ'

/** The exact length of an auto-assigned upstream code. */
const ENTRY_CODE_LENGTH = 8

/** True when the value is a well-formed upstream code. */
function isEntryCode(value: string): boolean {
  return (
    value.length === ENTRY_CODE_LENGTH &&
    [...value].every((char) => ENTRY_CODE_ALPHABET.includes(char))
  )
}

export function getContributionSubmitSchema(t: TFunction) {
  return z.object({
    entry_id: z
      .string()
      .refine(isEntryCode, t('Select a valid upstream from the list')),
    key: z.string().trim().min(1, t('Upstream API key is required')),
    agreed: z
      .boolean()
      .refine((value) => value, t('You must accept the statement before submitting')),
  })
}

export type ContributionSubmitValues = z.infer<
  ReturnType<typeof getContributionSubmitSchema>
>

/** What the revoke endpoint returns: the record's fresh, terminal summary. */
export interface ContributionRevokeData {
  contribution: ContributionSummary
}

export interface ContributionRevokeResponse {
  success: boolean
  message?: string
  code?: string
  data?: ContributionRevokeData
}
