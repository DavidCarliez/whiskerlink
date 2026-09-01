package domain

import "time"

type SessionKind string

const (
	SessionServiceShare SessionKind = "service-share"
	SessionServiceLink  SessionKind = "service-link"
	SessionFileOffer    SessionKind = "file-offer"
)

type Session struct {
	ID            string      `json:"id"`
	Kind          SessionKind `json:"kind"`
	Label         string      `json:"label"`
	State         string      `json:"state"`
	Token         string      `json:"token,omitempty"`
	LocalAddress  string      `json:"localAddress,omitempty"`
	RemotePort    uint16      `json:"remotePort,omitempty"`
	Transport     string      `json:"transport,omitempty"`
	LatencyMS     int64       `json:"latencyMs,omitempty"`
	Persistent    bool        `json:"persistent"`
	CLICompatible bool        `json:"cliCompatible"`
	CreatedAt     time.Time   `json:"createdAt"`
	Error         string      `json:"error,omitempty"`
}

type FileEntry struct {
	Path            string `json:"path"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"modTimeUnixNano"`
	SHA256          string `json:"sha256"`
}

type FileManifest struct {
	ProtocolVersion int         `json:"protocolVersion"`
	TransferID      string      `json:"transferId"`
	Label           string      `json:"label"`
	Files           []FileEntry `json:"files"`
	TotalBytes      int64       `json:"totalBytes"`
	CLICompatible   bool        `json:"cliCompatible"`
}

type TransferDirection string

const (
	TransferSend    TransferDirection = "send"
	TransferReceive TransferDirection = "receive"
)

type Transfer struct {
	ID               string            `json:"id"`
	Direction        TransferDirection `json:"direction"`
	Label            string            `json:"label"`
	State            string            `json:"state"`
	Token            string            `json:"token,omitempty"`
	Destination      string            `json:"destination,omitempty"`
	FilesTotal       int               `json:"filesTotal"`
	FilesCompleted   int               `json:"filesCompleted"`
	BytesTotal       int64             `json:"bytesTotal"`
	BytesTransferred int64             `json:"bytesTransferred"`
	Error            string            `json:"error,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
}

type TrustedDevice struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHint string    `json:"tokenHint"`
	CreatedAt time.Time `json:"createdAt"`
	LastUsed  time.Time `json:"lastUsed,omitempty"`
}

type Snapshot struct {
	Sessions       []Session       `json:"sessions"`
	Transfers      []Transfer      `json:"transfers"`
	TrustedDevices []TrustedDevice `json:"trustedDevices"`
}
