package tailnet

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tailscale/tailcat"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

const ServiceInviteScheme = "whiskerlink"

func EncodeServiceInvite(invite domain.ServiceInvite) (string, error) {
	invite, err := validateServiceInvite(invite)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("token", invite.Token)
	query.Set("port", strconv.Itoa(int(invite.RemotePort)))
	query.Set("type", invite.ServiceType)
	if invite.Label != "" {
		query.Set("label", invite.Label)
	}
	return (&url.URL{Scheme: ServiceInviteScheme, Host: "connect", RawQuery: query.Encode()}).String(), nil
}

func ParseServiceInvite(value string) (domain.ServiceInvite, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return domain.ServiceInvite{}, fmt.Errorf("invalid WhiskerLink invite: %w", err)
	}
	if parsed.Scheme != ServiceInviteScheme || parsed.Host != "connect" || parsed.Path != "" {
		return domain.ServiceInvite{}, errors.New("invite must start with whiskerlink://connect")
	}
	port, err := strconv.ParseUint(parsed.Query().Get("port"), 10, 16)
	if err != nil || port == 0 {
		return domain.ServiceInvite{}, errors.New("invite has an invalid remote port")
	}
	return validateServiceInvite(domain.ServiceInvite{
		Token:       parsed.Query().Get("token"),
		RemotePort:  uint16(port),
		Label:       parsed.Query().Get("label"),
		ServiceType: parsed.Query().Get("type"),
	})
}

func validateServiceInvite(invite domain.ServiceInvite) (domain.ServiceInvite, error) {
	invite.Token = strings.TrimSpace(invite.Token)
	invite.Label = strings.TrimSpace(invite.Label)
	var err error
	invite.ServiceType, err = normalizeServiceType(invite.ServiceType)
	if err != nil {
		return domain.ServiceInvite{}, err
	}
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(invite.Token)); err != nil {
		return domain.ServiceInvite{}, fmt.Errorf("invite contains an invalid Tailcat token: %w", err)
	}
	if invite.RemotePort == 0 {
		return domain.ServiceInvite{}, errors.New("invite has an invalid remote port")
	}
	if len(invite.Label) > 120 {
		return domain.ServiceInvite{}, errors.New("invite label is too long")
	}
	return invite, nil
}

func normalizeServiceType(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "tcp", nil
	}
	switch value {
	case "http", "https", "tcp":
		return value, nil
	default:
		return "", errors.New("invite has an unsupported service type")
	}
}
