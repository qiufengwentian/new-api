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
import i18next from 'i18next'

// Rejection codes returned by controller/contribution.go for POST
// /api/contribution/submit. The values are i18n keys, so they must be rendered
// through a translation function, never shown raw.
export const CONTRIBUTION_SUBMIT_REJECTION_MESSAGES: Record<string, string> = {
  contribution_consent_required: 'You must accept the statement before submitting',
  contribution_global_disabled: 'Contributing upstream keys is currently closed.',
  contribution_channel_type_unknown: 'This upstream is not open for contribution.',
  contribution_key_invalid:
    'This upstream key failed validation. Check the key and that it still has quota.',
  contribution_fingerprint_taken:
    'This upstream key has already been contributed.',
  contribution_key_already_pooled:
    'This upstream key is already in the shared pool.',
  contribution_pooling_failed:
    'The upstream key could not be added to the shared pool. Please contact your administrator.',
}

// The cooldown refusal carries how long the user has to wait, so it is rendered
// apart from the plain code lookup.
export const CONTRIBUTION_RATE_LIMITED_KEY =
  'Too many failed validation attempts. Please wait {{seconds}} seconds before trying again.'

/**
 * Resolves the copy for a refused submission: localized text for a known
 * rejection code, otherwise on the readable server message.
 */
export function contributionSubmitRejectionText(response: {
  code?: string
  message?: string
  retry_after_seconds?: number
}): string {
  if (response.code === 'contribution_rate_limited') {
    return i18next.t(CONTRIBUTION_RATE_LIMITED_KEY, {
      seconds: response.retry_after_seconds ?? 0,
    })
  }
  if (response.code) {
    const key = CONTRIBUTION_SUBMIT_REJECTION_MESSAGES[response.code]
    if (key) {
      return i18next.t(key)
    }
  }
  return response.message ?? ''
}
