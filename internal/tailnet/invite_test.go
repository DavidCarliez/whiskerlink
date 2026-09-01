package tailnet

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

const testTailcatToken = "tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu"

func TestServiceInviteRoundTrip(t *testing.T) {
	input := domain.ServiceInvite{
		Token: testTailcatToken, RemotePort: 8443, Label: "Local dashboard", ServiceType: "https",
	}
	encoded, err := EncodeServiceInvite(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "whiskerlink://connect?") {
		t.Fatalf("encoded invite = %q", encoded)
	}
	got, err := ParseServiceInvite(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Fatalf("parsed invite = %#v, want %#v", got, input)
	}
}

func TestParseServiceInviteRejectsInvalidInput(t *testing.T) {
	for _, value := range []string{
		"https://connect.example/?token=" + testTailcatToken + "&port=80&type=http",
		"whiskerlink://connect?token=invalid&port=80&type=http",
		"whiskerlink://connect?token=" + testTailcatToken + "&port=0&type=http",
		"whiskerlink://connect?token=" + testTailcatToken + "&port=80&type=database",
	} {
		if _, err := ParseServiceInvite(value); err == nil {
			t.Fatalf("ParseServiceInvite(%q) succeeded", value)
		}
	}
}

func TestConnectServiceChoosesFreeLocalPort(t *testing.T) {
	manager := NewManager(nil, nil)
	session, err := manager.ConnectService(context.Background(), ConnectServiceRequest{
		Token: testTailcatToken, RemotePort: 8080, LocalPort: 0, ServiceType: "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.StopAll)
	host, port, err := net.SplitHostPort(session.LocalAddress)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port == "0" {
		t.Fatalf("local address = %q, want allocated loopback port", session.LocalAddress)
	}
	if session.ServiceType != "http" {
		t.Fatalf("service type = %q, want http", session.ServiceType)
	}
}
