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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ContributionCatalogPage } from '../index'
import type { AdminContributionEntry, ContributionCatalogData } from '../types'

// Two catalog entries with distinct internal ids and 8-char upstream codes. The
// code is the durable user-facing identity and must never surface as a table
// column on the admin page; the ID column shows the internal auto-increment id.
const openai: AdminContributionEntry = {
  id: 1,
  code: 'ABCDEFGH',
  channel_type: 1,
  name: 'OpenAI',
  register_url: 'https://platform.openai.com/signup',
  key_placeholder: 'sk-...',
  enabled: true,
  host_channel_id: 2,
  plan_id: 3,
}

const azure: AdminContributionEntry = {
  id: 2,
  code: 'JKLMNPQR',
  channel_type: 3,
  name: 'Azure',
  register_url: 'https://azure.example/signup',
  key_placeholder: 'azure-...',
  enabled: false,
  host_channel_id: 2,
  plan_id: 3,
}

function renderPage(catalog: ContributionCatalogData) {
  vi.spyOn(api, 'get').mockImplementation(async (url: string) => {
    if (url === '/api/contribution/admin/catalog') {
      return { data: { success: true, data: catalog } }
    }
    if (url === '/api/channel') {
      // The dialog loads multi-key channels; keep it empty so the create form
      // renders with an empty host-channel select.
      return { data: { success: true, data: { items: [] } } }
    }
    if (url === '/api/subscription/admin/plans') {
      // The dialog loads admin subscription plans; keep them empty too.
      return { data: { success: true, data: [] } }
    }
    return { data: { success: true, data: {} } }
  })
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ContributionCatalogPage />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'admin', role: 100 })
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
})

afterEach(() => {
  vi.restoreAllMocks()
})

// The page renders the global switch, the table and the add-upstream dialog on
// the standalone /contribution-catalog route; the dialog keeps the same fields
// as before except the channel type dropdown.
it('renders the global switch, the catalog table and the add-upstream dialog', async () => {
  const user = userEvent.setup()
  renderPage({ enabled: true, entries: [openai, azure] })

  // Title and global switch (wait for the catalog to load so the switch is live).
  expect(
    await screen.findByRole('heading', { name: 'Contribution Catalog' })
  ).toBeInTheDocument()
  const table = await screen.findByRole('table')
  expect(
    screen.getByRole('switch', { name: /Open for contribution/ })
  ).toBeChecked()

  // Table lists both entries.
  expect(within(table).getByText('OpenAI')).toBeInTheDocument()
  expect(within(table).getByText('Azure')).toBeInTheDocument()

  // Opening the add dialog reveals the create form.
  await user.click(screen.getByRole('button', { name: 'Add upstream' }))
  expect(
    screen.getByRole('dialog', { name: 'Add upstream' })
  ).toBeInTheDocument()
  expect(screen.getByRole('textbox', { name: 'Display Name' })).toBeInTheDocument()
  expect(screen.getByRole('textbox', { name: 'Register URL' })).toBeInTheDocument()
  expect(screen.getByRole('textbox', { name: 'Key Placeholder' })).toBeInTheDocument()
  expect(
    screen.getByRole('combobox', { name: 'Host Channel' })
  ).toBeInTheDocument()
  expect(
    screen.getByRole('combobox', { name: 'Subscription Plan' })
  ).toBeInTheDocument()
})

// The create/edit dialog no longer exposes a Channel Type dropdown: the entry's
// identity is the auto-assigned upstream code, so there is nothing to pick.
it('does not expose a Channel Type dropdown in the create dialog', async () => {
  const user = userEvent.setup()
  renderPage({ enabled: true, entries: [openai] })

  await screen.findByRole('table')
  await user.click(screen.getByRole('button', { name: 'Add upstream' }))
  const dialog = await screen.findByRole('dialog', { name: 'Add upstream' })

  expect(
    within(dialog).queryByRole('combobox', { name: 'Channel Type' })
  ).not.toBeInTheDocument()
  expect(within(dialog).queryByText('Channel Type')).not.toBeInTheDocument()
})

// The catalog table's ID column shows the entry's internal numeric id, not the
// user-facing 8-char code: the code is the durable identity for the user page.
it('shows the internal numeric id in the ID column and never the upstream code', async () => {
  renderPage({ enabled: true, entries: [openai, azure] })

  const table = await screen.findByRole('table')
  expect(
    within(table).getByRole('columnheader', { name: 'ID' })
  ).toBeInTheDocument()
  expect(
    within(table).queryByRole('columnheader', { name: 'Channel Type' })
  ).not.toBeInTheDocument()

  // The internal ids render exactly once each (the id column); the shared host
  // channel id 2 and plan id 3 appear twice because two rows bind to them.
  expect(within(table).getAllByText('1')).toHaveLength(1)
  expect(within(table).getAllByText('2')).toHaveLength(3)
  expect(within(table).getAllByText('3')).toHaveLength(2)

  // The 8-char upstream codes never render on the admin table.
  expect(within(table).queryByText('ABCDEFGH')).not.toBeInTheDocument()
  expect(within(table).queryByText('JKLMNPQR')).not.toBeInTheDocument()
})
