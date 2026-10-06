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
import {
  useCallback,
  useState,
  type Dispatch,
  type SetStateAction,
} from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { handleServerError } from '@/lib/handle-server-error'

import { probeAllMultiKeys, probeMultiKey } from '../api'
import type { KeyStatus } from '../types'

// Per-key and all-keys probe state for the multi-key manage dialog. A
// single-row probe and the batch probe are mutually exclusive, and both
// refresh the visible rows in place from the response instead of reloading
// the key list. Probe results are reference-only: they never disable or
// re-schedule keys.
export function useMultiKeyProbe(
  channelId: number | null,
  setKeys: Dispatch<SetStateAction<KeyStatus[]>>
) {
  const { t } = useTranslation()
  // Key index of the in-flight per-key probe; null when no probe is running.
  const [probingIndex, setProbingIndex] = useState<number | null>(null)
  // True while the batch "probe all keys" request is in flight. Single-row
  // probes and the batch button are mutually exclusive with it.
  const [probingAll, setProbingAll] = useState(false)

  // Probe one key with a minimal real upstream request. The result is
  // reference-only: it updates the row's Probe column but never disables or
  // re-schedules the key. The local row is refreshed from the response so the
  // column reflects the persisted health without a full reload.
  const handleTestKey = useCallback(
    async (keyIndex: number) => {
      if (channelId === null || probingIndex !== null || probingAll) return
      setProbingIndex(keyIndex)
      try {
        const response = await probeMultiKey(channelId, keyIndex)
        const probe = response.data?.probe
        if (!probe) {
          handleServerError(response, t('Failed to probe key'))
          return
        }
        setKeys((prev) =>
          prev.map((key) => (key.index === keyIndex ? { ...key, probe } : key))
        )
      } catch (error: unknown) {
        handleServerError(error, t('Failed to probe key'))
      } finally {
        setProbingIndex(null)
      }
    },
    [channelId, probingIndex, probingAll, setKeys, t]
  )

  // Probe every key of the channel in one request: the server runs the batch
  // with capped concurrency and returns the per-key results plus a summary.
  // Each visible row is refreshed from the response; a failed or
  // interrupted request falls back to the retryable state without leaving
  // any row stuck in the probing state.
  const handleProbeAllKeys = useCallback(async () => {
    if (channelId === null || probingIndex !== null || probingAll) return
    setProbingAll(true)
    try {
      const response = await probeAllMultiKeys(channelId)
      const data = response.data
      if (!data) {
        handleServerError(response, t('Failed to probe all keys'))
        return
      }
      const probedByKey = new Map(
        data.keys.map((entry) => [entry.key_index, entry.probe])
      )
      setKeys((prev) =>
        prev.map((key) => {
          const probe = probedByKey.get(key.index)
          return probe ? { ...key, probe } : key
        })
      )
      const { tested, available, unavailable } = data.summary
      toast.success(
        t(
          'Probed {{tested}} keys: {{available}} available, {{unavailable}} unavailable',
          {
            tested,
            available,
            unavailable,
          }
        )
      )
    } catch (error: unknown) {
      handleServerError(error, t('Failed to probe all keys'))
    } finally {
      setProbingAll(false)
    }
  }, [channelId, probingIndex, probingAll, setKeys, t])

  return { probingIndex, probingAll, handleTestKey, handleProbeAllKeys }
}
