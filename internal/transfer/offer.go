package transfer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"sync"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

const (
	ProtocolVersion          = 1
	ProtocolPort      uint16 = 43111
	maxControlMessage        = 16 << 20
)

type sourceFile struct {
	absolute string
	entry    domain.FileEntry
}

type Offer struct {
	Manifest      domain.FileManifest
	files         map[string]sourceFile
	CompatibleDir string

	cleanupDir  string
	cleanupOnce sync.Once
	cleanupErr  error
	progressMu  sync.Mutex
	served      map[string]int64
	onProgress  func(bytes int64, completed int)
}

type request struct {
	Version    int    `json:"version"`
	Action     string `json:"action"`
	TransferID string `json:"transferId,omitempty"`
	Path       string `json:"path,omitempty"`
	Offset     int64  `json:"offset,omitempty"`
}

type response struct {
	OK       bool                 `json:"ok"`
	Error    string               `json:"error,omitempty"`
	Manifest *domain.FileManifest `json:"manifest,omitempty"`
	Size     int64                `json:"size,omitempty"`
	Offset   int64                `json:"offset,omitempty"`
}

func BuildOffer(paths []string, label string) (*Offer, error) {
	if len(paths) == 0 {
		return nil, errors.New("select at least one file or folder")
	}
	if label == "" {
		label = "File transfer"
	}
	offer := &Offer{
		Manifest: domain.FileManifest{ProtocolVersion: ProtocolVersion, TransferID: uuid.NewString(), Label: label},
		files:    make(map[string]sourceFile),
	}
	seenRoots := make(map[string]bool)
	for _, selected := range paths {
		absolute, err := filepath.Abs(selected)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", selected, err)
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("inspect %q: %w", selected, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symbolic links are not supported: %s", selected)
		}
		rootName := filepath.Base(absolute)
		if seenRoots[rootName] {
			return nil, fmt.Errorf("two selected items have the name %q", rootName)
		}
		seenRoots[rootName] = true
		if info.IsDir() {
			if len(paths) == 1 {
				offer.CompatibleDir = absolute
				offer.Manifest.CLICompatible = true
			}
			err = filepath.WalkDir(absolute, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("symbolic links are not supported: %s", path)
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(absolute, path)
				if err != nil {
					return err
				}
				return offer.addFile(path, filepath.Join(rootName, rel))
			})
			if err != nil {
				return nil, fmt.Errorf("scan %q: %w", selected, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("only regular files and folders can be sent: %s", selected)
		}
		if err := offer.addFile(absolute, rootName); err != nil {
			return nil, err
		}
		if len(paths) == 1 {
			stageDir, stageErr := os.MkdirTemp(filepath.Dir(absolute), ".whiskerlink-offer-")
			if stageErr == nil {
				if stageErr = os.Link(absolute, filepath.Join(stageDir, rootName)); stageErr == nil {
					offer.CompatibleDir = stageDir
					offer.Manifest.CLICompatible = true
					offer.cleanupDir = stageDir
				} else {
					_ = os.Remove(stageDir)
				}
			}
		}
	}
	if len(offer.files) == 0 {
		return nil, errors.New("the selection contains no files")
	}
	sort.Slice(offer.Manifest.Files, func(i, j int) bool { return offer.Manifest.Files[i].Path < offer.Manifest.Files[j].Path })
	return offer, nil
}

func (o *Offer) Close() error {
	o.cleanupOnce.Do(func() {
		if o.cleanupDir != "" {
			o.cleanupErr = os.RemoveAll(o.cleanupDir)
		}
	})
	return o.cleanupErr
}

func (o *Offer) addFile(absolute, relative string) error {
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return fmt.Errorf("unsafe relative path %q", relative)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("only regular files can be sent: %s", absolute)
	}
	hash, err := hashFile(absolute)
	if err != nil {
		return err
	}
	entry := domain.FileEntry{Path: clean, Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), SHA256: hash}
	o.files[clean] = sourceFile{absolute: absolute, entry: entry}
	o.Manifest.Files = append(o.Manifest.Files, entry)
	o.Manifest.TotalBytes += entry.Size
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (o *Offer) Handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	var req request
	if err := readJSONLine(reader, &req); err != nil {
		_ = writeJSONLine(conn, response{Error: err.Error()})
		return
	}
	if req.Version != ProtocolVersion {
		_ = writeJSONLine(conn, response{Error: "unsupported protocol version"})
		return
	}
	switch req.Action {
	case "manifest":
		_ = writeJSONLine(conn, response{OK: true, Manifest: &o.Manifest})
	case "get":
		o.serveFile(conn, req)
	default:
		_ = writeJSONLine(conn, response{Error: "unsupported action"})
	}
}

func (o *Offer) serveFile(conn net.Conn, req request) {
	if req.TransferID != o.Manifest.TransferID {
		_ = writeJSONLine(conn, response{Error: "transfer ID does not match"})
		return
	}
	source, ok := o.files[req.Path]
	if !ok {
		_ = writeJSONLine(conn, response{Error: "file is not in this offer"})
		return
	}
	if req.Offset < 0 || req.Offset > source.entry.Size {
		_ = writeJSONLine(conn, response{Error: "invalid resume offset"})
		return
	}
	f, err := os.Open(source.absolute)
	if err != nil {
		_ = writeJSONLine(conn, response{Error: "source file is unavailable"})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != source.entry.Size || info.ModTime().UnixNano() != source.entry.ModTimeUnixNano {
		_ = writeJSONLine(conn, response{Error: "source file changed after it was offered"})
		return
	}
	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		_ = writeJSONLine(conn, response{Error: "cannot resume source file"})
		return
	}
	if err := writeJSONLine(conn, response{OK: true, Size: source.entry.Size, Offset: req.Offset}); err != nil {
		return
	}
	n, _ := io.CopyN(conn, f, source.entry.Size-req.Offset)
	o.recordProgress(req.Path, req.Offset+n)
}

func (o *Offer) SetProgressCallback(callback func(bytes int64, completed int)) {
	o.progressMu.Lock()
	defer o.progressMu.Unlock()
	o.onProgress = callback
}

func (o *Offer) recordProgress(path string, end int64) {
	o.progressMu.Lock()
	if o.served == nil {
		o.served = make(map[string]int64)
	}
	if end > o.served[path] {
		o.served[path] = end
	}
	var total int64
	completed := 0
	for file, sent := range o.served {
		total += sent
		if source, ok := o.files[file]; ok && sent == source.entry.Size {
			completed++
		}
	}
	callback := o.onProgress
	o.progressMu.Unlock()
	if callback != nil {
		callback(total, completed)
	}
}

func writeJSONLine(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxControlMessage {
		return errors.New("control message is too large")
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func readJSONLine(reader *bufio.Reader, value any) error {
	var data []byte
	for {
		part, prefix, err := reader.ReadLine()
		if err != nil {
			return err
		}
		if len(data)+len(part) > maxControlMessage {
			return errors.New("control message is too large")
		}
		data = append(data, part...)
		if !prefix {
			break
		}
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid control message: %w", err)
	}
	return nil
}
