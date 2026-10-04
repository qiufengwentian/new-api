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
import i18next from 'i18next'
import { beforeEach, expect, it, vi } from 'vitest'

import { submitContribution } from '../api'
import { ContributePanel } from '../components/contribute-panel'
import {
  CONTRIBUTION_SUBMIT_REJECTION_MESSAGES,
  contributionRejectionText,
} from '../constants'
import type { ContributionCatalog, ContributionSubmitData } from '../types'

vi.mock('../api')

const enabledCatalog: ContributionCatalog = {
  enabled: true,
  agreement: 'This upstream key will be added to a shared channel.',
  entries: [
    {
      channel_type: 1,
      name: 'OpenAI',
      register_url: 'https://platform.openai.com/signup',
      key_placeholder: 'sk-...',
    },
    {
      channel_type: 3,
      name: 'Azure',
      register_url: 'https://azure.example/signup',
      key_placeholder: 'azure-...',
    },
  ],
}

function renderPanel(catalog: ContributionCatalog = enabledCatalog) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ContributePanel catalog={catalog} />
    </QueryClientProvider>
  )
}

async function fillAndSubmit() {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Upstream API Key'), 'sk-typed')
  await user.click(screen.getByRole('checkbox'))
  await user.click(screen.getByRole('button', { name: 'Contribute' }))
}

function acceptedSubmission(
  overrides: Partial<ContributionSubmitData> = {}
): { success: boolean; data: ContributionSubmitData } {
  return {
    success: true,
    data: {
      contribution: {
        id: 1,
        channel_type: 1,
        channel_type_name: 'OpenAI',
        status: 'active',
        reason: '',
        reason_time: 0,
        key_mask: '************',
        subscription_id: 0,
        subscription_status: '',
        subscription: null,
        reward_granted: true,
        created_time: 0,
      },
      reward: null,
      redundant: false,
      ...overrides,
    },
  }
}

beforeEach(() => {
  vi.mocked(submitContribution).mockReset()
})

it('lists every enabled upstream and shows the registration link of the selected one', async () => {
  const user = userEvent.setup()
  renderPanel()

  expect(screen.getByRole('radio', { name: 'OpenAI' })).toBeInTheDocument()
  expect(screen.getByRole('radio', { name: 'Azure' })).toBeInTheDocument()
  expect(
    screen.getByRole('link', { name: /Register an account/ })
  ).toHaveAttribute('href', 'https://platform.openai.com/signup')

  await user.click(screen.getByRole('radio', { name: 'Azure' }))

  expect(
    screen.getByRole('link', { name: /Register an account/ })
  ).toHaveAttribute('href', 'https://azure.example/signup')
})

it('keeps the submit button disabled until a key is entered and consent is given', async () => {
  const user = userEvent.setup()
  renderPanel()

  const submit = screen.getByRole('button', { name: 'Contribute' })
  expect(submit).toBeDisabled()

  await user.type(screen.getByLabelText('Upstream API Key'), 'sk-typed')
  expect(submit).toBeDisabled()

  await user.click(screen.getByRole('checkbox'))
  expect(submit).toBeEnabled()
})

it('shows no upstream form while the catalog is empty', () => {
  renderPanel({ enabled: false, entries: [], agreement: '' })

  expect(
    screen.getByText('No upstream is open for contribution right now.')
  ).toBeInTheDocument()
  expect(screen.queryByRole('radio')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Contribute' })
  ).not.toBeInTheDocument()
  expect(screen.queryByLabelText('Upstream API Key')).not.toBeInTheDocument()
})

// The agreement is a backend-owned source string, so it has to be rendered as a
// translation key instead of being printed raw.
it('renders the backend agreement through the translation function', () => {
  i18next.addResourceBundle(
    'en',
    'translation',
    { 'Agreement fixture key': 'Translated agreement text' },
    true,
    true
  )

  renderPanel({ ...enabledCatalog, agreement: 'Agreement fixture key' })

  expect(screen.getByText('Translated agreement text')).toBeInTheDocument()
})

it('clears the key input and shows the reward after a successful submit', async () => {
  vi.mocked(submitContribution).mockResolvedValue(
    acceptedSubmission({
      reward: {
        channel_type: 1,
        subscription_id: 9,
        plan_title: 'Pro plan',
        amount_total: 1000,
        amount_used: 250,
        end_time: 1_800_000_000,
        status: 'active',
      },
    })
  )

  renderPanel()
  await fillAndSubmit()

  expect(await screen.findByText('Contribution submitted')).toBeInTheDocument()
  // The reward reports what was actually granted: the channel type it was earned
  // for, the plan, the quota split and the expiry, plus the subscription status.
  expect(screen.getByText(/Channel Type/)).toBeInTheDocument()
  expect(screen.getByText(/Pro plan/)).toBeInTheDocument()
  // The quota split is rendered through the project quota formatter, not as raw
  // numbers: 250 / 1000 quota units at the default 500000 units per USD.
  expect(screen.getByText(/\$0\.0005 \/ \$0\.002/)).toBeInTheDocument()
  expect(screen.getByText(/Expires at/)).toBeInTheDocument()
  expect(screen.getByText('Active')).toBeInTheDocument()
  expect(screen.getByLabelText('Upstream API Key')).toHaveValue('')
  expect(submitContribution).toHaveBeenCalledWith({
    channel_type: 1,
    key: 'sk-typed',
    agreed: true,
  })
})

it('renders the redundant notice when the backend accepts a duplicate contribution', async () => {
  vi.mocked(submitContribution).mockResolvedValue(
    acceptedSubmission({ redundant: true })
  )

  renderPanel()
  await fillAndSubmit()

  expect(
    await screen.findByText(
      'This upstream already grants you a reward, so the new key was accepted as redundant.'
    )
  ).toBeInTheDocument()
  // A redundant submission grants nothing, so no reward is rendered: the server
  // sends reward = null and the page must not invent an instance.
  expect(screen.queryByText('Reward')).not.toBeInTheDocument()
  expect(screen.queryByText(/Expires at/)).not.toBeInTheDocument()
})

it('keeps the key in the form when the submission is refused', async () => {
  vi.mocked(submitContribution).mockResolvedValue({
    success: false,
    code: 'contribution_key_invalid',
    message: 'the upstream key failed validation',
  })

  renderPanel()
  await fillAndSubmit()

  expect(submitContribution).toHaveBeenCalledTimes(1)
  expect(screen.getByLabelText('Upstream API Key')).toHaveValue('sk-typed')
  expect(screen.queryByText('Contribution submitted')).not.toBeInTheDocument()
})

// The cooldown refusal has to tell the user how long to wait; the server sends
// the remaining seconds with the code.
it('renders the cooldown wait time for a rate limited submission', () => {
  const text = contributionRejectionText(
    {
      code: 'contribution_rate_limited',
      retry_after_seconds: 420,
    },
    CONTRIBUTION_SUBMIT_REJECTION_MESSAGES
  )

  expect(text).toContain('420')
})

// A contribution that was recorded but whose reward could not be issued fails
// loudly: the user reads a localized reason instead of a successful response.
it('localizes the reward grant failure', () => {
  expect(
    contributionRejectionText(
      { code: 'contribution_reward_failed' },
      CONTRIBUTION_SUBMIT_REJECTION_MESSAGES
    )
  ).toBe(
    'The contribution was recorded but its reward subscription could not be issued. Please contact your administrator.'
  )
})

it('falls back to the server message for an unknown rejection code', () => {
  expect(
    contributionRejectionText(
      {
        code: 'contribution_something_new',
        message: 'the server explained it',
      },
      CONTRIBUTION_SUBMIT_REJECTION_MESSAGES
    )
  ).toBe('the server explained it')
})
