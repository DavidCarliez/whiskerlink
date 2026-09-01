package transfer

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tailscale/tailcat"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

func EncodeFileInvite(invite domain.FileInvite) (string, error) {
	invite, err := validateFileInvite(invite)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("token", invite.Token)
	if invite.Label != "" {
		query.Set("label", invite.Label)
	}
	return (&url.URL{Scheme: "whiskerlink", Host: "receive", RawQuery: query.Encode()}).String(), nil
}

func ParseFileInvite(value string) (domain.FileInvite, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return domain.FileInvite{}, fmt.Errorf("invalid WhiskerLink invite: %w", err)
	}
	if parsed.Scheme != "whiskerlink" || parsed.Host != "receive" || parsed.Path != "" {
		return domain.FileInvite{}, errors.New("invite must start with whiskerlink://receive")
	}
	return validateFileInvite(domain.FileInvite{
		Token: parsed.Query().Get("token"),
		Label: parsed.Query().Get("label"),
	})
}

func validateFileInvite(invite domain.FileInvite) (domain.FileInvite, error) {
	invite.Token = strings.TrimSpace(invite.Token)
	invite.Label = strings.TrimSpace(invite.Label)
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(invite.Token)); err != nil {
		return domain.FileInvite{}, fmt.Errorf("invite contains an invalid Tailcat token: %w", err)
	}
	if len(invite.Label) > 120 {
		return domain.FileInvite{}, errors.New("invite label is too long")
	}
	return invite, nil
}
