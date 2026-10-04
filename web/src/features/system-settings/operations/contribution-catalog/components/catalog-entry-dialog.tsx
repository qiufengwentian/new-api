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
import { zodResolver } from '@hookform/resolvers/zod'
import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'
import { type Resolver, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { getChannels } from '@/features/channels/api'
import { CHANNEL_TYPE_OPTIONS } from '@/features/channels/constants'
import { getAdminPlans } from '@/features/subscriptions/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../../../components/settings-form-layout'
import {
  useCreateContributionEntry,
  useUpdateContributionEntry,
} from '../hooks/use-contribution-catalog-mutations'
import {
  getContributionEntryFormSchema,
  type AdminContributionEntry,
  type ContributionEntryFormValues,
} from '../types'

type CatalogEntryDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  entry: AdminContributionEntry | null
}

const ENTRY_FORM_ID = 'contribution-catalog-entry-form'

const EMPTY_ENTRY: ContributionEntryFormValues = {
  channel_type: CHANNEL_TYPE_OPTIONS[0].value,
  name: '',
  register_url: '',
  key_placeholder: '',
  host_channel_id: 0,
  plan_id: 0,
  enabled: false,
}

function entryFormValues(entry: AdminContributionEntry): ContributionEntryFormValues {
  return {
    channel_type: entry.channel_type,
    name: entry.name,
    register_url: entry.register_url,
    key_placeholder: entry.key_placeholder,
    host_channel_id: entry.host_channel_id,
    plan_id: entry.plan_id,
    enabled: entry.enabled,
  }
}

export function CatalogEntryDialog(props: CatalogEntryDialogProps) {
  const { t } = useTranslation()
  const isEditing = props.entry !== null
  const createEntry = useCreateContributionEntry()
  const updateEntry = useUpdateContributionEntry()
  const channelsQuery = useQuery({
    queryKey: ['contribution-multi-key-channels'],
    queryFn: async () => {
      const result = requireServerSuccess(await getChannels({ p: 1, page_size: 100 }))
      return (result.data?.items ?? []).filter(
        (channel) => channel.channel_info.is_multi_key
      )
    },
  })
  const plansQuery = useQuery({
    queryKey: ['admin-subscription-plans'],
    queryFn: async () => {
      const result = requireServerSuccess(await getAdminPlans())
      return result.data ?? []
    },
  })
  const channels = channelsQuery.data ?? []
  const plans = plansQuery.data ?? []

  const schema = getContributionEntryFormSchema(t)
  const form = useForm<ContributionEntryFormValues>({
    resolver: zodResolver(schema) as unknown as Resolver<ContributionEntryFormValues>,
    defaultValues: EMPTY_ENTRY,
  })

  useEffect(() => {
    if (!props.open) {
      return
    }
    form.reset(props.entry ? entryFormValues(props.entry) : EMPTY_ENTRY)
  }, [props.open, props.entry, form])

  const onSubmit = async (values: ContributionEntryFormValues) => {
    if (isEditing && props.entry) {
      const res = await updateEntry.mutateAsync({ ...values, id: props.entry.id })
      if (res.success) {
        props.onOpenChange(false)
      }
      return
    }
    const res = await createEntry.mutateAsync(values)
    if (res.success) {
      props.onOpenChange(false)
    }
  }

  const isPending = createEntry.isPending || updateEntry.isPending

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={isEditing ? t('Edit upstream') : t('Add upstream')}
      description={t(
        'A contributable upstream binds one channel type to the multi-key host channel and reward plan that receive user keys.'
      )}
      contentClassName='max-h-[min(85dvh,var(--dialog-available-height))] overflow-y-auto sm:max-w-2xl'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={isPending}
          >
            {t('Cancel')}
          </Button>
          <Button type='submit' form={ENTRY_FORM_ID} disabled={isPending}>
            {isPending ? t('Saving...') : t('Save')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <SettingsForm id={ENTRY_FORM_ID} onSubmit={form.handleSubmit(onSubmit)}>
          <FormField
            control={form.control}
            name='channel_type'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Channel Type')}</FormLabel>
                <Select
                  items={CHANNEL_TYPE_OPTIONS}
                  value={String(field.value)}
                  onValueChange={(value) => field.onChange(Number(value))}
                >
                  <FormControl>
                    <SelectTrigger className='w-full'>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      {CHANNEL_TYPE_OPTIONS.map((option) => (
                        <SelectItem key={option.value} value={String(option.value)}>
                          {t(option.label)}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
                <FormDescription>
                  {t('One enabled entry per channel type; the reward unit is the channel type.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='name'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Display Name')}</FormLabel>
                <FormControl>
                  <Input placeholder='OpenAI' {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='register_url'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Register URL')}</FormLabel>
                <FormControl>
                  <Input
                    placeholder='https://platform.openai.com/signup'
                    {...field}
                  />
                </FormControl>
                <FormDescription>
                  {t('Shown to users so they can create an upstream account first.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='key_placeholder'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Key Placeholder')}</FormLabel>
                <FormControl>
                  <Input placeholder='sk-...' {...field} />
                </FormControl>
                <FormDescription>
                  {t('Hint shown inside the key input on the user page.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='host_channel_id'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Host Channel')}</FormLabel>
                <Select
                  items={channels.map((channel) => ({
                    value: String(channel.id),
                    label: `${channel.name} (#${channel.id})`,
                  }))}
                  value={field.value > 0 ? String(field.value) : null}
                  onValueChange={(value) => field.onChange(Number(value))}
                >
                  <FormControl>
                    <SelectTrigger className='w-full'>
                      <SelectValue placeholder={t('Select a multi-key channel')} />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      {channels.map((channel) => (
                        <SelectItem key={channel.id} value={String(channel.id)}>
                          {`${channel.name} (#${channel.id})`}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
                <FormDescription>
                  {t('The multi-key channel that receives contributed keys.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='plan_id'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Subscription Plan')}</FormLabel>
                <Select
                  items={plans.map((record) => ({
                    value: String(record.plan.id),
                    label: record.plan.title,
                  }))}
                  value={field.value > 0 ? String(field.value) : null}
                  onValueChange={(value) => field.onChange(Number(value))}
                >
                  <FormControl>
                    <SelectTrigger className='w-full'>
                      <SelectValue placeholder={t('Select a subscription plan')} />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      {plans.map((record) => (
                        <SelectItem key={record.plan.id} value={String(record.plan.id)}>
                          {record.plan.title}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
                <FormDescription>
                  {t('Plan granted to the contributor as a reward.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Enabled')}</FormLabel>
                  <FormDescription>
                    {t('Users can pick this upstream while it is enabled.')}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    size='sm'
                    checked={field.value}
                    onCheckedChange={(value) => field.onChange(value === true)}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />
        </SettingsForm>
      </Form>
    </Dialog>
  )
}
