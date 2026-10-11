package deploy

import (
	"github.com/alinz/script.go"
)

var Config = &script.Config{
	User: "bot",
	Host: "farsi.ella.to",
	Port: 22,
	// SSH_PRIVATE_KEY holds the key contents (that is what CI sets). When it
	// is unset — a manual deploy from a laptop — the key is read from this
	// file instead.
	PrivateKeyEnv:    "SSH_PRIVATE_KEY",
	DefaultLocalPath: "~/.ssh/id_ed25519",
}
