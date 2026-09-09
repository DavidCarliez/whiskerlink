package tailnet

import (
	"errors"
	"fmt"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/DavidCarliez/whiskerlink/internal/storage"
)

type IdentityStore struct {
	store     *storage.Store
	namespace string
}

func NewIdentityStore(store *storage.Store) *IdentityStore {
	return NewNamedIdentityStore(store, "host-default")
}
func NewNamedIdentityStore(store *storage.Store, namespace string) *IdentityStore {
	return &IdentityStore{store: store, namespace: namespace}
}

func (i *IdentityStore) ConfigureServer(server *tailcat.Server) error {
	encoded, err := i.store.Secrets.Get("identity:" + i.namespace)
	if err != nil {
		if errors.Is(err, storage.ErrSecretNotFound) {
			server.Key = key.NewNode()
			return nil
		}
		return fmt.Errorf("load persistent identity: %w", err)
	}
	if err := server.Key.UnmarshalText([]byte(encoded)); err != nil {
		return fmt.Errorf("parse persistent identity: %w", err)
	}
	if oldToken, err := i.store.Secrets.Get("identity-token:" + i.namespace); err == nil {
		if info, err := tailcat.ParseConnBlob(tailcat.ConnBlob(oldToken)); err == nil {
			server.RegionID = info.RegionID
			if server.RegionID == 0 && len(info.Region) > 0 {
				server.Region = info.Region[0]
			}
		}
	}
	return nil
}

func (i *IdentityStore) SaveServer(private key.NodePrivate, token string) error {
	encoded, err := private.MarshalText()
	if err != nil {
		return fmt.Errorf("encode persistent identity: %w", err)
	}
	if err := i.store.Secrets.Set("identity:"+i.namespace, string(encoded)); err != nil {
		return fmt.Errorf("store persistent identity in credential manager: %w", err)
	}
	if err := i.store.Secrets.Set("identity-token:"+i.namespace, token); err != nil {
		return fmt.Errorf("store persistent address in credential manager: %w", err)
	}
	return nil
}
