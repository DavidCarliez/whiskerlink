import { describe, expect, test } from 'bun:test'
import { createInviteQRCode, inviteKind, redactInvite } from './invites'

const fileInvite = 'whiskerlink://receive?token=tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu&label=Release'

describe('invite helpers', () => {
  test('routes supported invite kinds', () => {
    expect(inviteKind(fileInvite)).toBe('receive')
    expect(inviteKind('whiskerlink://connect?token=tc&port=80&type=http')).toBe('connect')
    expect(inviteKind('https://example.com')).toBeNull()
  })

  test('redacts the capability token', () => {
    const redacted = redactInvite(fileInvite)
    expect(redacted).toContain('tcomFwWCCc…pGQEu')
    expect(redacted).not.toContain('tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu')
  })

  test('generates QR data locally and rejects oversized payloads', async () => {
    expect(await createInviteQRCode(fileInvite)).toStartWith('data:image/png;base64,')
    expect(await createInviteQRCode(`whiskerlink://receive?token=${'x'.repeat(2300)}`)).toBeNull()
  })
})
