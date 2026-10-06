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
import { beforeEach, expect, it, vi } from 'vitest'

import { getMyContributions } from '../api'
import { MyContributions } from '../components/my-contributions'
import type { ContributionMineData, ContributionSummary } from '../types'

vi.mock('../api')

// The three states a contribution can be in, each with the reward it did or did
// not earn: a live record with an active subscription, one the upstream killed,
// and one the contributor withdrew.
const contributions: ContributionSummary[] = [
  {
    id: 3,
    entry_code: 'QWERTYAB',
    channel_type: 1,
    channel_type_name: 'OpenAI',
    status: 'active',
    reason: '',
    reason_time: 0,
    key_mask: '************',
    subscription_id: 9,
    subscription_status: 'active',
    subscription: {
      plan_title: 'Pro plan',
      amount_total: 1000,
      amount_used: 250,
      end_time: 1_800_000_000,
      status: 'active',
    },
    reward_granted: true,
    created_time: 1_700_000_000,
  },
  {
    id: 2,
    entry_code: 'ZXCVBNM2',
    channel_type: 3,
    channel_type_name: 'Azure',
    status: 'dead',
    reason: 'upstream_unauthorized',
    reason_time: 1_700_100_000,
    key_mask: '************',
    subscription_id: 10,
    subscription_status: 'cancelled',
    subscription: {
      plan_title: 'Reward 1',
      amount_total: 5000,
      amount_used: 5000,
      end_time: 1_800_100_000,
      status: 'cancelled',
    },
    reward_granted: true,
    created_time: 1_699_000_000,
  },
  {
    id: 1,
    entry_code: 'HJKMPQRS',
    channel_type: 14,
    channel_type_name: 'Anthropic',
    status: 'revoked',
    reason: 'user_revoked',
    reason_time: 1_700_200_000,
    key_mask: '************',
    subscription_id: 0,
    subscription_status: '',
    subscription: null,
    reward_granted: false,
    created_time: 1_698_000_000,
  },
]

function renderList(data: ContributionMineData) {
  vi.mocked(getMyContributions).mockResolvedValue({ success: true, data })
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <MyContributions />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  vi.mocked(getMyContributions).mockReset()
})

// Every state reads truthfully: the status, the reason it ended and the state of
// the reward it earned.
it('renders every contribution state with its reason and reward', async () => {
  renderList({ items: contributions, summary: { entry_code_count: 1 } })

  // The record's own status and its reward's status both read "Active" here.
  const activeRow = await screen.findByRole('row', { name: /OpenAI/ })
  expect(within(activeRow).getAllByText('Active')).toHaveLength(2)
  expect(within(activeRow).getByText('Pro plan')).toBeInTheDocument()

  const deadRow = screen.getByRole('row', { name: /Azure/ })
  expect(within(deadRow).getByText('Dead')).toBeInTheDocument()
  expect(
    within(deadRow).getByText('The upstream rejected this key')
  ).toBeInTheDocument()
  expect(within(deadRow).getByText('Invalidated')).toBeInTheDocument()

  const revokedRow = screen.getByRole('row', { name: /Anthropic/ })
  expect(within(revokedRow).getByText('Revoked')).toBeInTheDocument()
  expect(
    within(revokedRow).getByText('You revoked this contribution')
  ).toBeInTheDocument()
  expect(within(revokedRow).getByText('No reward')).toBeInTheDocument()
})

// The list only ever shows the display mask the backend stores; the plaintext key
// is not part of the contract at all.
it('shows the stored key mask for every record', async () => {
  renderList({ items: contributions, summary: { entry_code_count: 1 } })

  await screen.findByRole('row', { name: /OpenAI/ })
  expect(screen.getAllByText('************')).toHaveLength(3)
})

it('reports how many upstreams (distinct codes) already reward the account', async () => {
  renderList({ items: contributions, summary: { entry_code_count: 2 } })

  expect(
    await screen.findByText('You have brought in 2 upstreams.')
  ).toBeInTheDocument()
})

// Withdrawing is only possible while the record is live, and it goes through the
// existing confirmation dialog rather than acting on the click alone.
it('offers the revoke action on a live record only', async () => {
  const user = userEvent.setup()
  renderList({ items: contributions, summary: { entry_code_count: 1 } })

  const activeRow = await screen.findByRole('row', { name: /OpenAI/ })
  const deadRow = screen.getByRole('row', { name: /Azure/ })
  const revokedRow = screen.getByRole('row', { name: /Anthropic/ })

  expect(
    within(deadRow).getByRole('button', { name: 'Revoke' })
  ).toBeDisabled()
  expect(
    within(revokedRow).getByRole('button', { name: 'Revoke' })
  ).toBeDisabled()

  await user.click(within(activeRow).getByRole('button', { name: 'Revoke' }))

  expect(
    screen.getByRole('heading', { name: 'Revoke contribution' })
  ).toBeInTheDocument()
})

it('shows an empty state before the first contribution', async () => {
  renderList({ items: [], summary: { entry_code_count: 0 } })

  expect(await screen.findByText('No contributions yet')).toBeInTheDocument()
})
