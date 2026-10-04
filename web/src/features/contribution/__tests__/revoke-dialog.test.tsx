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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it, vi } from 'vitest'

import { revokeContribution } from '../api'
import { RevokeContributionDialog } from '../components/revoke-contribution-dialog'
import { contributionRevokeRejectionText } from '../constants'
import type { ContributionSummary } from '../types'

vi.mock('../api')

const activeContribution: ContributionSummary = {
  id: 42,
  channel_type: 1,
  channel_type_name: 'OpenAI',
  status: 'active',
  reason: '',
  reason_time: 0,
  key_mask: '************',
  subscription_id: 9,
  subscription_status: 'active',
  reward_granted: true,
  created_time: 1_700_000_000,
}

function renderDialog(onClose = vi.fn()) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const view = render(
    <QueryClientProvider client={queryClient}>
      <RevokeContributionDialog
        contribution={activeContribution}
        onClose={onClose}
      />
    </QueryClientProvider>
  )
  return { ...view, onClose }
}

beforeEach(() => {
  vi.mocked(revokeContribution).mockReset()
})

// Withdrawing is irreversible, so the endpoint is only called after the explicit
// confirmation the shared ConfirmDialog provides.
it('withdraws the record only after the confirmation', async () => {
  const user = userEvent.setup()
  vi.mocked(revokeContribution).mockResolvedValue({
    success: true,
    data: {
      contribution: {
        ...activeContribution,
        status: 'revoked',
        reason: 'user_revoked',
        subscription_status: 'cancelled',
      },
    },
  })
  const { onClose } = renderDialog()

  expect(
    screen.getByRole('heading', { name: 'Revoke contribution' })
  ).toBeInTheDocument()
  expect(revokeContribution).not.toHaveBeenCalled()

  await user.click(screen.getByRole('button', { name: 'Revoke' }))

  expect(revokeContribution).toHaveBeenCalledWith(42)
  expect(onClose).toHaveBeenCalled()
})

it('leaves the record alone when the confirmation is cancelled', async () => {
  const user = userEvent.setup()
  const { onClose } = renderDialog()

  await user.click(screen.getByRole('button', { name: 'Cancel' }))

  expect(revokeContribution).not.toHaveBeenCalled()
  expect(onClose).toHaveBeenCalled()
})

// A refusal the user can act on is copy, not a raw server sentence: each known
// rejection code resolves to its own localized text.
it('localizes the withdrawal refusals', () => {
  expect(
    contributionRevokeRejectionText({ code: 'contribution_not_found' })
  ).toBe('This contribution no longer exists.')
  expect(
    contributionRevokeRejectionText({ code: 'contribution_dead_is_final' })
  ).toBe(
    'This contribution was ended because the upstream rejected the key, so it cannot be revoked.'
  )
  expect(
    contributionRevokeRejectionText({
      code: 'contribution_something_new',
      message: 'the server explained it',
    })
  ).toBe('the server explained it')
})
