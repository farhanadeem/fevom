package main

import "fev/host/trust"

type keychain struct{}

func (keychain) State(der []byte) (string, error) { return trust.State(der, "") }
func (keychain) Remove(der []byte) error          { return trust.Remove(der, "") }

func (keychain) DeleteKey(signer string) error { return trust.DeleteKeychainKey("", "", signer) }
