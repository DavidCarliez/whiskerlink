package transfer

import (
	"strings"
	"testing"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

const fileInviteTestToken = "tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu"

func TestFileInviteRoundTrip(t *testing.T) {
	input := domain.FileInvite{Token: fileInviteTestToken, Label: "Release files"}
	encoded, err := EncodeFileInvite(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "whiskerlink://receive?") {
		t.Fatalf("encoded invite = %q", encoded)
	}
	got, err := ParseFileInvite(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Fatalf("parsed invite = %#v, want %#v", got, input)
	}
}

func TestParseFileInviteRejectsInvalidInput(t *testing.T) {
	for _, value := range []string{
		"whiskerlink://connect?token=" + fileInviteTestToken,
		"whiskerlink://receive?token=invalid",
		"https://example.com/?token=" + fileInviteTestToken,
	} {
		if _, err := ParseFileInvite(value); err == nil {
			t.Fatalf("ParseFileInvite(%q) succeeded", value)
		}
	}
}
