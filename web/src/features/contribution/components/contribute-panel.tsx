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
import { ExternalLink, KeyRound } from 'lucide-react'
import { useState } from 'react'
import { type Resolver, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'

import { useSubmitContribution } from '../hooks/use-submit-contribution'
import {
  getContributionSubmitSchema,
  type ContributionCatalog,
  type ContributionSubmitValues,
} from '../types'
import { ContributionResult } from './contribution-result'

type ContributePanelProps = {
  catalog: ContributionCatalog
}

export function ContributePanel(props: ContributePanelProps) {
  const { t } = useTranslation()
  const entries = props.catalog.entries
  const firstEntry = entries[0]
  const firstChannelType = firstEntry ? firstEntry.channel_type : 0
  const [selectedChannelType, setSelectedChannelType] = useState(firstChannelType)
  const schema = getContributionSubmitSchema(t)
  const form = useForm<ContributionSubmitValues>({
    resolver: zodResolver(schema) as unknown as Resolver<ContributionSubmitValues>,
    defaultValues: { channel_type: firstChannelType, key: '', agreed: false },
  })
  const submitMutation = useSubmitContribution()
  const agreed = form.watch('agreed')
  const key = form.watch('key')
  const result = submitMutation.data?.success ? submitMutation.data.data : undefined
  const selected = entries.find(
    (entry) => entry.channel_type === selectedChannelType
  )

  if (entries.length === 0) {
    return (
      <EmptyState
        icon={KeyRound}
        bordered
        title={t('No upstream is open for contribution right now.')}
        description={t(
          'The administrator has not opened any upstream for contribution yet.'
        )}
      />
    )
  }

  const handleSelect = (value: string) => {
    const channelType = Number(value)
    setSelectedChannelType(channelType)
    form.setValue('channel_type', channelType, { shouldDirty: true })
  }

  const submitContribution = form.handleSubmit((values) => {
    submitMutation.mutate(values, {
      onSuccess: (response) => {
        if (response.success) {
          // The key is never echoed back, so the form is cleared right away.
          form.reset({ channel_type: values.channel_type, key: '', agreed: false })
        }
      },
    })
  })

  return (
    <div className='flex flex-col gap-4'>
      <Card>
        <CardHeader>
          <CardTitle>{t('Available upstreams')}</CardTitle>
          <CardDescription>
            {t('Pick the upstream you hold a key for.')}
          </CardDescription>
        </CardHeader>
        <CardContent className='flex flex-col gap-3'>
          <RadioGroup
            value={String(selectedChannelType)}
            onValueChange={handleSelect}
            aria-label={t('Available upstreams')}
          >
            {entries.map((entry) => {
              const itemId = `contribution-upstream-${entry.channel_type}`
              return (
                <Label
                  key={entry.channel_type}
                  htmlFor={itemId}
                  className='hover:border-primary/40 focus-within:border-primary/50 has-data-[checked]:border-primary has-data-[checked]:ring-primary/20 flex cursor-pointer items-center gap-3 rounded-lg border p-3 font-normal transition-all has-data-[checked]:ring-2'
                >
                  <RadioGroupItem
                    id={itemId}
                    value={String(entry.channel_type)}
                  />
                  <span className='text-sm font-medium'>{entry.name}</span>
                </Label>
              )
            })}
          </RadioGroup>
          {selected ? (
            <div className='flex flex-wrap items-center justify-between gap-2 rounded-lg border p-3'>
              <span className='text-muted-foreground text-sm'>
                {t('Registration link')}
              </span>
              <a
                href={selected.register_url}
                target='_blank'
                rel='noopener noreferrer'
                className='text-primary inline-flex items-center gap-1 text-sm hover:underline'
              >
                {t('Register an account')}
                <ExternalLink className='size-3.5' aria-hidden='true' />
              </a>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('Contribute an upstream key')}</CardTitle>
          <CardDescription>
            {t('One key per submission. It is never echoed back afterwards.')}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Form {...form}>
            <form
              onSubmit={submitContribution}
              className='flex flex-col gap-4'
            >
              <FormField
                control={form.control}
                name='key'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Upstream API Key')}</FormLabel>
                    <FormControl>
                      <Input
                        type='password'
                        autoComplete='off'
                        placeholder={
                          selected?.key_placeholder ||
                          t('Paste one upstream API key')
                        }
                        {...field}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <Alert>
                <AlertTitle>{t('Agreement')}</AlertTitle>
                <AlertDescription className='space-y-3 text-sm'>
                  <p>{t(props.catalog.agreement)}</p>
                  <div className='flex items-start gap-3'>
                    <Checkbox
                      id='contribution-consent'
                      checked={agreed}
                      onCheckedChange={(value) =>
                        form.setValue('agreed', value === true, {
                          shouldDirty: true,
                          shouldValidate: true,
                        })
                      }
                      className='mt-0.5'
                    />
                    <Label
                      htmlFor='contribution-consent'
                      className='items-start gap-1 text-left text-xs leading-5 font-normal'
                    >
                      {t('I have read and agree to the statement above')}
                    </Label>
                  </div>
                </AlertDescription>
              </Alert>

              <Button
                type='submit'
                disabled={
                  !agreed || key.trim() === '' || submitMutation.isPending
                }
              >
                {t('Contribute')}
              </Button>
            </form>
          </Form>
        </CardContent>
      </Card>

      {result ? (
        <ContributionResult result={result} />
      ) : null}
    </div>
  )
}
