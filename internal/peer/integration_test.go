package peer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/DavidCarliez/whiskerlink/internal/transfer"
)

type integrationCommand struct {
	Action, Value, ID, Destination string
	Selected                       []string
}
type integrationReply struct {
	ID, Code, Error, Hash string
	Snapshot              Snapshot
	Manifest              domain.FileManifest
	Transfer              domain.Transfer
}

// This uses separate processes, actual Tailcat networking and the production
// transfer engine. It requires network access to the public DERP service.
func TestPairedFilesTailcatRoundTrip(t *testing.T) {
	if os.Getenv("TAILCAT_INTEGRATION") != "1" {
		t.Skip("set TAILCAT_INTEGRATION=1 for real two-process pairing and file transfer")
	}
	secret := &memorySecrets{values: map[string]string{}}
	store, err := storage.OpenAt(t.TempDir(), secret)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr, err := transfer.NewManager(store, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { tr.Close() }()
	a, err := New(store, tr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	if err = a.Enable("Sender"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPairedFilesHelper$")
	cmd.Env = append(os.Environ(), "WHISKERLINK_TEST_HELPER=1", "WHISKERLINK_TEST_DIR="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	enc := json.NewEncoder(stdin)
	dec := json.NewDecoder(stdout)
	read := func() integrationReply {
		var r integrationReply
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("helper response: %v; %s", err, stderr.String())
		}
		if r.Error != "" {
			t.Fatalf("helper: %s", r.Error)
		}
		return r
	}
	call := func(c integrationCommand) integrationReply {
		if err := enc.Encode(c); err != nil {
			t.Fatal(err)
		}
		return read()
	}
	ready := read()
	t.Log("two independent Tailcat device endpoints started")
	invite, err := a.CreateInvite()
	if err != nil {
		t.Fatal(err)
	}
	joined := call(integrationCommand{Action: "join", Value: invite.URL})
	if a.Snapshot().Devices[0].Code != joined.Code {
		t.Fatal("verification codes differ")
	}
	if err = a.Confirm(ready.ID); err != nil {
		t.Fatal(err)
	}
	call(integrationCommand{Action: "confirm", ID: a.id})
	call(integrationCommand{Action: "tick"})
	a.tick(ctx)
	if a.Snapshot().Devices[0].State != "paired" {
		t.Fatal("devices did not pair")
	}
	t.Log("pairing confirmed by both processes")
	source := filepath.Join(t.TempDir(), "payload.bin")
	payload := make([]byte, 8*1024*1024)
	if _, err = rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	delivery, err := a.Send(ready.ID, []string{source}, "Integration payload")
	if err != nil {
		t.Fatal(err)
	}
	a.tick(ctx)
	incoming := call(integrationCommand{Action: "snapshot"})
	if len(incoming.Snapshot.Inbox) != 1 {
		t.Fatalf("inbox request count = %d", len(incoming.Snapshot.Inbox))
	}
	inspected := call(integrationCommand{Action: "inspect", ID: delivery.ID})
	destination := t.TempDir()
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatal("files written before acceptance")
	}
	paths := []string{}
	for _, f := range inspected.Manifest.Files {
		paths = append(paths, f.Path)
	}
	accepted := call(integrationCommand{Action: "accept", ID: delivery.ID, Destination: destination, Selected: paths})
	call(integrationCommand{Action: "pause", ID: accepted.Transfer.ID})
	t.Log("transfer explicitly accepted and paused")
	a.Close()
	tr.Close()
	tr, err = transfer.NewManager(store, func() {})
	if err != nil {
		t.Fatal(err)
	}
	a, err = New(store, tr)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Enable("Sender"); err != nil {
		t.Fatal(err)
	}
	a.tick(ctx)
	t.Log("sender restarted and restored the original offer identity and manifest")
	call(integrationCommand{Action: "restart"})
	call(integrationCommand{Action: "resume", ID: accepted.Transfer.ID})
	t.Log("receiver restarted and resumed persisted transfer")
	done := false
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		r := call(integrationCommand{Action: "snapshot"})
		for _, d := range r.Snapshot.Inbox {
			if d.State == "completed" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !done {
		t.Fatal("transfer did not complete")
	}
	got, err := os.ReadFile(filepath.Join(destination, paths[0]))
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(got)
	if gotSum != sum {
		t.Fatal("received content hash mismatch")
	}
	a.tick(ctx)
	if a.Snapshot().Outbox[0].State != "completed" {
		t.Fatal("sender did not receive verified completion")
	}
	t.Logf("received %d bytes with matching SHA-256 %s", len(got), hex.EncodeToString(sum[:]))
	call(integrationCommand{Action: "unpair", ID: a.id})
	t.Log("receiver revoked pairing")
}

func TestPairedFilesHelper(t *testing.T) {
	if os.Getenv("WHISKERLINK_TEST_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	secrets := &memorySecrets{values: map[string]string{}}
	store, err := storage.OpenAt(os.Getenv("WHISKERLINK_TEST_DIR"), secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var tr *transfer.Manager
	var m *Manager
	start := func() error {
		var err error
		tr, err = transfer.NewManager(store, func() {})
		if err != nil {
			return err
		}
		m, err = New(store, tr)
		if err != nil {
			return err
		}
		return m.Enable("Receiver")
	}
	if err = start(); err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close(); tr.Close() }()
	out := json.NewEncoder(os.Stdout)
	_ = out.Encode(integrationReply{ID: m.id})
	reader := json.NewDecoder(bufio.NewReader(os.Stdin))
	for {
		var c integrationCommand
		if err = reader.Decode(&c); err == io.EOF {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		r := integrationReply{}
		err = nil
		switch c.Action {
		case "join":
			var d Device
			d, err = m.JoinInvite(c.Value)
			r.Code = d.Code
		case "confirm":
			err = m.Confirm(c.ID)
		case "tick":
			m.tick(context.Background())
		case "snapshot":
			m.tick(context.Background())
			r.Snapshot = m.Snapshot()
		case "inspect":
			r.Manifest, err = m.Inspect(c.ID)
		case "accept":
			r.Transfer, err = m.Accept(c.ID, c.Destination, c.Selected, "rename")
		case "pause":
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				err = tr.Pause(c.ID)
				if err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err == nil {
				deadline = time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					paused := false
					for _, item := range tr.Transfers() {
						if item.ID == c.ID && item.State == "paused" {
							paused = true
						}
					}
					if paused {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		case "restart":
			m.Close()
			tr.Close()
			err = start()
		case "resume":
			err = tr.Resume(c.ID)
		case "unpair":
			err = m.Unpair(c.ID)
		default:
			err = fmt.Errorf("unknown helper command %s", c.Action)
		}
		if err != nil {
			r.Error = err.Error()
		}
		if err = out.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}
