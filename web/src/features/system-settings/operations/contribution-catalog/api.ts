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
  AdminContributionEntry,
  ContributionCatalogData,
  ContributionCatalogMutationResponse,
} from './types'

interface AdminContributionCatalogResponse {
  success: boolean
  message?: string
  data?: ContributionCatalogData
}

export async function getAdminContributionCatalog(): Promise<AdminContributionCatalogResponse> {
  const res = await api.get('/api/contribution/admin/catalog')
  return res.data
}

export async function createContributionEntry(
  entry: Omit<AdminContributionEntry, 'id'>
): Promise<ContributionCatalogMutationResponse> {
  const res = await api.post('/api/contribution/admin/catalog', entry)
  return res.data
}

export async function updateContributionEntry(
  entry: AdminContributionEntry
): Promise<ContributionCatalogMutationResponse> {
  const res = await api.put('/api/contribution/admin/catalog', entry)
  return res.data
}

export async function deleteContributionEntry(
  id: number
): Promise<ContributionCatalogMutationResponse> {
  const res = await api.delete('/api/contribution/admin/catalog', {
    params: { id },
  })
  return res.data
}

export async function updateContributionGlobalEnabled(
  enabled: boolean
): Promise<ContributionCatalogMutationResponse> {
  const res = await api.put('/api/contribution/admin/global', { enabled })
  return res.data
}
