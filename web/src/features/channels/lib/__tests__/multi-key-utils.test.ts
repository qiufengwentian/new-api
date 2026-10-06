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
import { describe, expect, it } from 'vitest'

import { splitChannelKeys } from '../multi-key-utils'

describe('splitChannelKeys', () => {
  it('splits the newline-separated key list used by multi-key channels', () => {
    expect(splitChannelKeys('sk-alpha\nsk-bravo\nsk-charlie')).toEqual([
      'sk-alpha',
      'sk-bravo',
      'sk-charlie',
    ])
  })

  it('trims edge newlines like the server does, keeping inner entries as stored', () => {
    expect(splitChannelKeys('\nsk-alpha\nsk-bravo\n')).toEqual([
      'sk-alpha',
      'sk-bravo',
    ])
  })

  it('parses a JSON-array key list into raw element values', () => {
    expect(splitChannelKeys('["sk-alpha","sk-bravo"]')).toEqual([
      '"sk-alpha"',
      '"sk-bravo"',
    ])
  })

  it('falls back to the newline split when the JSON array is invalid', () => {
    expect(splitChannelKeys('[sk-alpha')).toEqual(['[sk-alpha'])
  })

  it('returns an empty list for an empty key', () => {
    expect(splitChannelKeys('')).toEqual([])
  })
})
