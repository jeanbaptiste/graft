// Package activitypub exposes graft's mirrored repos over ActivityPub: one
// actor per series (e.g. @federation-x@graft.cyberwild.org), so anyone on
// the fediverse can follow a repo and see its mirrored commits, issues, and
// patches show up as posts.
package activitypub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// GenerateKeyPair creates a new RSA-2048 keypair, PEM-encoded — RSA because
// that's what HTTP Signatures (the scheme ActivityPub implementations like
// Mastodon use to authenticate federated requests) expects in practice.
func GenerateKeyPair() (privatePEM, publicPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate rsa key: %w", err)
	}

	privBytes := x509.MarshalPKCS1PrivateKey(key)
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes})

	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal public key: %w", err)
	}
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	return string(priv), string(pub), nil
}

// ParsePrivateKey decodes a PEM-encoded RSA private key, as stored via
// GenerateKeyPair.
func ParsePrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// ParsePublicKey decodes a PEM-encoded RSA public key, e.g. one fetched
// from a remote actor's publicKeyPem field.
func ParsePublicKey(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not RSA")
	}
	return rsaKey, nil
}
