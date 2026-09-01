package transfer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestOfferManifestAndResume(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("tailcat-gui\n"), 1024)
	path := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	offer, err := BuildOffer([]string{path}, "Payload")
	if err != nil {
		t.Fatal(err)
	}
	if got := offer.Manifest.TotalBytes; got != int64(len(content)) {
		t.Fatalf("manifest size = %d, want %d", got, len(content))
	}
	wantHash := sha256.Sum256(content)
	if got := offer.Manifest.Files[0].SHA256; got != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("manifest hash = %s, want %x", got, wantHash)
	}

	server, client := net.Pipe()
	go offer.Handle(server)
	offset := int64(137)
	if err := writeJSONLine(client, request{
		Version: ProtocolVersion, Action: "get", TransferID: offer.Manifest.TransferID,
		Path: offer.Manifest.Files[0].Path, Offset: offset,
	}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	var header response
	if err := readJSONLine(reader, &header); err != nil {
		t.Fatal(err)
	}
	if !header.OK || header.Offset != offset || header.Size != int64(len(content)) {
		t.Fatalf("unexpected response: %+v", header)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content[offset:]) {
		t.Fatal("resumed payload does not match source")
	}
}

func TestSingleFileOfferStagesCLIExport(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(source, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	offer, err := BuildOffer([]string{source}, "Photo")
	if err != nil {
		t.Fatal(err)
	}
	if !offer.Manifest.CLICompatible || offer.CompatibleDir == "" {
		t.Fatal("single-file offer is not CLI compatible")
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	stagedInfo, err := os.Stat(filepath.Join(offer.CompatibleDir, "photo.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, stagedInfo) {
		t.Fatal("CLI export copied the source instead of hard-linking it")
	}
	stageDir := offer.CompatibleDir
	if err := offer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("CLI staging directory still exists after close: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("closing offer affected source file: %v", err)
	}
}

func TestOfferRejectsChangedSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	offer, err := BuildOffer([]string{path}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed and longer"), 0o600); err != nil {
		t.Fatal(err)
	}

	server, client := net.Pipe()
	go offer.Handle(server)
	entry := offer.Manifest.Files[0]
	if err := writeJSONLine(client, request{Version: ProtocolVersion, Action: "get", TransferID: offer.Manifest.TransferID, Path: entry.Path}); err != nil {
		t.Fatal(err)
	}
	var header response
	if err := readJSONLine(bufio.NewReader(client), &header); err != nil {
		t.Fatal(err)
	}
	if header.OK || header.Error != "source file changed after it was offered" {
		t.Fatalf("unexpected response: %+v", header)
	}
}

func TestSafeDestinationRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape", "folder/../../escape", "/absolute"} {
		if _, err := safeDestination(root, path); err == nil {
			t.Fatalf("safeDestination accepted %q", path)
		}
	}
	if got, err := safeDestination(root, "folder/file.txt"); err != nil || got != filepath.Join(root, "folder", "file.txt") {
		t.Fatalf("safe path = %q, %v", got, err)
	}
}
