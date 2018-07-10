package vault

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/hashicorp/vault/api"
	"github.com/kubevault/unsealer/pkg/kv"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// vault is an implementation of the Vault interface that will perform actions
// against a Vault server, using a provided KMS to retrieve
type vault struct {
	keyStore kv.Service
	cl       *api.Client
	config   *VaultOptions
}

var _ Vault = &vault{}

// Vault is an interface that can be used to attempt to perform actions against
// a Vault server.
type Vault interface {
	Sealed() (bool, error)
	Unseal() error
	Init() error
	CheckReadWriteAccess() error
}

// New returns a new vault Vault, or an error.
func New(k kv.Service, cl *api.Client, config VaultOptions) (Vault, error) {
	return &vault{
		keyStore: k,
		cl:       cl,
		config:   &config,
	}, nil
}

func (u *vault) Sealed() (bool, error) {
	resp, err := u.cl.Sys().SealStatus()
	if err != nil {
		return false, fmt.Errorf("error checking status: %s", err.Error())
	}
	return resp.Sealed, nil
}

// Unseal will attempt to unseal vault by retrieving keys from the kms service
// and sending unseal requests to vault. It will return an error if retrieving
// a key fails, or if the unseal progress is reset to 0 (indicating that a key)
// was invalid.
func (u *vault) Unseal() error {
	for i := 0; ; i++ {
		keyID := u.unsealKeyForID(i)

		logrus.Debugf("retrieving key from kms service...")
		k, err := u.keyStore.Get(keyID)

		if err != nil {
			return fmt.Errorf("unable to get key '%s': %s", keyID, err.Error())
		}

		logrus.Debugf("sending unseal request to vault...")
		resp, err := u.cl.Sys().Unseal(string(k))

		if err != nil {
			return fmt.Errorf("fail to send unseal request to vault: %s", err.Error())
		}

		logrus.Debugf("got unseal response: %+v", *resp)

		if !resp.Sealed {
			return nil
		}

		// if progress is 0, we failed to unseal vault.
		if resp.Progress == 0 {
			return fmt.Errorf("failed to unseal vault. progress reset to 0")
		}
	}
}

func (u *vault) keyStoreNotFound(key string) bool {
	_, err := u.keyStore.Get(key)
	if err != nil {
		glog.Errorf("error response when checking whether key(%s) exists or not: %v", key, err)
	}
	if _, ok := err.(*kv.NotFoundError); ok {
		return true
	}
	return false
}

func (u *vault) keyStoreSet(key string, val []byte) error {
	if !u.config.OverwriteExisting && !u.keyStoreNotFound(key) {
		return fmt.Errorf("error setting key '%s': it already exists or encounter error when getting key", key)
	}
	return u.keyStore.Set(key, val)
}

func (u *vault) Init() error {
	// test backend first
	err := u.keyStore.Test(u.testKey())
	if err != nil {
		return fmt.Errorf("error testing keystore before init: %s", err.Error())
	}

	// test for an existing keys
	if !u.config.OverwriteExisting {
		keys := []string{
			u.rootTokenKey(),
		}

		// add unseal keys
		for i := 0; i <= u.config.SecretShares; i++ {
			keys = append(keys, u.unsealKeyForID(i))
		}

		// test every key
		for _, key := range keys {
			if !u.keyStoreNotFound(key) {
				return fmt.Errorf("error before init: keystore value for '%s' already exists or encounter error when getting key", key)
			}
		}
	}

	resp, err := u.cl.Sys().Init(&api.InitRequest{
		SecretShares:    u.config.SecretShares,
		SecretThreshold: u.config.SecretThreshold,
	})

	if err != nil {
		return fmt.Errorf("error initialising vault: %s", err.Error())
	}

	for i, k := range resp.Keys {
		keyID := u.unsealKeyForID(i)
		err := u.keyStoreSet(keyID, []byte(k))

		if err != nil {
			return fmt.Errorf("error storing unseal key '%s': %s", keyID, err.Error())
		}
	}

	rootToken := resp.RootToken

	if u.config.StoreRootToken {
		rootTokenKey := u.rootTokenKey()
		if err = u.keyStoreSet(rootTokenKey, []byte(resp.RootToken)); err != nil {
			return fmt.Errorf("error storing root token '%s' in key'%s'", rootToken, rootTokenKey)
		}
		logrus.WithField("key", rootTokenKey).Info("root token stored in key store")
	} else {
		logrus.WithField("root-token", resp.RootToken).Warnf("won't store root token in key store, this token grants full privileges to vault, so keep this secret")
	}

	return nil

}

// CheckReadWriteAccess will test read write access
func (u *vault) CheckReadWriteAccess() error {
	glog.Infoln("Testing the read/write access...")

	err := u.keyStore.CheckWriteAccess()
	if err != nil {
		return errors.Wrap(err, "read/write access test failed")
	}

	glog.Infoln("Testing the read/write access is successful")
	return nil
}

func (u *vault) unsealKeyForID(i int) string {
	return fmt.Sprintf("%s-unseal-%d", u.config.KeyPrefix, i)
}

func (u *vault) rootTokenKey() string {
	return fmt.Sprintf("%s-root", u.config.KeyPrefix)
}

func (u *vault) testKey() string {
	return fmt.Sprintf("%s-test", u.config.KeyPrefix)
}
