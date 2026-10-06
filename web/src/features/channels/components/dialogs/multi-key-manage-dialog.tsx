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
import { useQueryClient } from '@tanstack/react-query'
import {
  Loader2,
  RefreshCw,
  Trash2,
  Power,
  PowerOff,
  Gauge,
  Eye,
  EyeOff,
} from 'lucide-react'
import { useState, useEffect, useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { SecureVerificationDialog } from '@/features/auth/secure-verification'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { ROLE } from '@/lib/roles'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import {
  getMultiKeyStatus,
  enableMultiKey,
  disableMultiKey,
  deleteMultiKey,
  enableAllMultiKeys,
  disableAllMultiKeys,
  deleteDisabledMultiKeys,
} from '../../api'
import { MULTI_KEY_FILTER_OPTIONS } from '../../constants'
import { useChannelKeyDisclosure } from '../../hooks/use-channel-key-disclosure'
import { useMultiKeyProbe } from '../../hooks/use-multi-key-probe'
import {
  channelsQueryKeys,
  formatTimestamp,
  getMultiKeyStatusConfig,
  getMultiKeyConfirmMessage,
  isDestructiveAction,
  splitChannelKeys,
} from '../../lib'
import type { KeyStatus, MultiKeyConfirmAction } from '../../types'
import { useChannels } from '../channels-provider'
import { StatisticsCard } from './multi-key-statistics-card'
import { MultiKeyTableRowActions } from './multi-key-table-row-actions'

type MultiKeyManageDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function MultiKeyManageDialog({
  open,
  onOpenChange,
}: MultiKeyManageDialogProps) {
  const { t } = useTranslation()
  const { currentRow } = useChannels()
  const queryClient = useQueryClient()
  const currentUser = useAuthStore((s) => s.auth.user)
  const canEditSensitive = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.SENSITIVE_WRITE
  )
  // Key disclosure is super-admin only: the key read endpoint is root-only,
  // so the entry is hidden (not merely disabled) for everyone else.
  const canRevealKeys = currentUser?.role === ROLE.SUPER_ADMIN
  const { channelKey, isChannelKeyLoading, handleRevealKey, verification } =
    useChannelKeyDisclosure(open, currentRow?.id ?? null)
  // Indices of rows whose plaintext is currently shown. Cleared whenever the
  // dialog closes or the channel changes so no key material lingers.
  const [revealedKeyIndices, setRevealedKeyIndices] = useState<Set<number>>(
    () => new Set()
  )
  const revealedKeys = useMemo(
    () => (channelKey ? splitChannelKeys(channelKey) : []),
    [channelKey]
  )

  // Data state
  const [isLoading, setIsLoading] = useState(false)
  const [keys, setKeys] = useState<KeyStatus[]>([])
  const [currentPage, setCurrentPage] = useState(1)
  const [pageSize, setPageSize] = useState(10)
  const [total, setTotal] = useState(0)
  const [totalPages, setTotalPages] = useState(0)
  const [enabledCount, setEnabledCount] = useState(0)
  const [manualDisabledCount, setManualDisabledCount] = useState(0)
  const [autoDisabledCount, setAutoDisabledCount] = useState(0)

  // UI state
  const [statusFilter, setStatusFilter] = useState<number | null>(null)
  const [confirmAction, setConfirmAction] =
    useState<MultiKeyConfirmAction | null>(null)
  const [isPerformingAction, setIsPerformingAction] = useState(false)
  const { probingIndex, probingAll, handleTestKey, handleProbeAllKeys } =
    useMultiKeyProbe(currentRow?.id ?? null, setKeys)

  // Reset and load data when dialog opens
  useEffect(() => {
    if (open && currentRow) {
      setCurrentPage(1)
      setStatusFilter(null)
      loadKeyStatus(1, pageSize, null)
    }
    setRevealedKeyIndices(new Set())
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, currentRow?.id])

  const loadKeyStatus = async (
    page: number = currentPage,
    size: number = pageSize,
    status: number | null = statusFilter
  ) => {
    if (!currentRow) return

    setIsLoading(true)
    try {
      const response = await getMultiKeyStatus(
        currentRow.id,
        page,
        size,
        status === null ? undefined : status
      )

      if (response.success && response.data) {
        setKeys(response.data.keys || [])
        setTotal(response.data.total || 0)
        setCurrentPage(response.data.page || 1)
        setPageSize(response.data.page_size || 10)
        setTotalPages(response.data.total_pages || 0)
        setEnabledCount(response.data.enabled_count || 0)
        setManualDisabledCount(response.data.manual_disabled_count || 0)
        setAutoDisabledCount(response.data.auto_disabled_count || 0)
      } else {
        handleServerError(response, t('Failed to load key status'))
      }
    } catch (error: unknown) {
      handleServerError(error, t('Failed to load key status'))
    } finally {
      setIsLoading(false)
    }
  }

  const handleStatusFilterChange = (value: string) => {
    const newFilter = value === 'all' ? null : Number.parseInt(value)
    setStatusFilter(newFilter)
    setCurrentPage(1)
    loadKeyStatus(1, pageSize, newFilter)
  }

  const handlePageChange = (newPage: number) => {
    setCurrentPage(newPage)
    loadKeyStatus(newPage, pageSize)
  }

  const performAction = async () => {
    if (!confirmAction || !currentRow) return
    if (
      !canEditSensitive &&
      (confirmAction.type === 'delete' ||
        confirmAction.type === 'delete-disabled')
    ) {
      setConfirmAction(null)
      return
    }

    setIsPerformingAction(true)
    try {
      const { type, keyIndex } = confirmAction
      let response

      // Execute the appropriate action
      if (type === 'enable' && keyIndex !== undefined) {
        response = await enableMultiKey(currentRow.id, keyIndex)
      } else if (type === 'disable' && keyIndex !== undefined) {
        response = await disableMultiKey(currentRow.id, keyIndex)
      } else if (type === 'delete' && keyIndex !== undefined) {
        response = await deleteMultiKey(currentRow.id, keyIndex)
      } else if (type === 'enable-all') {
        response = await enableAllMultiKeys(currentRow.id)
      } else if (type === 'disable-all') {
        response = await disableAllMultiKeys(currentRow.id)
      } else if (type === 'delete-disabled') {
        response = await deleteDisabledMultiKeys(currentRow.id)
      }

      if (response?.success) {
        toast.success(response.message || t('Operation successful'))
        queryClient.invalidateQueries({ queryKey: channelsQueryKeys.lists() })

        // Reload data - reset to page 1 for bulk actions
        const isBulkAction = type.includes('all') || type === 'delete-disabled'
        if (isBulkAction) {
          setCurrentPage(1)
          loadKeyStatus(1, pageSize)
        } else {
          loadKeyStatus(currentPage, pageSize)
        }
      } else {
        handleServerError(response, t('Operation failed'))
      }
    } catch (error: unknown) {
      handleServerError(error, t('Operation failed'))
    } finally {
      setIsPerformingAction(false)
      setConfirmAction(null)
    }
  }

  const toggleKeyReveal = (keyIndex: number) => {
    setRevealedKeyIndices((previous) => {
      const next = new Set(previous)
      if (next.has(keyIndex)) {
        next.delete(keyIndex)
      } else {
        next.add(keyIndex)
      }
      return next
    })
  }

  const renderStatusBadge = (status: number) => {
    const config = getMultiKeyStatusConfig(status)
    return (
      <StatusBadge
        label={t(config.label)}
        variant={config.variant}
        showDot
        copyable={false}
      />
    )
  }

  const formatKeyTimestamp = (timestamp?: number) => {
    if (!timestamp) return '-'
    return formatTimestamp(timestamp)
  }

  const renderProbeCell = (key: KeyStatus) => {
    const probe = key.probe
    if (!probe) {
      return (
        <StatusBadge
          label={t('Not probed')}
          variant='neutral'
          showDot
          copyable={false}
        />
      )
    }
    const available = probe.result === 'ok'
    return (
      <div className='flex flex-col gap-1'>
        <StatusBadge
          label={available ? t('Available') : t('Unavailable')}
          variant={available ? 'success' : 'danger'}
          showDot
          copyable={false}
        />
        {probe.error_code && (
          <div className='text-muted-foreground truncate text-xs'>
            {probe.error_code}
          </div>
        )}
        {probe.last_probe_at && (
          <div className='text-muted-foreground text-xs'>
            {formatTimestamp(probe.last_probe_at)}
          </div>
        )}
      </div>
    )
  }

  if (!currentRow) return null

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={onOpenChange}
        title={
          <>
            {t('Multi-Key Management')}
            <StatusBadge
              label={currentRow.name}
              variant='neutral'
              copyable={false}
            />
            {currentRow.channel_info?.multi_key_mode && (
              <StatusBadge
                label={
                  currentRow.channel_info.multi_key_mode === 'random'
                    ? t('Random')
                    : t('Polling')
                }
                variant='neutral'
                copyable={false}
              />
            )}
          </>
        }
        description={t(
          'Manage multi-key status and configuration for this channel'
        )}
        contentClassName='flex max-h-[min(90dvh,var(--dialog-available-height))] max-w-5xl flex-col'
        titleClassName='flex items-center gap-2'
        contentHeight='min(72vh, 720px)'
        bodyClassName='space-y-4'
      >
        <div className='flex min-h-0 flex-1 flex-col space-y-4 overflow-hidden'>
          {/* Statistics */}
          <div className='grid shrink-0 grid-cols-3 gap-3'>
            <StatisticsCard
              label={t('Enabled')}
              count={enabledCount}
              total={total}
            />
            <StatisticsCard
              label={t('Manual Disabled')}
              count={manualDisabledCount}
              total={total}
            />
            <StatisticsCard
              label={t('Auto Disabled')}
              count={autoDisabledCount}
              total={total}
            />
          </div>

          <Separator className='shrink-0' />

          {/* Toolbar */}
          <div className='flex shrink-0 items-center justify-between'>
            <Select
              items={MULTI_KEY_FILTER_OPTIONS.map((option) => ({
                value: option.value,
                label: t(option.label),
              }))}
              value={statusFilter === null ? 'all' : statusFilter.toString()}
              onValueChange={(v) => v !== null && handleStatusFilterChange(v)}
            >
              <SelectTrigger className='w-40'>
                <SelectValue placeholder={t('All Status')} />
              </SelectTrigger>
              <SelectContent alignItemWithTrigger={false}>
                <SelectGroup>
                  {MULTI_KEY_FILTER_OPTIONS.map((option) => (
                    <SelectItem key={option.value} value={option.value}>
                      {t(option.label)}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>

            <div className='flex items-center gap-2'>
              {canRevealKeys && (
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => void handleRevealKey()}
                  disabled={isChannelKeyLoading || verification.isActive}
                >
                  {isChannelKeyLoading || verification.isActive ? (
                    <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                  ) : (
                    <Eye className='mr-2 h-4 w-4' />
                  )}
                  {t('Reveal keys')}
                </Button>
              )}
              <Button
                variant='outline'
                size='sm'
                onClick={handleProbeAllKeys}
                disabled={probingIndex !== null || probingAll}
              >
                {probingAll ? (
                  <Loader2 className='h-4 w-4 animate-spin' />
                ) : (
                  <Gauge className='h-4 w-4' />
                )}
                {t('Test All Keys')}
              </Button>
              <Button
                variant='outline'
                size='sm'
                onClick={() => loadKeyStatus()}
                disabled={isLoading}
              >
                <RefreshCw className='h-4 w-4' />
              </Button>

              {manualDisabledCount + autoDisabledCount > 0 && (
                <Button
                  variant='default'
                  size='sm'
                  onClick={() => setConfirmAction({ type: 'enable-all' })}
                >
                  <Power className='mr-2 h-4 w-4' />
                  {t('Enable All')}
                </Button>
              )}

              {enabledCount > 0 && (
                <Button
                  variant='destructive'
                  size='sm'
                  onClick={() => setConfirmAction({ type: 'disable-all' })}
                >
                  <PowerOff className='mr-2 h-4 w-4' />
                  {t('Disable All')}
                </Button>
              )}

              {autoDisabledCount > 0 && (
                <Button
                  variant='destructive'
                  size='sm'
                  onClick={() => {
                    if (!canEditSensitive) return
                    setConfirmAction({ type: 'delete-disabled' })
                  }}
                  disabled={!canEditSensitive}
                  title={
                    canEditSensitive
                      ? undefined
                      : t('No permission to perform this action')
                  }
                >
                  <Trash2 className='mr-2 h-4 w-4' />
                  {t('Delete Auto-Disabled')}
                </Button>
              )}
            </div>
          </div>
          {!canEditSensitive && (
            <p className='text-muted-foreground text-xs'>
              {t('No permission to perform this action')}
            </p>
          )}

          {/* Table */}
          <div className='min-h-0 flex-1 overflow-auto rounded-md border'>
            {isLoading && (
              <div className='flex items-center justify-center py-12'>
                <Loader2 className='text-muted-foreground h-8 w-8 animate-spin' />
              </div>
            )}
            {!isLoading && keys.length === 0 && (
              <div className='text-muted-foreground py-12 text-center'>
                {t('No keys found')}
              </div>
            )}
            {!isLoading && !(keys.length === 0) && (
              <StaticDataTable
                className='rounded-none border-0'
                tableClassName='min-w-[800px]'
                data={keys}
                getRowKey={(key) => key.index}
                columns={[
                  {
                    id: 'index',
                    header: t('Index'),
                    className: 'w-20',
                    cellClassName: 'font-mono text-sm',
                    cell: (key) => `#${key.index + 1}`,
                  },
                  ...(canRevealKeys
                    ? [
                        {
                          id: 'key',
                          header: t('Key'),
                          className: 'min-w-[220px]',
                          cell: (key: KeyStatus) => {
                            const plaintext = revealedKeys[key.index]
                            const revealed = revealedKeyIndices.has(key.index)
                            return (
                              <div className='flex items-center gap-1'>
                                <span
                                  className={cn(
                                    'min-w-0 flex-1 truncate font-mono text-sm',
                                    !revealed && 'text-muted-foreground'
                                  )}
                                >
                                  {revealed
                                    ? (plaintext ?? key.key_preview ?? '')
                                    : key.key_preview}
                                </span>
                                {plaintext !== undefined && (
                                  <>
                                    <Button
                                      type='button'
                                      variant='ghost'
                                      size='icon'
                                      onClick={() => toggleKeyReveal(key.index)}
                                      aria-label={
                                        revealed ? t('Hide key') : t('Show key')
                                      }
                                      title={
                                        revealed ? t('Hide key') : t('Show key')
                                      }
                                    >
                                      {revealed ? (
                                        <EyeOff className='h-4 w-4' />
                                      ) : (
                                        <Eye className='h-4 w-4' />
                                      )}
                                    </Button>
                                    <CopyButton
                                      value={plaintext}
                                      aria-label={t('Copy key')}
                                    />
                                  </>
                                )}
                              </div>
                            )
                          },
                        },
                      ]
                    : []),
                  {
                    id: 'status',
                    header: t('Status'),
                    className: 'w-32',
                    cell: (key) => renderStatusBadge(key.status),
                  },
                  {
                    id: 'probe',
                    header: t('Probe'),
                    className: 'w-44',
                    cell: (key) => renderProbeCell(key),
                  },
                  {
                    id: 'reason',
                    header: t('Disabled Reason'),
                    className: 'min-w-[200px]',
                    cellClassName: 'max-w-xs truncate text-sm',
                    cell: (key) => key.reason || '-',
                  },
                  {
                    id: 'disabled-time',
                    header: t('Disabled Time'),
                    className: 'w-44',
                    cellClassName: 'text-muted-foreground text-sm',
                    cell: (key) => formatKeyTimestamp(key.disabled_time),
                  },
                  {
                    id: 'actions',
                    header: t('Actions'),
                    className: 'text-right',
                    cell: (key) => (
                      <MultiKeyTableRowActions
                        keyIndex={key.index}
                        status={key.status}
                        canDelete={canEditSensitive}
                        probeBusy={probingIndex !== null || probingAll}
                        isProbing={probingAll || probingIndex === key.index}
                        onAction={setConfirmAction}
                        onProbe={handleTestKey}
                      />
                    ),
                  },
                ]}
              />
            )}
          </div>

          {/* Pagination */}
          {totalPages > 1 && (
            <div className='flex shrink-0 items-center justify-between'>
              <div className='text-muted-foreground text-sm'>
                {t('Page {{current}} of {{total}}', {
                  current: currentPage,
                  total: totalPages,
                })}
              </div>
              <div className='flex gap-2'>
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => handlePageChange(currentPage - 1)}
                  disabled={currentPage === 1 || isLoading}
                >
                  {t('Previous')}
                </Button>
                <Button
                  variant='outline'
                  size='sm'
                  onClick={() => handlePageChange(currentPage + 1)}
                  disabled={currentPage >= totalPages || isLoading}
                >
                  {t('Next')}
                </Button>
              </div>
            </div>
          )}
        </div>
      </Dialog>

      {/* Step-up verification for key disclosure (passkey / 2FA, no
          password fallback) */}
      <SecureVerificationDialog {...verification.dialogProps} />

      {/* Confirmation Dialog */}
      <ConfirmDialog
        open={confirmAction !== null}
        onOpenChange={(open) => !open && setConfirmAction(null)}
        title={t('Confirm Action')}
        desc={t(getMultiKeyConfirmMessage(confirmAction))}
        destructive={isDestructiveAction(confirmAction)}
        isLoading={isPerformingAction}
        handleConfirm={performAction}
      />
    </>
  )
}
