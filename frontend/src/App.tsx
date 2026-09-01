import { useEffect, useMemo, useState } from 'react'
import { Dialogs, Events } from '@wailsio/runtime'
import * as API from '../bindings/github.com/DavidCarliez/whiskerlink/appservice.js'
import './App.css'

type View = 'home' | 'send' | 'receive' | 'share' | 'connect' | 'activity' | 'devices'

type Session = {
  id: string; kind: string; label: string; state: string; token?: string; localAddress?: string
  remotePort?: number; transport?: string; latencyMs?: number; persistent: boolean; cliCompatible: boolean
  createdAt: string; error?: string
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

function App() {
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

  const [connectLabel, setConnectLabel] = useState('Remote service')
  const [connectToken, setConnectToken] = useState('')
  const [connectDevice, setConnectDevice] = useState('')
  const [connectRemotePort, setConnectRemotePort] = useState(3000)
  const [connectLocalPort, setConnectLocalPort] = useState(0)

  const [deviceName, setDeviceName] = useState('')
  const [deviceToken, setDeviceToken] = useState('')

  const refresh = async () => setSnapshot(normalizeSnapshot(await API.Snapshot()))
  useEffect(() => {
    refresh().catch((error) => setNotice({ kind: 'error', text: errorText(error) }))
    return Events.On('snapshot', (event) => setSnapshot(normalizeSnapshot(event.data)))
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
        <header><div><p className="eyebrow">ACCOUNT-FREE / END-TO-END ENCRYPTED</p><h1>{nav.find((item) => item.id === view)?.label}</h1></div><button className="quiet" onClick={() => refresh()}>Refresh</button></header>

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
          <div className="panel form-panel"><label>Source<select value={receiveDevice} onChange={(e) => { setReceiveDevice(e.target.value); setManifest(null) }}><option value="">Temporary invite token</option>{snapshot.trustedDevices.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select></label>{!receiveDevice && <label>Tailcat invite<textarea value={receiveToken} onChange={(e) => { setReceiveToken(e.target.value); setManifest(null) }} placeholder="tc…" rows={3} /></label>}<button disabled={busy || (!receiveDevice && !receiveToken.trim())} onClick={inspectOffer}>Inspect offer</button>
          {manifest && <div className="manifest"><div><strong>{manifest.label}</strong><small>{manifest.files.length} files · {formatBytes(manifest.totalBytes)}</small></div><div className="manifest-files">{manifest.files.map((file) => <label key={file.path}><input type="checkbox" checked={selectedFiles.includes(file.path)} onChange={() => setSelectedFiles((current) => current.includes(file.path) ? current.filter((p) => p !== file.path) : [...current, file.path])} /><span>{file.path}</span><small>{formatBytes(file.size)}</small></label>)}</div><label>Destination<div className="picker"><button onClick={chooseDestination}>Choose folder</button><span>{destination || 'No destination selected'}</span></div></label><label>Existing files<select value={collision} onChange={(e) => setCollision(e.target.value)}><option value="rename">Keep both</option><option value="overwrite">Replace after verification</option><option value="skip">Skip existing</option></select></label><button className="primary wide" disabled={busy || !destination || !selectedFiles.length} onClick={receiveFiles}>Accept selected files</button></div>}</div>
        </section>}

        {view === 'share' && <section className="page narrow"><Intro number="03" title="Share one local service" text="Expose a local TCP service through Tailcat. Only the selected remote port is admitted; your operating system routes remain untouched." /><div className="panel form-panel"><label>Session label<input value={shareLabel} onChange={(e) => setShareLabel(e.target.value)} /></label><div className="field-grid"><label>Local port<input type="number" min="1" max="65535" value={shareLocalPort} onChange={(e) => setShareLocalPort(Number(e.target.value))} /></label><label>Remote port<input type="number" min="1" max="65535" value={shareRemotePort} onChange={(e) => setShareRemotePort(Number(e.target.value))} /></label></div><label className="switch-row"><input type="checkbox" checked={sharePersistent} onChange={(e) => setSharePersistent(e.target.checked)} /><span><strong>Persistent trusted identity</strong><small>Anyone previously given this address can reconnect unless access is rotated.</small></span></label><button className="primary wide" disabled={busy} onClick={() => run(() => API.ShareService({ label: shareLabel, localHost: '127.0.0.1', localPort: shareLocalPort, remotePort: shareRemotePort, persistent: sharePersistent }), 'Private service is listening.').then((result) => result && setView('activity'))}>Start sharing</button></div></section>}

        {view === 'connect' && <section className="page narrow"><Intro number="04" title="Open a remote service locally" text="A loopback listener maps ordinary browser, database, and IDE traffic through one reusable Tailcat client." /><div className="panel form-panel"><label>Source<select value={connectDevice} onChange={(e) => setConnectDevice(e.target.value)}><option value="">Temporary invite token</option>{snapshot.trustedDevices.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select></label>{!connectDevice && <label>Tailcat token<textarea value={connectToken} onChange={(e) => setConnectToken(e.target.value)} placeholder="tc…" rows={3} /></label>}<label>Connection label<input value={connectLabel} onChange={(e) => setConnectLabel(e.target.value)} /></label><div className="field-grid"><label>Remote port<input type="number" min="1" max="65535" value={connectRemotePort} onChange={(e) => setConnectRemotePort(Number(e.target.value))} /></label><label>Local port <small>0 chooses a free port</small><input type="number" min="0" max="65535" value={connectLocalPort} onChange={(e) => setConnectLocalPort(Number(e.target.value))} /></label></div><button className="primary wide" disabled={busy || (!connectDevice && !connectToken.trim())} onClick={() => run(() => connectDevice ? API.ConnectTrustedService(connectDevice, connectLabel, connectRemotePort, connectLocalPort) : API.ConnectService({ label: connectLabel, token: connectToken, remotePort: connectRemotePort, localPort: connectLocalPort }), 'Local service link started.').then((result) => result && setView('activity'))}>Connect locally</button></div></section>}

        {view === 'activity' && <section className="page"><SectionTitle title="Live sessions" /><div className="session-grid">{snapshot.sessions.length ? snapshot.sessions.map((session) => <SessionCard key={session.id} session={session} onCopied={(text) => setNotice({ kind: 'ok', text })} onStop={(id) => run(() => API.StopSession(id), 'Session stopped.')} />) : <Empty text="No live sessions. Start a file offer or share a service." />}</div><SectionTitle title="Transfer history" /><TransferList transfers={snapshot.transfers} onPause={(id) => run(() => API.PauseTransfer(id), 'Transfer paused.')} onResume={(id) => run(() => API.ResumeTransfer(id), 'Transfer queued to resume.')} /></section>}

        {view === 'devices' && <section className="page narrow"><Intro number="05" title="Name a persistent endpoint" text="The complete capability token is stored in the operating system credential manager. The database keeps only a redacted hint." /><div className="panel form-panel"><label>Device name<input value={deviceName} onChange={(e) => setDeviceName(e.target.value)} placeholder="Home workstation" /></label><label>Persistent Tailcat token<textarea value={deviceToken} onChange={(e) => setDeviceToken(e.target.value)} rows={3} placeholder="tc…" /></label><button className="primary wide" disabled={busy || !deviceName || !deviceToken} onClick={() => run(() => API.AddTrustedDevice(deviceName, deviceToken), 'Trusted device saved.').then((result) => { if (result) { setDeviceName(''); setDeviceToken('') } })}>Save trusted device</button></div><div className="device-list">{snapshot.trustedDevices.map((device) => <article key={device.id}><span className="device-mark">{device.name.slice(0, 2).toUpperCase()}</span><div><strong>{device.name}</strong><code>{device.tokenHint}</code></div><button className="quiet" onClick={() => run(() => API.RemoveTrustedDevice(device.id), 'Trusted device removed.')}>Remove</button></article>)}</div></section>}
      </main>
      {notice && <div className={`toast ${notice.kind}`}>{notice.text}</div>}
      {busy && <div className="busy-line" />}
    </div>
  )
}

type ReceiverPlatform = 'windows' | 'linux' | 'macos'

function receiverCommand(platform: ReceiverPlatform, token: string): string {
  if (platform === 'windows') {
    return `.\\tailcat.exe cp -r "${token}:." .`
  }
  if (platform === 'linux') {
    return `./tailcat cp -r '${token}:.' .`
  }
  return `tailcat cp -r '${token}:.' .`
}

function SessionCard({ session, onCopied, onStop }: { session: Session; onCopied: (text: string) => void; onStop: (id: string) => void }) {
  return <article className="session">
    <div className="session-head"><span className={`state ${session.state}`}>{session.state}</span><small>{session.kind.replace('-', ' ')}</small></div>
    <h3>{session.label}</h3>
    {session.localAddress && <code>{session.localAddress}</code>}
    {session.token && <div className="token"><span>{session.token.slice(0, 22)}…</span><button onClick={() => navigator.clipboard.writeText(session.token || '').then(() => onCopied('Invite copied to clipboard.'))}>Copy invite</button></div>}
    {session.kind === 'service-share' && session.token && session.remotePort && <ServiceClientGuide token={session.token} port={session.remotePort} onCopied={onCopied} />}
    {session.kind === 'file-offer' && session.token && <ReceiverGuide token={session.token} compatible={session.cliCompatible} onCopied={onCopied} />}
    <dl><div><dt>Transport</dt><dd>{session.transport || 'waiting'}</dd></div><div><dt>Remote port</dt><dd>{session.remotePort || '—'}</dd></div></dl>
    {session.error && <p className="inline-error">{session.error}</p>}
    <button className="danger" onClick={() => onStop(session.id)}>Stop session</button>
  </article>
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
    <div className="receiver-head"><div><strong>Receiver without the GUI</strong><small>Open a terminal in the destination folder, then run this command.</small></div><select aria-label="Receiver operating system" value={platform} onChange={(event) => setPlatform(event.target.value as ReceiverPlatform)}><option value="windows">Windows</option><option value="linux">Linux release</option><option value="macos">macOS</option></select></div>
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
    <div className="receiver-head"><div><strong>Client without the GUI</strong><small>Run a local SOCKS5 proxy for service port {port}.</small></div><select aria-label="Client operating system" value={platform} onChange={(event) => setPlatform(event.target.value as ReceiverPlatform)}><option value="windows">Windows</option><option value="linux">Linux release</option><option value="macos">macOS</option></select></div>
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
