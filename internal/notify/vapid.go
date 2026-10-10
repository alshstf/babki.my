// Package notify sends reminders to the family's phones as web push (decision
// Р-27): a member turns them on for a device, the browser's push service gives
// it an address, and what is due on the credit cards (internal/creditcard)
// goes there — encrypted for that device alone, so the push service carries
// it without reading it. Each reminder goes to each member once.
package notify

import (
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"strconv"
)

// deriver is the encryption key's box: the push keys are worked out from it,
// so nothing more is stored and they last as long as it does.
type deriver interface {
	Derive(label string, n int) ([]byte, error)
}

// Keys are the server's push keys (VAPID), base64url: the public one is what a
// browser subscribes with, the private one signs every push.
type Keys struct {
	Public, Private string
}

// KeysFrom works the push keys out of the box. A derived scalar outside the
// curve's range — about one in four billion — moves on to the next label.
func KeysFrom(box deriver) (Keys, error) {
	for i := 0; i < 8; i++ {
		raw, err := box.Derive("web push vapid "+strconv.Itoa(i), 32)
		if err != nil {
			return Keys{}, err
		}
		key, err := ecdh.P256().NewPrivateKey(raw)
		if err != nil {
			continue
		}
		return Keys{
			Public:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			Private: base64.RawURLEncoding.EncodeToString(raw),
		}, nil
	}
	return Keys{}, fmt.Errorf("notify: no push key could be derived")
}
