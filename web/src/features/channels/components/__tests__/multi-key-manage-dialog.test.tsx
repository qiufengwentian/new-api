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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import type { KeyStatus, ProbeAllKeysResponse, ProbeKeyResponse } from '../../types'
import { MultiKeyManageDialog } from '../dialogs/multi-key-manage-dialog'

const fixtures = vi.hoisted(() => {
  const channel = {
    id: 7,
    type: 1,
    key: '',
    status: 1,
    name: 'probe-channel',
    created_time: 0,
    test_time: 0,
    response_time: 0,
    other: '',
    balance: 0,
    balance_updated_time: 0,
    models: 'gpt-4o-mini',
    group: 'default',
    used_quota: 0,
    other_info: '',
    remark: '',
    max_input_tokens: 0,
    channel_info: {
      is_multi_key: true,
      multi_key_size: 3,
      multi_key_polling_index: 0,
      multi_key_mode: 'random',
    },
    settings: '{}',
  }
  return { channel }
})

vi.mock('../channels-provider', () => ({
  useChannels: () => ({
    open: 'multi-key-manage',
    currentRow: fixtures.channel,
    setOpen: () => {},
  }),
}))

vi.mock('@/lib/handle-server-error', () => ({
  handleServerError: vi.fn(async () => {}),
}))

const originalAuth = useAuthStore.getState().auth
let client: QueryClient

// Three key rows: probed-ok, never probed (manual disabled), probed-error.
const keyStatusPayload: KeyStatus[] = [
  {
    index: 0,
    status: 1,
    key_preview: 'sk-key-a...',
    probe: { result: 'ok', last_probe_at: 1700000000 },
  },
  {
    index: 1,
    status: 2,
    key_preview: 'sk-key-b...',
    disabled_time: 1699999000,
    reason: 'manual operation',
  },
  {
    index: 2,
    status: 3,
    key_preview: 'sk-key-c...',
    disabled_time: 1699999500,
    reason: 'upstream 401',
    probe: {
      result: 'error',
      error_code: 'invalid_api_key',
      last_probe_at: 1700000500,
    },
  },
]

const batchProbePayload: ProbeAllKeysResponse = {
  success: true,
  data: {
    keys: [
      { key_index: 0, probe: { result: 'ok', last_probe_at: 1700002000 } },
      { key_index: 1, probe: { result: 'ok', last_probe_at: 1700002001 } },
      {
        key_index: 2,
        probe: {
          result: 'error',
          error_code: 'invalid_api_key',
          last_probe_at: 1700002002,
        },
      },
    ],
    summary: { tested: 3, available: 2, unavailable: 1 },
  },
}

function mockChannelApi(
  probe: (keyIndex: number) => Promise<ProbeKeyResponse>,
  probeAll?: () => Promise<ProbeAllKeysResponse>
) {
  let statusCalls = 0
  let probeAllCalls = 0
  const probeCalls: number[] = []
  const post = vi
    .spyOn(api, 'post')
    .mockImplementation(async (url: string, data?: unknown) => {
      if (url === '/api/channel/multi_key/manage') {
        statusCalls++
        return {
          data: {
            success: true,
            data: {
              keys: keyStatusPayload,
              total: 3,
              page: 1,
              page_size: 10,
              total_pages: 1,
              enabled_count: 1,
              manual_disabled_count: 1,
              auto_disabled_count: 1,
            },
          },
        }
      }
      if (url === '/api/channel/7/probe_all_keys') {
        probeAllCalls++
        if (!probeAll) throw new Error('unexpected batch probe request')
        const response = await probeAll()
        return { data: response }
      }
      const keyIndex =
        (data as { key_index?: number } | undefined)?.key_index ?? -1
      probeCalls.push(keyIndex)
      const response = await probe(keyIndex)
      return { data: response }
    })
  return {
    post,
    statusCalls: () => statusCalls,
    probeCalls: () => probeCalls,
    probeAllCalls: () => probeAllCalls,
  }
}

// The dialog renders one static table; rows are scoped by their "#N" index label.
function rowContaining(
  container: HTMLElement,
  indexLabel: string
): HTMLElement {
  const rows = within(container).queryAllByRole('row')
  const row = rows.find((r) => within(r).queryByText(indexLabel) !== null)
  if (!row) throw new Error(`row with label ${indexLabel} not found`)
  return row
}

function renderDialog() {
  render(
    <QueryClientProvider client={client}>
      <MultiKeyManageDialog open onOpenChange={() => {}} />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
})

afterEach(() => {
  client.clear()
  useAuthStore.setState({ auth: originalAuth })
})

it('renders the probe column from the key status payload', async () => {
  mockChannelApi(async () => ({
    success: true,
    data: { key_index: 0, probe: { result: 'ok', last_probe_at: 1700000000 } },
  }))

  renderDialog()

  await waitFor(() =>
    expect(screen.getByRole('columnheader', { name: 'Probe' })).toBeVisible()
  )
  expect(screen.getByText('Available')).toBeVisible()
  expect(screen.getByText('Unavailable')).toBeVisible()
  expect(screen.getByText('Not probed')).toBeVisible()
  expect(screen.getByText('invalid_api_key')).toBeVisible()

  // Every key row carries a Test button, including the disabled rows.
  expect(screen.getAllByRole('button', { name: 'Test' })).toHaveLength(3)
})

it('probes the clicked key and updates the row without reloading the key list', async () => {
  const { probeCalls, statusCalls } = mockChannelApi(async (keyIndex) => {
    if (keyIndex === 1) {
      return {
        success: true,
        data: {
          key_index: 1,
          probe: { result: 'ok', last_probe_at: 1700000999 },
        },
      }
    }
    return {
      success: false,
      message: 'unexpected key',
      data: {
        key_index: keyIndex,
        probe: { result: 'error', last_probe_at: 1700001000 },
      },
    }
  })

  const user = userEvent.setup()
  renderDialog()
  const table = await screen.findByRole('table')
  const row = rowContaining(table, '#2')
  await waitFor(() =>
    expect(within(row).queryByText('Not probed')).not.toBeNull()
  )

  await user.click(within(row).getByRole('button', { name: 'Test' }))

  await waitFor(() => expect(within(row).queryByText('Not probed')).toBeNull())
  expect(within(row).getByText('Available')).toBeVisible()

  expect(probeCalls()).toEqual([1])
  // The key list was loaded once on open and must not be reloaded after a probe.
  expect(statusCalls()).toBe(1)
})

it('shows a failed probe as unavailable with its error code on the row', async () => {
  mockChannelApi(async (keyIndex) => {
    if (keyIndex === 0) {
      return {
        success: false,
        message: 'bad response status code 401',
        error_code: 'invalid_api_key',
        data: {
          key_index: 0,
          probe: {
            result: 'error',
            error_code: 'invalid_api_key',
            last_probe_at: 1700001111,
          },
        },
      }
    }
    return { success: false, message: 'unexpected key' }
  })

  const user = userEvent.setup()
  renderDialog()
  const table = await screen.findByRole('table')
  const row = rowContaining(table, '#1')
  await waitFor(() =>
    expect(within(row).queryByText('Available')).not.toBeNull()
  )

  await user.click(within(row).getByRole('button', { name: 'Test' }))

  await waitFor(() =>
    expect(within(row).queryByText('Unavailable')).not.toBeNull()
  )
  expect(within(row).getByText('invalid_api_key')).toBeVisible()
})

it('disables the test buttons while a probe is in flight', async () => {
  let resolveProbe: (() => void) | undefined
  const { probeCalls } = mockChannelApi(
    (keyIndex) =>
      new Promise<ProbeKeyResponse>((resolve) => {
        resolveProbe = () =>
          resolve({
            success: true,
            data: {
              key_index: keyIndex,
              probe: { result: 'ok', last_probe_at: 1700000999 },
            },
          })
      })
  )

  const user = userEvent.setup()
  renderDialog()
  const table = await screen.findByRole('table')
  const row = rowContaining(table, '#2')
  await waitFor(() =>
    expect(within(row).queryByText('Not probed')).not.toBeNull()
  )

  await user.click(within(row).getByRole('button', { name: 'Test' }))

  expect(
    (
      screen.getAllByRole('button', { name: 'Test' }) as HTMLButtonElement[]
    ).every((button) => button.disabled)
  ).toBe(true)
  // The batch button is mutually exclusive with an in-flight single-row probe.
  expect(screen.getByRole('button', { name: 'Test All Keys' })).toBeDisabled()
  // Clicking another row's Test while one is in flight is a no-op.
  const otherRow = rowContaining(table, '#3')
  await user.click(within(otherRow).getByRole('button', { name: 'Test' }))
  // A second probe must not start while one is in flight.
  expect(probeCalls()).toEqual([1])

  resolveProbe?.()
  const rowAfter = rowContaining(table, '#2')
  await waitFor(() =>
    expect(within(rowAfter).queryByText('Available')).not.toBeNull()
  )
  expect(
    (
      screen.getAllByRole('button', { name: 'Test' }) as HTMLButtonElement[]
    ).every((button) => button.disabled)
  ).toBe(false)
  // The batch button becomes available again once the single probe settles.
  expect(screen.getByRole('button', { name: 'Test All Keys' })).toBeEnabled()
})

it('probes all keys in one request, refreshes every row, and toasts the summary', async () => {
  let resolveBatch: (() => void) | undefined
  const { probeCalls, probeAllCalls } = mockChannelApi(
    async () => ({ success: false, message: 'unexpected single probe' }),
    () =>
      new Promise<ProbeAllKeysResponse>((resolve) => {
        resolveBatch = () => resolve(batchProbePayload)
      })
  )
  const successSpy = vi.spyOn(toast, 'success').mockReturnValue(0)

  const user = userEvent.setup()
  renderDialog()
  const table = await screen.findByRole('table')
  const row2 = rowContaining(table, '#2')
  await waitFor(() =>
    expect(within(row2).queryByText('Not probed')).not.toBeNull()
  )

  await user.click(screen.getByRole('button', { name: 'Test All Keys' }))

  // While the batch is in flight, every row probes, the batch button is
  // locked, and no single-row probe may start.
  expect(
    (
      screen.getAllByRole('button', { name: 'Test' }) as HTMLButtonElement[]
    ).every((button) => button.disabled)
  ).toBe(true)
  expect(screen.getByRole('button', { name: 'Test All Keys' })).toBeDisabled()
  expect(probeAllCalls()).toBe(1)
  expect(probeCalls()).toEqual([])

  resolveBatch?.()
  await waitFor(() =>
    expect(
      within(rowContaining(table, '#2')).queryByText('Available')
    ).not.toBeNull()
  )
  // The never-probed row now shows its result; the failed row keeps its
  // error code, refreshed by the batch.
  expect(
    within(rowContaining(table, '#3')).getByText('invalid_api_key')
  ).toBeVisible()
  expect(successSpy).toHaveBeenCalledWith(
    'Probed 3 keys: 2 available, 1 unavailable'
  )
  // The batch left no probing state behind: every control is retryable.
  expect(
    (
      screen.getAllByRole('button', { name: 'Test' }) as HTMLButtonElement[]
    ).every((button) => !button.disabled)
  ).toBe(true)
  expect(screen.getByRole('button', { name: 'Test All Keys' })).toBeEnabled()
})

it('falls back to a retryable state when the batch request fails', async () => {
  const { probeAllCalls } = mockChannelApi(
    async () => ({ success: false, message: 'unexpected single probe' }),
    () => Promise.reject(new Error('network down'))
  )

  const user = userEvent.setup()
  renderDialog()
  await screen.findByRole('table')

  await user.click(screen.getByRole('button', { name: 'Test All Keys' }))

  await waitFor(() =>
    expect(handleServerError).toHaveBeenCalledWith(
      expect.any(Error),
      'Failed to probe all keys'
    )
  )
  // No fake in-flight state remains: every row's Test button and the batch
  // button are retryable.
  expect(
    (
      screen.getAllByRole('button', { name: 'Test' }) as HTMLButtonElement[]
    ).every((button) => !button.disabled)
  ).toBe(true)
  expect(screen.getByRole('button', { name: 'Test All Keys' })).toBeEnabled()
  expect(probeAllCalls()).toBe(1)
})

it('reports a probe response without a health payload as an error', async () => {
  const { probeCalls } = mockChannelApi(async () => ({
    success: false,
    message: '该渠道不是多密钥模式',
  }))

  const user = userEvent.setup()
  renderDialog()
  const table = await screen.findByRole('table')
  const row = rowContaining(table, '#2')
  await waitFor(() =>
    expect(within(row).queryByText('Not probed')).not.toBeNull()
  )

  await user.click(within(row).getByRole('button', { name: 'Test' }))

  await waitFor(() =>
    expect(handleServerError).toHaveBeenCalledWith(
      expect.objectContaining({ success: false }),
      'Failed to probe key'
    )
  )
  expect(probeCalls()).toEqual([1])
  expect(within(row).queryByText('Available')).toBeNull()
  expect(within(row).queryByText('Unavailable')).toBeNull()
})
