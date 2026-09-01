import QRCode from 'qrcode'

export type InviteKind = 'receive' | 'connect'

export function inviteKind(value: string): InviteKind | null {
  const trimmed = value.trim()
  if (trimmed.startsWith('whiskerlink://receive?')) return 'receive'
  if (trimmed.startsWith('whiskerlink://connect?')) return 'connect'
  return null
}

export function redactInvite(value: string): string {
  try {
    const parsed = new URL(value)
    const token = parsed.searchParams.get('token') || ''
    if (!token) return parsed.toString()
    const redacted = token.length > 16
      ? `${token.slice(0, 10)}…${token.slice(-5)}`
      : 'hidden'
    return value.replace(/([?&]token=)[^&]*/, `$1${redacted}`)
  } catch {
    return 'Invalid WhiskerLink invite'
  }
}

export async function createInviteQRCode(value: string): Promise<string | null> {
  if (!inviteKind(value) || new TextEncoder().encode(value).length > 2200) return null
  try {
    return await QRCode.toDataURL(value, {
      errorCorrectionLevel: 'M',
      margin: 2,
      width: 320,
      color: { dark: '#17211d', light: '#fafbf6' },
    })
  } catch {
    return null
  }
}
