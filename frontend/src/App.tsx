import { useEffect, useLayoutEffect, useMemo, useState } from 'react'
import { Browser, Dialogs, Events } from '@wailsio/runtime'
import * as API from '../bindings/github.com/DavidCarliez/whiskerlink/appservice.js'
import { createInviteQRCode, inviteKind, redactInvite } from './invites'
import './App.css'

type View = 'home' | 'send' | 'receive' | 'share' | 'connect' | 'activity' | 'devices'
type Theme = 'light' | 'dark'


type Session = {
  id: string; kind: string; label: string; state: string; token?: string; invite?: string; localAddress?: string
  remotePort?: number; serviceType?: ServiceType; transport?: string; latencyMs?: number; persistent: boolean
  cliCompatible: boolean; createdAt: string; error?: string
}
type Transfer = {
  id: string; direction: 'send' | 'receive'; label: string; state: string; destination?: string
  filesTotal: number; filesCompleted: number; bytesTotal: number; bytesTransferred: number
  error?: string; createdAt: string; updatedAt: string
}
type Device = { id: string; name: string; tokenHint: string; createdAt: string }
type FileEntry = { path: string; size: number; modTimeUnixNano: number; sha256: string }
type Manifest = { protocolVersion: number; transferId: string; label: string; files: FileEntry[]; totalBytes: number; cliCompatible: boolean }
type Snapshot = { sessions: Session[]; transfers: Transfer[]; trustedDevices: Device[] }
type ServiceType = 'http' | 'https' | 'tcp'
type ServiceInvite = { token: string; remotePort: number; label: string; serviceType: ServiceType }
type FileInvite = { token: string; label: string }

const emptySnapshot: Snapshot = { sessions: [], transfers: [], trustedDevices: [] }

function normalizeSnapshot(value: unknown): Snapshot {
  const candidate = typeof value === 'object' && value !== null
    ? value as Record<string, unknown>
    : {}
  return {
    sessions: Array.isArray(candidate.sessions) ? candidate.sessions as Session[] : [],
    transfers: Array.isArray(candidate.transfers) ? candidate.transfers as Transfer[] : [],
    trustedDevices: Array.isArray(candidate.trustedDevices) ? candidate.trustedDevices as Device[] : [],
  }
}
const nav: { id: View; label: string; mark: string }[] = [
  { id: 'home', label: 'Overview', mark: '01' },
  { id: 'send', label: 'Send files', mark: '02' },
  { id: 'receive', label: 'Receive files', mark: '03' },
  { id: 'share', label: 'Share service', mark: '04' },
  { id: 'connect', label: 'Connect service', mark: '05' },
  { id: 'activity', label: 'Activity', mark: '06' },
  { id: 'devices', label: 'Trusted devices', mark: '07' },
]

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / Math.pow(1024, index)
  return `${value >= 10 || index === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`
}

function progress(transfer: Transfer): number {
  if (transfer.state === 'completed') return 100
  if (!transfer.bytesTotal) return 0
  return Math.min(100, Math.round((transfer.bytesTransferred / transfer.bytesTotal) * 100))
}

function errorText(error: unknown): string {
  if (error instanceof Error) return error.message
  if (typeof error === 'string') return error
  return 'The operation failed.'
}

function localServiceURL(address: string, serviceType?: ServiceType): string | null {
  if (serviceType !== 'http' && serviceType !== 'https') return null
  return `${serviceType}://${address}/`
}

function App() {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      const saved = window.localStorage.getItem('whiskerlink-theme')
      if (saved === 'light' || saved === 'dark') return saved
    } catch {
      // The system preference remains a safe fallback when storage is unavailable.
    }
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  })
  useLayoutEffect(() => {
    document.documentElement.dataset.theme = theme
    document.documentElement.style.colorScheme = theme
    try {
      window.localStorage.setItem('whiskerlink-theme', theme)
    } catch {
      // Theme still applies for this session when storage is unavailable.
    }
  }, [theme])

  const [view, setView] = useState<View>('home')
  const [snapshot, setSnapshot] = useState<Snapshot>(emptySnapshot)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'ok' | 'error'; text: string } | null>(null)

  const [sendPaths, setSendPaths] = useState<string[]>([])
  const [sendLabel, setSendLabel] = useState('')
  const [sendPersistent, setSendPersistent] = useState(false)

  const [receiveToken, setReceiveToken] = useState('')
  const [receiveDevice, setReceiveDevice] = useState('')
  const [manifest, setManifest] = useState<Manifest | null>(null)
  const [selectedFiles, setSelectedFiles] = useState<string[]>([])
  const [destination, setDestination] = useState('')
  const [collision, setCollision] = useState('rename')

  const [shareLabel, setShareLabel] = useState('Development server')
  const [shareLocalPort, setShareLocalPort] = useState(3000)
  const [shareRemotePort, setShareRemotePort] = useState(3000)
  const [sharePersistent, setSharePersistent] = useState(false)
  const [shareServiceType, setShareServiceType] = useState<ServiceType>('http')

  const [connectLabel, setConnectLabel] = useState('Remote service')
  const [connectToken, setConnectToken] = useState('')
  const [connectDevice, setConnectDevice] = useState('')
  const [connectRemotePort, setConnectRemotePort] = useState(3000)
  const [connectLocalPort, setConnectLocalPort] = useState(0)
  const [connectServiceType, setConnectServiceType] = useState<ServiceType>('http')
  const [connectAutoOpen, setConnectAutoOpen] = useState(true)

  const [deviceName, setDeviceName] = useState('')
  const [deviceToken, setDeviceToken] = useState('')

  const refresh = async () => setSnapshot(normalizeSnapshot(await API.Snapshot()))

  const applyServiceInvite = async (value: string) => {
    try {
      const invite = await API.ParseServiceInvite(value) as ServiceInvite
      setConnectDevice('')
      setConnectToken(invite.token)
      setConnectRemotePort(invite.remotePort)
      setConnectLabel(invite.label || 'Remote service')
      setConnectServiceType(invite.serviceType)
      setConnectLocalPort(0)
      setView('connect')
      setNotice({ kind: 'ok', text: 'Service invite loaded. A free local port will be selected.' })
    } catch (error) {
      setNotice({ kind: 'error', text: errorText(error) })
    }
  }

  const applyFileInvite = async (value: string) => {
    setBusy(true)
    try {
      const invite = await API.ParseFileInvite(value) as FileInvite
      setReceiveDevice('')
      setReceiveToken(invite.token)
      setManifest(null)
      setView('receive')
      const result = await API.InspectFileOffer(invite.token) as Manifest
      setManifest(result)
      setSelectedFiles(result.files.map((file) => file.path))
      setNotice({ kind: 'ok', text: 'File invite verified. Choose a destination before accepting.' })
    } catch (error) {
      setNotice({ kind: 'error', text: errorText(error) })
    } finally {
      setBusy(false)
    }
  }

  const applyInvite = async (value: string) => {
    const kind = inviteKind(value)
    if (kind === 'receive') {
      await applyFileInvite(value)
      return
    }
    if (kind === 'connect') {
      await applyServiceInvite(value)
      return
    }
    setNotice({ kind: 'error', text: 'Clipboard does not contain a supported WhiskerLink invite.' })
  }

  const pasteFromClipboard = async (target: 'receive' | 'connect') => {
    try {
      const value = (await navigator.clipboard.readText()).trim()
      if (!value) {
        setNotice({ kind: 'error', text: 'Clipboard is empty.' })
        return
      }
      if (inviteKind(value)) {
        await applyInvite(value)
        return
      }
      if (target === 'receive') {
        setReceiveDevice('')
        setReceiveToken(value)
        setManifest(null)
      } else {
        setConnectDevice('')
        setConnectToken(value)
      }
      setNotice({ kind: 'ok', text: 'Tailcat token pasted.' })
    } catch (error) {
      setNotice({ kind: 'error', text: `Clipboard could not be read: ${errorText(error)}` })
    }
  }

  const takePendingInvite = async () => {
    const value = await API.TakePendingInvite()
    if (value) await applyInvite(value)
  }
  useEffect(() => {
    refresh().catch((error) => setNotice({ kind: 'error', text: errorText(error) }))
    return Events.On('snapshot', (event) => setSnapshot(normalizeSnapshot(event.data)))
  }, [])

  useEffect(() => {
    const unsubscribe = Events.On('invite', () => { void takePendingInvite() })
    void takePendingInvite()
    return unsubscribe
  }, [])

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(null), 5200)
    return () => window.clearTimeout(timer)
  }, [notice])

  const run = async <T,>(operation: () => Promise<T>, success: string): Promise<T | undefined> => {
    setBusy(true)
    try {
      const result = await operation()
      await refresh()
      setNotice({ kind: 'ok', text: success })
      return result
    } catch (error) {
      setNotice({ kind: 'error', text: errorText(error) })
    } finally {
      setBusy(false)
    }
  }

  const chooseSendFiles = async () => {
    const result = await Dialogs.OpenFile({
      Title: 'Choose files to send', CanChooseFiles: true, CanChooseDirectories: false,
      AllowsMultipleSelection: true, ShowHiddenFiles: false,
    })
    if (result.length) setSendPaths((current) => [...new Set([...current, ...result])])
  }

  const chooseSendFolder = async () => {
    const result = await Dialogs.OpenFile({
      Title: 'Choose a folder to send', CanChooseFiles: false, CanChooseDirectories: true,
      AllowsMultipleSelection: false, ShowHiddenFiles: false,
    })
    if (result) setSendPaths((current) => [...new Set([...current, result])])
  }

  const chooseDestination = async () => {
    const result = await Dialogs.OpenFile({
      Title: 'Choose destination folder', CanChooseFiles: false, CanChooseDirectories: true,
      AllowsMultipleSelection: false,
    })
    if (result) setDestination(result)
  }

  const startFileOffer = async () => {
    const session = await run(() => API.StartFileOffer({ paths: sendPaths, label: sendLabel, persistent: sendPersistent }), 'File offer is ready to share.') as Session | undefined
    if (session) {
      setView('activity')
      setSendPaths([])
      setSendLabel('')
    }
  }

  const inspectOffer = async () => {
    const result = await run(
      () => receiveDevice ? API.InspectTrustedFileOffer(receiveDevice) : API.InspectFileOffer(receiveToken),
      'File offer verified.',
    ) as Manifest | undefined
    if (result) {
      setManifest(result)
      setSelectedFiles(result.files.map((file) => file.path))
    }
  }

  const receiveFiles = async () => {
    if (!manifest) return
    const request = { token: receiveToken, destination, selected: selectedFiles, collision }
    const result = await run(
      () => receiveDevice ? API.ReceiveTrustedFiles(receiveDevice, request) : API.ReceiveFiles(request),
      'Transfer added to the queue.',
    )
    if (result) {
      setManifest(null)
      setView('activity')
    }
  }

  const connectService = async () => {
    const session = await run(
      () => connectDevice
        ? API.ConnectTrustedService(connectDevice, connectLabel, connectRemotePort, connectLocalPort, connectServiceType)
        : API.ConnectService({
            label: connectLabel,
            token: connectToken,
            remotePort: connectRemotePort,
            localPort: connectLocalPort,
            serviceType: connectServiceType,
          }),
      'Local service link started.',
    ) as Session | undefined
    if (!session) return
    setView('activity')
    const url = session.localAddress ? localServiceURL(session.localAddress, session.serviceType) : null
    if (connectAutoOpen && url) {
      try {
        await Browser.OpenURL(url)
      } catch (error) {
        setNotice({ kind: 'error', text: `Service connected, but the browser could not open: ${errorText(error)}` })
      }
    }
  }

  const activeTransfers = snapshot.transfers.filter((item) => !['completed', 'failed'].includes(item.state))
  const latest = useMemo(() => snapshot.transfers.slice(0, 4), [snapshot.transfers])

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand"><span className="brand-cut">WL</span><div><strong>WhiskerLink</strong><small>private exchange</small></div></div>
        <nav>{nav.map((item) => <button key={item.id} className={view === item.id ? 'active' : ''} onClick={() => setView(item.id)}><span>{item.mark}</span>{item.label}</button>)}</nav>
        <div className="sidebar-foot"><span className="status-dot" />Local core ready<small>{snapshot.sessions.length} live sessions</small></div>
      </aside>

      <main>
        <header>
          <div><p className="eyebrow">ACCOUNT-FREE / END-TO-END ENCRYPTED</p><h1>{nav.find((item) => item.id === view)?.label}</h1></div>
          <button
            className="theme-toggle"
            aria-label={`Use ${theme === 'dark' ? 'light' : 'dark'} theme`}
            title={`Use ${theme === 'dark' ? 'light' : 'dark'} theme`}
            onClick={() => setTheme((current) => current === 'dark' ? 'light' : 'dark')}
          >
            {theme === 'dark'
              ? <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3.5" /><path d="M12 2v2.2M12 19.8V22M4.9 4.9l1.6 1.6m11 11 1.6 1.6M2 12h2.2M19.8 12H22M4.9 19.1l1.6-1.6m11-11 1.6-1.6" /></svg>
              : <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20.2 15.5A8.5 8.5 0 0 1 8.5 3.8 8.5 8.5 0 1 0 20.2 15.5Z" /></svg>}
            <span>{theme === 'dark' ? 'Light' : 'Dark'}</span>
          </button>
        </header>

        {view === 'home' && <section className="page">
          <div className="hero"><div><p className="eyebrow">PRIVATE PATHS, ON DEMAND</p><h2>Move files. Open services.<br />Leave no network behind.</h2><p>WhiskerLink creates temporary WireGuard paths through Tailcat without accounts, firewall rules, or device enrollment.</p><div className="actions"><button className="primary" onClick={() => setView('send')}>Send files</button><button onClick={() => setView('share')}>Share a service</button></div></div><div className="signal"><span>TAILCAT LINK</span><strong>{snapshot.sessions.length ? 'LIVE' : 'IDLE'}</strong><i /><small>Direct path when possible<br />DERP fallback when needed</small></div></div>
          <div className="metrics"><article><span>ACTIVE SESSIONS</span><strong>{snapshot.sessions.length}</strong><small>Temporary and trusted</small></article><article><span>TRANSFER QUEUE</span><strong>{activeTransfers.length}</strong><small>{formatBytes(activeTransfers.reduce((n, t) => n + Math.max(0, t.bytesTotal - t.bytesTransferred), 0))} remaining</small></article><article><span>TRUSTED DEVICES</span><strong>{snapshot.trustedDevices.length}</strong><small>Tokens held by OS credentials</small></article></div>
          <SectionTitle title="Recent activity" action="View all" onAction={() => setView('activity')} />
          <TransferList transfers={latest} onPause={(id) => run(() => API.PauseTransfer(id), 'Transfer paused.')} onResume={(id) => run(() => API.ResumeTransfer(id), 'Transfer queued to resume.')} />
        </section>}

        {view === 'send' && <section className="page narrow"><Intro number="01" title="Offer files directly" text="The sender hosts an ephemeral encrypted offer. The recipient inspects it, chooses what to accept, and pulls files directly or through DERP." />
          <div className="panel form-panel"><label>Transfer label<input value={sendLabel} onChange={(e) => setSendLabel(e.target.value)} placeholder="Release artifacts" /></label><label>Files and folders<div className="picker"><button onClick={chooseSendFiles}>Choose files</button><button onClick={chooseSendFolder}>Choose folder</button><span>{sendPaths.length ? `${sendPaths.length} selected` : 'Nothing selected yet'}</span></div></label>{sendPaths.length > 0 && <div className="path-list">{sendPaths.map((path) => <span key={path}>{path}</span>)}</div>}<label className="switch-row"><input type="checkbox" checked={sendPersistent} onChange={(e) => setSendPersistent(e.target.checked)} /><span><strong>Use trusted host identity</strong><small>Keeps the same Tailcat identity in the OS credential store.</small></span></label><button className="primary wide" disabled={busy || !sendPaths.length} onClick={startFileOffer}>Create file offer</button></div>
        </section>}

        {view === 'receive' && <section className="page narrow"><Intro number="02" title="Inspect before receiving" text="Connection metadata is validated before the manifest is shown. Nothing is written until you accept the offer." />
          <div className="panel form-panel">
            <label>Source<select value={receiveDevice} onChange={(e) => { setReceiveDevice(e.target.value); setManifest(null) }}><option value="">WhiskerLink invite or Tailcat token</option>{snapshot.trustedDevices.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select></label>
            {!receiveDevice && <label>File invite or Tailcat token<div className="invite-input"><textarea value={receiveToken} onChange={(e) => { setReceiveToken(e.target.value); setManifest(null) }} onPaste={(event) => { const value = event.clipboardData.getData('text').trim(); if (value.startsWith('whiskerlink://')) { event.preventDefault(); void applyInvite(value) } }} placeholder="whiskerlink://receive?… or tc…" rows={3} /><button type="button" onClick={() => void pasteFromClipboard('receive')}>Paste</button></div><small>WhiskerLink invites inspect the manifest automatically.</small></label>}
            <button disabled={busy || (!receiveDevice && !receiveToken.trim())} onClick={inspectOffer}>Inspect offer</button>
          {manifest && <div className="manifest"><div><strong>{manifest.label}</strong><small>{manifest.files.length} files · {formatBytes(manifest.totalBytes)}</small></div><div className="manifest-files">{manifest.files.map((file) => <label key={file.path}><input type="checkbox" checked={selectedFiles.includes(file.path)} onChange={() => setSelectedFiles((current) => current.includes(file.path) ? current.filter((p) => p !== file.path) : [...current, file.path])} /><span>{file.path}</span><small>{formatBytes(file.size)}</small></label>)}</div><label>Destination<div className="picker"><button onClick={chooseDestination}>Choose folder</button><span>{destination || 'No destination selected'}</span></div></label><label>Existing files<select value={collision} onChange={(e) => setCollision(e.target.value)}><option value="rename">Keep both</option><option value="overwrite">Replace after verification</option><option value="skip">Skip existing</option></select></label><button className="primary wide" disabled={busy || !destination || !selectedFiles.length} onClick={receiveFiles}>Accept selected files</button></div>}</div>
        </section>}

        {view === 'share' && <section className="page narrow">
          <Intro number="03" title="Share one local service" text="Expose a local TCP service through Tailcat. Only the selected remote port is admitted; your operating system routes remain untouched." />
          <div className="panel form-panel">
            <label>Session label<input value={shareLabel} onChange={(e) => setShareLabel(e.target.value)} /></label>
            <label>Service type<select value={shareServiceType} onChange={(e) => setShareServiceType(e.target.value as ServiceType)}><option value="http">HTTP website or API</option><option value="https">HTTPS website or API</option><option value="tcp">Other TCP service</option></select></label>
            <div className="field-grid">
              <label>Local port<input type="number" min="1" max="65535" value={shareLocalPort} onChange={(e) => setShareLocalPort(Number(e.target.value))} /></label>
              <label>Remote port<input type="number" min="1" max="65535" value={shareRemotePort} onChange={(e) => setShareRemotePort(Number(e.target.value))} /></label>
            </div>
            <label className="switch-row"><input type="checkbox" checked={sharePersistent} onChange={(e) => setSharePersistent(e.target.checked)} /><span><strong>Persistent trusted identity</strong><small>Anyone previously given this address can reconnect unless access is rotated.</small></span></label>
            <button className="primary wide" disabled={busy} onClick={() => run(() => API.ShareService({ label: shareLabel, localHost: '127.0.0.1', localPort: shareLocalPort, remotePort: shareRemotePort, persistent: sharePersistent, serviceType: shareServiceType }), 'Private service is listening.').then((result) => result && setView('activity'))}>Start sharing</button>
          </div>
        </section>}

        {view === 'connect' && <section className="page narrow">
          <Intro number="04" title="Open a remote service locally" text="Paste one WhiskerLink invite. A loopback listener maps browser, database, and IDE traffic through Tailcat." />
          <div className="panel form-panel">
            <label>Source<select value={connectDevice} onChange={(e) => setConnectDevice(e.target.value)}><option value="">WhiskerLink invite or Tailcat token</option>{snapshot.trustedDevices.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select></label>
            {!connectDevice && <label>Service invite or Tailcat token<div className="invite-input"><textarea value={connectToken} onChange={(e) => setConnectToken(e.target.value)} onPaste={(event) => { const value = event.clipboardData.getData('text').trim(); if (value.startsWith('whiskerlink://')) { event.preventDefault(); void applyInvite(value) } }} placeholder="whiskerlink://connect?… or tc…" rows={3} /><button type="button" onClick={() => void pasteFromClipboard('connect')}>Paste</button></div><small>WhiskerLink invites fill the remaining fields automatically.</small></label>}
            <label>Connection label<input value={connectLabel} onChange={(e) => setConnectLabel(e.target.value)} /></label>
            <label>Service type<select value={connectServiceType} onChange={(e) => setConnectServiceType(e.target.value as ServiceType)}><option value="http">HTTP website or API</option><option value="https">HTTPS website or API</option><option value="tcp">Other TCP service</option></select></label>
            <div className="field-grid">
              <label>Remote port<input type="number" min="1" max="65535" value={connectRemotePort} onChange={(e) => setConnectRemotePort(Number(e.target.value))} /></label>
              <label>Local port <small>0 chooses a free port</small><input type="number" min="0" max="65535" value={connectLocalPort} onChange={(e) => setConnectLocalPort(Number(e.target.value))} /></label>
            </div>
            {(connectServiceType === 'http' || connectServiceType === 'https') && <label className="switch-row"><input type="checkbox" checked={connectAutoOpen} onChange={(e) => setConnectAutoOpen(e.target.checked)} /><span><strong>Open in browser after connecting</strong><small>Uses the selected free localhost port.</small></span></label>}
            <button className="primary wide" disabled={busy || (!connectDevice && !connectToken.trim())} onClick={connectService}>Connect locally</button>
          </div>
        </section>}

        {view === 'activity' && <section className="page"><SectionTitle title="Live sessions" /><div className="session-grid">{snapshot.sessions.length ? snapshot.sessions.map((session) => <SessionCard key={session.id} session={session} onCopied={(text) => setNotice({ kind: 'ok', text })} onError={(text) => setNotice({ kind: 'error', text })} onStop={(id) => run(() => API.StopSession(id), 'Session stopped.')} />) : <Empty text="No live sessions. Start a file offer or share a service." />}</div><SectionTitle title="Transfer history" /><TransferList transfers={snapshot.transfers} onPause={(id) => run(() => API.PauseTransfer(id), 'Transfer paused.')} onResume={(id) => run(() => API.ResumeTransfer(id), 'Transfer queued to resume.')} /></section>}

        {view === 'devices' && <section className="page narrow"><Intro number="05" title="Name a persistent endpoint" text="The complete capability token is stored in the operating system credential manager. The database keeps only a redacted hint." /><div className="panel form-panel"><label>Device name<input value={deviceName} onChange={(e) => setDeviceName(e.target.value)} placeholder="Home workstation" /></label><label>Persistent Tailcat token<textarea value={deviceToken} onChange={(e) => setDeviceToken(e.target.value)} rows={3} placeholder="tc…" /></label><button className="primary wide" disabled={busy || !deviceName || !deviceToken} onClick={() => run(() => API.AddTrustedDevice(deviceName, deviceToken), 'Trusted device saved.').then((result) => { if (result) { setDeviceName(''); setDeviceToken('') } })}>Save trusted device</button></div><div className="device-list">{snapshot.trustedDevices.map((device) => <article key={device.id}><span className="device-mark">{device.name.slice(0, 2).toUpperCase()}</span><div><strong>{device.name}</strong><code>{device.tokenHint}</code></div><button className="quiet" onClick={() => run(() => API.RemoveTrustedDevice(device.id), 'Trusted device removed.')}>Remove</button></article>)}</div></section>}
      </main>
      {notice && <div className={`toast ${notice.kind}`}>{notice.text}</div>}
      {busy && <div className="busy-line" />}
    </div>
  )
}

type ReceiverPlatform = 'windows' | 'linux' | 'macos'
const platformLabels: Record<ReceiverPlatform, string> = {
  windows: 'Windows',
  linux: 'Linux',
  macos: 'macOS',
}

function PlatformIcon({ platform }: { platform: ReceiverPlatform }) {
  if (platform === 'windows') {
    return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 4.5 10.5 3.4v7.7H3V4.5Zm8.5-1.2L21 2v9.1h-9.5V3.3ZM3 12.1h7.5v7.7L3 18.7v-6.6Zm8.5 0H21V21l-9.5-1.1v-7.8Z" /></svg>
  }
  if (platform === 'linux') {
    return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.1c-2.8 0-4.4 2.5-4.4 5.9 0 1.2-.4 2.2-1.2 3.4-1.1 1.6-1.8 3.2-1.2 4.6.4.9 1.3 1.4 2.5 1.4.9 0 1.7-.3 2.4-.8.6.4 1.2.6 1.9.6s1.4-.2 1.9-.6c.7.5 1.5.8 2.4.8 1.2 0 2.1-.5 2.5-1.4.6-1.4-.1-3-1.2-4.6-.8-1.2-1.2-2.2-1.2-3.4 0-3.4-1.6-5.9-4.4-5.9Zm-1.5 4.1c-.5 0-.9-.5-.9-1.1s.4-1.1.9-1.1.9.5.9 1.1-.4 1.1-.9 1.1Zm3 0c-.5 0-.9-.5-.9-1.1s.4-1.1.9-1.1.9.5.9 1.1-.4 1.1-.9 1.1Zm-3.3 2.1c.5-.5 1.1-.8 1.8-.8s1.3.3 1.8.8c-.5.8-1.1 1.2-1.8 1.2s-1.3-.4-1.8-1.2Z" /></svg>
  }
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M15.8 12.7c0-2 1.6-3 1.7-3.1-1-.1-2.1.6-2.6.6-.6 0-1.4-.6-2.3-.6-1.2 0-2.3.7-2.9 1.8-1.3 2.2-.3 5.5.9 7.2.6.8 1.3 1.8 2.2 1.7.9 0 1.2-.5 2.3-.5s1.4.5 2.3.5c1 0 1.6-.9 2.1-1.7.7-1 1-2 1-2.1-.1 0-1.9-.7-1.9-2.9 0-1.8 1.5-2.7 1.6-2.8-1-.9-2.4-1-2.9-1-.8 0-1.5.4-2 .4Zm1-5.2c.5-.6.8-1.5.7-2.3-.8 0-1.7.5-2.2 1.1-.5.5-.9 1.4-.8 2.2.9.1 1.7-.4 2.3-1Z" /></svg>
}

function PlatformPicker({ value, onChange, label }: { value: ReceiverPlatform; onChange: (platform: ReceiverPlatform) => void; label: string }) {
  const platforms: ReceiverPlatform[] = ['windows', 'linux', 'macos']
  return <div className="platform-picker" role="group" aria-label={label}>
    {platforms.map((platform) => <button
      key={platform}
      type="button"
      className={value === platform ? 'active' : ''}
      aria-label={platformLabels[platform]}
      aria-pressed={value === platform}
      title={platformLabels[platform]}
      onClick={() => onChange(platform)}
    ><PlatformIcon platform={platform} /><span>{platformLabels[platform]}</span></button>)}
  </div>
}


function receiverCommand(platform: ReceiverPlatform, token: string): string {
  if (platform === 'windows') {
    return `.\\tailcat.exe cp -r "${token}:." .`
  }
  if (platform === 'linux') {
    return `./tailcat cp -r '${token}:.' .`
  }
  return `tailcat cp -r '${token}:.' .`
}

function SessionCard({ session, onCopied, onError, onStop }: { session: Session; onCopied: (text: string) => void; onError: (text: string) => void; onStop: (id: string) => void }) {
  const serviceURL = session.kind === 'service-link' && session.localAddress
    ? localServiceURL(session.localAddress, session.serviceType)
    : null
  return <article className="session">
    <div className="session-head"><span className={`state ${session.state}`}>{session.state}</span><small>{session.kind.replace('-', ' ')}</small></div>
    <h3>{session.label}</h3>
    {session.localAddress && <code>{session.localAddress}</code>}
    {session.invite
      ? <InviteSharePanel session={session} onCopied={onCopied} onError={onError} />
      : session.token && <details className="token-access"><summary>Advanced token access</summary><button onClick={() => navigator.clipboard.writeText(session.token || '').then(() => onCopied('Token copied to clipboard.')).catch((error) => onError(errorText(error)))}>Copy raw Tailcat token</button></details>}
    {serviceURL && <div className="receiver-guide">
      <div className="receiver-head"><div><strong>Local service ready</strong><small>{serviceURL}</small></div></div>
      <div className="receiver-command service-url"><code>{serviceURL}</code><button onClick={() => Browser.OpenURL(serviceURL).catch((error) => onError(errorText(error)))}>Open</button><button onClick={() => navigator.clipboard.writeText(serviceURL).then(() => onCopied('Local URL copied.'))}>Copy URL</button></div>
    </div>}
    <dl><div><dt>Transport</dt><dd>{session.transport || 'waiting'}</dd></div><div><dt>Remote port</dt><dd>{session.remotePort || '—'}</dd></div></dl>
    {session.error && <p className="inline-error">{session.error}</p>}
    <button className="danger" onClick={() => onStop(session.id)}>Stop session</button>
  </article>
}

function InviteSharePanel({ session, onCopied, onError }: { session: Session; onCopied: (text: string) => void; onError: (text: string) => void }) {
  const [open, setOpen] = useState(false)
  const [qrCode, setQRCode] = useState<string | null | undefined>()
  const invite = session.invite || ''

  useEffect(() => {
    let active = true
    if (!open || !invite) {
      setQRCode(undefined)
      return () => { active = false }
    }
    setQRCode(undefined)
    void createInviteQRCode(invite).then((value) => {
      if (active) setQRCode(value)
    })
    return () => { active = false }
  }, [open, invite])

  const description = session.kind === 'file-offer'
    ? 'File offer'
    : `${(session.serviceType || 'tcp').toUpperCase()} service on remote port ${session.remotePort || '—'}`
  const copyToClipboard = (value: string, notice: string) => {
    void navigator.clipboard.writeText(value)
      .then(() => onCopied(notice))
      .catch((error) => onError(errorText(error)))
  }

  return <div className={`invite-share ${open ? 'open' : ''}`}>
    <button className="wide invite-share-toggle" aria-expanded={open} onClick={() => setOpen((value) => !value)}>{open ? 'Hide invite' : 'Share invite'}</button>
    {open && <div className="invite-share-body">
      <div className="invite-qr">
        {qrCode === undefined && <span className="qr-placeholder">Generating QR…</span>}
        {qrCode === null && <span className="qr-placeholder">Invite is too large for a reliable QR code. Use Copy invite.</span>}
        {qrCode && <img src={qrCode} alt="WhiskerLink invite QR code" />}
      </div>
      <div className="invite-share-details">
        <p className="eyebrow">WHISKERLINK INVITE</p>
        <strong>{description}</strong>
        <small>Status: {session.state} · Active until stopped</small>
        <code>{redactInvite(invite)}</code>
        <div className="invite-share-actions">
          <button className="primary" onClick={() => copyToClipboard(invite, 'Invite copied to clipboard.')}>Copy invite</button>
        </div>
        <p className="invite-warning">This QR code and invite contain a capability secret. Share them only with the intended recipient.</p>
        {session.token && <details>
          <summary>Advanced</summary>
          <button onClick={() => copyToClipboard(session.token || '', 'Raw Tailcat token copied.')}>Copy raw Tailcat token</button>
          {session.kind === 'service-share' && session.remotePort && <ServiceClientGuide token={session.token} port={session.remotePort} onCopied={onCopied} />}
          {session.kind === 'file-offer' && <ReceiverGuide token={session.token} compatible={session.cliCompatible} onCopied={onCopied} />}
        </details>}
      </div>
    </div>}
  </div>
}

function ReceiverGuide({ token, compatible, onCopied }: { token: string; compatible: boolean; onCopied: (text: string) => void }) {
  const [platform, setPlatform] = useState<ReceiverPlatform>('linux')
  if (!compatible) {
    return <div className="receiver-guide unavailable"><strong>Receiver without the GUI</strong><p>CLI download requires one selected file or one selected folder. Create a separate offer for this selection.</p></div>
  }
  const command = receiverCommand(platform, token)
  const installHint = platform === 'windows'
    ? 'Download tailcat.exe from GitHub. Windows OpenSSH Client is also required.'
    : platform === 'macos' ? 'Install first with: brew install tailcat' : 'Extract the Linux release from GitHub. OpenSSH scp is also required.'
  return <div className="receiver-guide">
    <div className="receiver-head">
      <div><strong>Receiver without the GUI</strong><small>Open a terminal in the destination folder, then run this command.</small></div>
      <PlatformPicker value={platform} onChange={setPlatform} label="Receiver operating system" />
    </div>
    <div className="receiver-command"><code>{command}</code><button onClick={() => navigator.clipboard.writeText(command).then(() => onCopied('Receiver command copied.'))}>Copy command</button></div>
    <small>{installHint}</small>
    <small className="receiver-status-note">CLI downloads do not report progress back to this app. Keep the offer open until the receiver confirms completion, then stop the session.</small>
  </div>
}
function ServiceClientGuide({ token, port, onCopied }: { token: string; port: number; onCopied: (text: string) => void }) {
  const [platform, setPlatform] = useState<ReceiverPlatform>('linux')
  const binary = platform === 'windows' ? '.\\tailcat.exe' : platform === 'linux' ? './tailcat' : 'tailcat'
  const command = platform === 'windows'
    ? `${binary} socks --listen=127.0.0.1:1080 "${token}"`
    : `${binary} socks --listen=127.0.0.1:1080 '${token}'`
  const browserURL = `http://server.tailcat:${port}/`
  return <div className="receiver-guide">
    <div className="receiver-head">
      <div><strong>Client without the GUI</strong><small>Run a local SOCKS5 proxy for service port {port}.</small></div>
      <PlatformPicker value={platform} onChange={setPlatform} label="Client operating system" />
    </div>
    <div className="receiver-command"><code>{command}</code><button onClick={() => navigator.clipboard.writeText(command).then(() => onCopied('Proxy command copied.'))}>Copy proxy command</button></div>
    <small>Configure the client application to use SOCKS5 proxy <code>127.0.0.1:1080</code> and connect to <code>server.tailcat:{port}</code>.</small>
    <div className="receiver-command service-url"><code>{browserURL}</code><button onClick={() => navigator.clipboard.writeText(browserURL).then(() => onCopied('HTTP service URL copied.'))}>Copy HTTP URL</button></div>
    <small className="receiver-status-note">This does not create localhost:{port}. For an ordinary local port, use WhiskerLink's Connect service workflow.</small>
  </div>
}


function Intro({ number, title, text }: { number: string; title: string; text: string }) { return <div className="intro"><span>{number}</span><div><h2>{title}</h2><p>{text}</p></div></div> }
function SectionTitle({ title, action, onAction }: { title: string; action?: string; onAction?: () => void }) { return <div className="section-title"><h2>{title}</h2>{action && <button className="quiet" onClick={onAction}>{action}</button>}</div> }
function Empty({ text }: { text: string }) { return <div className="empty"><span>NO ACTIVE LINK</span><p>{text}</p></div> }
function TransferList({ transfers, onPause, onResume }: { transfers: Transfer[]; onPause: (id: string) => void; onResume: (id: string) => void }) {
  if (!transfers.length) return <Empty text="Transfers will appear here once you send or receive files." />
  return <div className="transfer-list">{transfers.map((item) => <article key={item.id}><span className={`direction ${item.direction}`}>{item.direction === 'send' ? 'OUT' : 'IN'}</span><div className="transfer-main"><div><strong>{item.label}</strong><small>{item.filesCompleted}/{item.filesTotal} files · {formatBytes(item.bytesTransferred)} of {formatBytes(item.bytesTotal)}</small></div><div className="bar"><i style={{ width: `${progress(item)}%` }} /></div>{item.error && <p className="inline-error">{item.error}</p>}</div><span className={`state ${item.state}`}>{item.state}</span>{['transferring', 'connecting', 'queued', 'verifying'].includes(item.state) && <button className="quiet" onClick={() => onPause(item.id)}>Pause</button>}{['paused', 'interrupted'].includes(item.state) && <button onClick={() => onResume(item.id)}>Resume</button>}</article>)}</div>
}

export default App
