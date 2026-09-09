import { useEffect, useState } from 'react'
import { Dialogs } from '@wailsio/runtime'
import * as API from '../bindings/github.com/DavidCarliez/whiskerlink/appservice.js'
import { createInviteQRCode } from './invites'

export type PeerDevice = { id: string; name: string; state: string; code?: string; confirmed: boolean; online: boolean; expiresAt: string }
type Delivery = { id: string; deviceId: string; deviceName: string; label: string; state: string; error?: string; transferId?: string }
export type PeerSnapshot = { enabled: boolean; name: string; error?: string; devices: PeerDevice[]; inbox: Delivery[]; outbox: Delivery[] }
export const emptyPeers: PeerSnapshot = { enabled: false, name: '', devices: [], inbox: [], outbox: [] }
type Manifest = { label: string; files: { path: string; size: number }[]; totalBytes: number }
const size = (n: number) => n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`

export function Peers({ snapshot, refresh, incomingInvite, onInviteHandled, activity }: { snapshot: PeerSnapshot; refresh: () => Promise<void>; incomingInvite: string; onInviteHandled: () => void; activity: () => void }) {
  const [name, setName] = useState(snapshot.name)
  const [invite, setInvite] = useState(incomingInvite)
  const [ownInvite, setOwnInvite] = useState('')
  const [expires, setExpires] = useState('')
  const [qr, setQR] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [inspection, setInspection] = useState<{ id: string; manifest: Manifest } | null>(null)
  const [selected, setSelected] = useState<string[]>([])
  const [destination, setDestination] = useState('')
  const [collision, setCollision] = useState('rename')
  const [removeID, setRemoveID] = useState('')
  useEffect(() => { if (snapshot.name && !name) setName(snapshot.name) }, [snapshot.name])
  useEffect(() => { if (incomingInvite) { setInvite(incomingInvite); onInviteHandled() } }, [incomingInvite])
  useEffect(() => { let active = true; setQR(null); if (ownInvite) void createInviteQRCode(ownInvite).then(value => { if (active) setQR(value) }); return () => { active = false } }, [ownInvite])
  const run = async (fn: () => Promise<unknown>, success: string) => {
    setBusy(true); setError(''); setNotice('')
    try { await fn(); await refresh(); setNotice(success) } catch (e) { setError(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }
  const paired = snapshot.devices.filter(d => d.state === 'paired')
  return <section className="page peers-page">
    <div className="intro"><span>08</span><div><h2>Pair once. Send directly.</h2><p>Pair your computers, then send files to their inbox. The recipient chooses what to receive. Keep both apps running for delivery.</p></div></div>
    {(error || snapshot.error) && <p role="alert" className="inline-error">{error || snapshot.error}</p>}
    {notice && <p role="status" className="peer-notice">{notice}</p>}
    <details className="peer-setup" open={!snapshot.enabled || !paired.length || !!invite}>
    <summary>{paired.length ? 'Pair another computer · device settings' : 'Set up paired devices'}</summary>
    <div className="panel form-panel">
      <label>This computer<input maxLength={80} value={name} onChange={e => setName(e.target.value)} placeholder="Studio laptop" /></label>
      <button disabled={busy || !name.trim()} onClick={() => void run(() => API.EnablePairing(name), 'This computer is ready for pairing.')}>{snapshot.enabled ? 'Save device name' : 'Enable paired devices'}</button>
      <small>Device credentials are stored in your operating system credential manager. Pairing stays available while WhiskerLink is running, including in the tray.</small>
    </div>
    {snapshot.enabled && <>
      <div className="peer-columns">
        <div className="panel form-panel">
          <h3>Invite another computer</h3><p>Create a one-use invitation, valid for 10 minutes. Paste it into WhiskerLink on your other computer.</p>
          <button disabled={busy} onClick={() => void run(async () => { const result = await API.CreatePairingInvite(); setOwnInvite(result.url); setExpires(result.expiresAt) }, 'Pairing invitation created.')}>Create pairing invite</button>
          {ownInvite && <div className="peer-invite">{qr && <img src={qr} alt="Pair this computer with WhiskerLink" />}<small>Expires {new Date(expires).toLocaleTimeString()}. Share only with the computer you intend to pair.</small><button onClick={() => void run(() => navigator.clipboard.writeText(ownInvite), 'Pairing invite copied.')}>Copy pairing invite</button><button className="quiet" onClick={() => setOwnInvite('')}>Hide invite</button></div>}
        </div>
        <div className="panel form-panel">
          <h3>Join an invitation</h3><label>Pairing invite<textarea rows={4} value={invite} onChange={e => setInvite(e.target.value)} placeholder="whiskerlink://pair?…" /></label>
          <button disabled={busy || !invite.trim()} onClick={() => void run(async () => { await API.JoinPairingInvite(invite); setInvite('') }, 'Compare the verification code on both computers, then confirm on each one.')}>Request pairing</button>
        </div>
      </div>
    </>}
    </details>
      <div className="section-title"><h2>Your devices</h2><span>{paired.length} paired</span></div>
      <div className="peer-device-grid">{snapshot.devices.map(device => <article className="panel form-panel" key={device.id}>
        <div className="session-head"><strong>{device.name}</strong><span className={`state ${device.online ? 'connected' : ''}`}>{device.state === 'pending' ? 'Confirm pairing' : device.online ? 'Reachable' : 'Not reachable'}</span></div>
        {device.state === 'pending' && <><p>Confirm only if this code matches on both computers.</p><code className="pair-code">{device.code}</code><small>Expires {new Date(device.expiresAt).toLocaleTimeString()}</small><button className="primary" disabled={busy || device.confirmed || Date.parse(device.expiresAt) <= Date.now()} onClick={() => void run(() => API.ConfirmPairing(device.id), 'Confirmed here. Waiting for the other computer if needed.')}>{device.confirmed ? 'Confirmed here — waiting' : 'Codes match — confirm'}</button></>}
        {device.state === 'paired' && <p>Select this computer in <strong>Send files → Send to</strong>. Offline requests stay queued on the sender.</p>}
        {removeID === device.id ? <div><p>Unpairing stops outgoing offers and cancels incoming transfers from this device. Received files remain on disk. Remove this computer on the other device too before pairing again.</p><button className="danger" disabled={busy} onClick={() => void run(async () => { await API.UnpairDevice(device.id); setRemoveID('') }, 'Device removed. Pair again to reconnect.')}>Unpair and stop sharing</button><button className="quiet" onClick={() => setRemoveID('')}>Keep device</button></div> : <button className="quiet" onClick={() => setRemoveID(device.id)}>{device.state === 'pending' ? 'Reject pairing' : 'Unpair device'}</button>}
      </article>)}</div>
      {!snapshot.devices.length && <div className="empty"><p>Your paired computers will appear here.</p></div>}
    <div className="section-title"><h2>Incoming files</h2><button className="quiet" onClick={activity}>Transfer activity</button></div>
    {!snapshot.inbox.length && <div className="empty"><p>No incoming requests. Files arrive here when a paired computer sends to you.</p></div>}
    <div className="peer-requests">{snapshot.inbox.map(item => <article className="panel form-panel" key={item.id}>
      <div className="session-head"><div><strong>{item.label}</strong><small className="peer-sender">From {item.deviceName}</small></div><span className={`state ${item.state}`}>{item.state}</span></div>
      {item.error && <p className="inline-error">{item.error}</p>}
      {item.state === 'pending' && <div className="actions"><button disabled={busy} onClick={() => void run(async () => { const result = await API.InspectIncoming(item.id); const manifest = { ...result, files: result.files ?? [] }; setInspection({ id: item.id, manifest }); setSelected(manifest.files.map(f => f.path)); setDestination('') }, 'File list inspected. Choose files and a destination before accepting.')}>Inspect files</button><button disabled={busy} onClick={() => void run(async () => { await API.DeclineIncoming(item.id); if (inspection?.id === item.id) setInspection(null) }, 'Incoming request declined.')}>Decline</button></div>}
      {inspection?.id === item.id && item.state === 'pending' && <div className="manifest"><strong>{inspection.manifest.files.length} files · {size(inspection.manifest.totalBytes)}</strong><div className="manifest-files">{inspection.manifest.files.map(file => <label key={file.path}><input type="checkbox" checked={selected.includes(file.path)} onChange={() => setSelected(current => current.includes(file.path) ? current.filter(p => p !== file.path) : [...current, file.path])} /><span>{file.path}</span><small>{size(file.size)}</small></label>)}</div><label>Destination<div className="picker"><button disabled={busy} onClick={() => void run(async () => { const path = await Dialogs.OpenFile({ Title: 'Choose incoming files destination', CanChooseDirectories: true, CanChooseFiles: false, AllowsMultipleSelection: false }); if (typeof path === 'string') setDestination(path) }, '')}>Choose destination</button><span>{destination || 'No folder selected'}</span></div></label><label>Existing files<select value={collision} onChange={e => setCollision(e.target.value)}><option value="rename">Keep both</option><option value="skip">Skip existing</option><option value="overwrite">Replace after verification</option></select></label><button className="primary" disabled={busy || !destination || !selected.length} onClick={() => void run(async () => { await API.AcceptIncoming(item.id, destination, selected, collision); setInspection(null) }, 'Accepted. Open Transfer activity to pause, resume, or follow progress.')}>Accept selected files</button></div>}
    </article>)}</div>
    <div className="section-title"><h2>Sent requests</h2></div>
    {!snapshot.outbox.length && <div className="empty"><p>Choose a paired computer in Send files to start.</p></div>}
    <div className="peer-requests">{snapshot.outbox.map(item => <article className="panel form-panel" key={item.id}><div className="session-head"><div><strong>{item.label}</strong><small className="peer-sender">To {item.deviceName}</small></div><span className={`state ${item.state}`}>{item.state}</span></div>{item.error && <p className="inline-error">{item.error}</p>}<small>{item.state === 'queued' ? 'Waiting for your other computer. Keep the source files unchanged.' : item.state === 'pending' ? 'Delivered to the inbox. Waiting for the recipient to accept.' : item.state === 'completed' ? 'The recipient finished receiving and verifying the selected files.' : 'Transfer status is refreshed while both computers are reachable.'}</small></article>)}</div>
  </section>
}
