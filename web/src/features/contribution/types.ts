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
  channel_type: number
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

export function getContributionSubmitSchema(t: TFunction) {
  return z.object({
    channel_type: z.number().int().positive(),
    key: z.string().trim().min(1, t('Upstream API key is required')),
    agreed: z
      .boolean()
      .refine((value) => value, t('You must accept the statement before submitting')),
  })
}

export type ContributionSubmitValues = z.infer<
  ReturnType<typeof getContributionSubmitSchema>
>
