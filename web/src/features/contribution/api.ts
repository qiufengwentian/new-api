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
import { api } from '@/lib/api'

import type {
  ContributionCatalogResponse,
  ContributionSubmitResponse,
  ContributionSubmitValues,
} from './types'

/**
 * Reads the contributable upstreams open to the signed-in user together with
 * the consent statement the backend owns.
 */
export async function getContributionCatalog(): Promise<ContributionCatalogResponse> {
  const res = await api.get('/api/contribution/catalog')
  return res.data
}

/**
 * Submits one upstream key. The backend re-validates the consent flag, so the
 * form's checkbox is only the user-facing gate.
 */
export async function submitContribution(
  values: ContributionSubmitValues
): Promise<ContributionSubmitResponse> {
  const res = await api.post('/api/contribution/submit', values)
  return res.data
}
