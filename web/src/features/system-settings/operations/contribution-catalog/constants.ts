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
// Rejection codes returned by controller/contribution.go, mapped to translation
// keys so a refused enable is explained in every supported locale.
export const CONTRIBUTION_REJECTION_MESSAGES: Record<string, string> = {
  contribution_invalid_entry: 'Invalid contribution entry',
  contribution_entry_not_found: 'Contribution entry not found',
  contribution_host_channel_not_found: 'Host channel not found',
  contribution_channel_not_multi_key: 'Host channel must be a multi-key channel',
  contribution_plan_not_found: 'Subscription plan not found',
  contribution_channel_type_taken:
    'Another enabled entry already uses this channel type',
}

/**
 * Resolves the text to show for a refused admin mutation: a localized key for a
 * known rejection code, otherwise the readable server message.
 */
export function contributionRejectionText(response: {
  code?: string
  message?: string
}): string {
  if (response.code) {
    const key = CONTRIBUTION_REJECTION_MESSAGES[response.code]
    if (key) {
      return key
    }
  }
  return response.message ?? ''
}
