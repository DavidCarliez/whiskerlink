package tailnet

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestShareServiceRejectsUnavailableLocalTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(nil, nil)
	_, err = manager.ShareService(context.Background(), ShareServiceRequest{
		Label: "missing service", LocalPort: port, RemotePort: 42002,
	})
	if err == nil || !strings.Contains(err.Error(), "local service is unavailable") {
		t.Fatalf("ShareService error = %v, want unavailable local service", err)
	}
}

func TestTailcatServiceRoundTrip(t *testing.T) {
	if os.Getenv("TAILCAT_INTEGRATION") != "1" {
		t.Skip("set TAILCAT_INTEGRATION=1 to use the public Tailcat DERP service")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	manager := NewManager(nil, nil)
	defer manager.StopAll()
	server, err := manager.ShareService(context.Background(), ShareServiceRequest{
		Label: "integration echo", LocalPort: uint16(echo.Addr().(*net.TCPAddr).Port), RemotePort: 42001,
	})
	if err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=TestTailcatClientHelper", "-test.v")
	command.Env = append(os.Environ(), "TAILCAT_CLIENT_HELPER=1", "TAILCAT_TOKEN="+server.Token, "TAILCAT_PORT=42001")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("client process failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "tailcat-gui-round-trip") {
		t.Fatalf("client output did not contain echoed payload:\n%s", output)
	}
}

func TestTailcatSOCKSHTTPRoundTrip(t *testing.T) {
	if os.Getenv("TAILCAT_INTEGRATION") != "1" {
		t.Skip("set TAILCAT_INTEGRATION=1 to use the public Tailcat DERP service")
	}
	tailcatCLI := os.Getenv("TAILCAT_CLI")
	if tailcatCLI == "" {
		var err error
		tailcatCLI, err = exec.LookPath("tailcat")
		if err != nil {
			t.Skip("set TAILCAT_CLI to a tailcat executable")
		}
	}

	web := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "tailcat-socks-http")
	}))
	defer web.Close()
	localPort := uint16(web.Listener.Addr().(*net.TCPAddr).Port)

	manager := NewManager(nil, nil)
	defer manager.StopAll()
	server, err := manager.ShareService(context.Background(), ShareServiceRequest{
		Label: "integration HTTP", LocalPort: localPort, RemotePort: 42003,
	})
	if err != nil {
		t.Fatal(err)
	}

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	socksAddress := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, tailcatCLI, "socks", "--listen="+socksAddress, server.Token)
	var commandOutput bytes.Buffer
	command.Stdout = &commandOutput
	command.Stderr = &commandOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = command.Wait()
	}()

	readyDeadline := time.Now().Add(10 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("tcp", socksAddress, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("SOCKS proxy did not start: %v\n%s", dialErr, commandOutput.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	dialer, err := proxy.SOCKS5("tcp", socksAddress, nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return dialer.Dial(network, address)
		}},
	}
	response, err := httpClient.Get("http://server.tailcat:42003/")
	if err != nil {
		t.Fatalf("HTTP through Tailcat SOCKS failed: %v\n%s", err, commandOutput.String())
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "tailcat-socks-http" {
		t.Fatalf("HTTP body = %q, want tailcat-socks-http", body)
	}
}

func TestTailcatClientHelper(t *testing.T) {
	if os.Getenv("TAILCAT_CLIENT_HELPER") != "1" {
		t.Skip("integration helper")
	}
	portValue, err := strconv.ParseUint(os.Getenv("TAILCAT_PORT"), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, nil)
	defer manager.StopAll()
	link, err := manager.ConnectService(context.Background(), ConnectServiceRequest{
		Label: "integration client", Token: os.Getenv("TAILCAT_TOKEN"), RemotePort: uint16(portValue),
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", link.LocalAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := []byte("tailcat-gui-round-trip")
	if _, err := conn.Write(message); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v; sessions: %+v", err, manager.Sessions())
	}
	t.Log(string(got))
}
