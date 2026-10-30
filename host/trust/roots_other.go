//go:build !(darwin && cgo)

package trust

import (
	"crypto/x509"
	"errors"
)

func Roots() ([]*x509.Certificate, error) { return nil, ErrUnsupported }

var (
	ErrKeyUnusable     = errors.New("the CA's key cannot be used by this build")
	ErrKeyOtherProgram = errors.New("the CA's key was made by another program than fev's key signer")
	ErrDeclined        = errors.New("the trust dialog was cancelled")
	ErrNoGUI           = errors.New("the trust dialog needs the Mac's own login session (not SSH)")
)

const DefaultKeyLabel = "dev.fevom.ca"

type KeychainOptions struct {
	Keychain, Label, RootPath string
	Root                      RootOptions
	AnyApplication            bool
	Signer                    string
}

func Identity() string                                { return "" }
func OpenKeychainCA(KeychainOptions) (*CA, error)     { return nil, ErrUnsupported }
func DeleteKeychainKey(string, string, string) error  { return ErrUnsupported }
func Install([]byte, string) error                    { return ErrUnsupported }
func Remove([]byte, string) error                     { return ErrUnsupported }
func State([]byte, string) (string, error)            { return "", ErrUnsupported }
func peekKey(string, string, string) (string, string) { return "none", "" }
