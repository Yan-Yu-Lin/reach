package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"

	"golang.org/x/crypto/ssh"
)

// parseTunnelPrivateKey accepts the key formats produced by both modern and
// old OpenSSH/ssh-keygen. In particular, RHEL/CentOS 6's OpenSSH 5.3 can write
// ECDSA PEM keys with explicit EC parameters instead of a named-curve OID;
// Go's x509 parser (used by x/crypto/ssh) rejects those with
// "x509: unknown elliptic curve" even though OpenSSH can use them.
func parseTunnelPrivateKey(key []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(key)
	if err == nil {
		return signer, nil
	}

	if fallbackSigner, fallbackErr := parseExplicitECPrivateKey(key); fallbackErr == nil {
		return fallbackSigner, nil
	} else {
		return nil, fmt.Errorf("%w; explicit EC fallback failed: %v", err, fallbackErr)
	}
}

type sec1ECPrivateKey struct {
	Version    int
	PrivateKey []byte
	Parameters asn1.RawValue  `asn1:"optional,explicit,tag:0"`
	PublicKey  asn1.BitString `asn1:"optional,explicit,tag:1"`
}

type explicitECDomain struct {
	Version  int
	FieldID  explicitECFieldID
	Curve    explicitECCurve
	Base     []byte
	Order    *big.Int
	Cofactor int `asn1:"optional"`
}

type explicitECFieldID struct {
	FieldType  asn1.ObjectIdentifier
	Parameters asn1.RawValue
}

type explicitECCurve struct {
	A    []byte
	B    []byte
	Seed asn1.BitString `asn1:"optional"`
}

var oidPrimeField = asn1.ObjectIdentifier{1, 2, 840, 10045, 1, 1}

func parseExplicitECPrivateKey(key []byte) (ssh.Signer, error) {
	var block *pem.Block
	for len(key) > 0 {
		block, key = pem.Decode(key)
		if block == nil {
			break
		}
		if block.Type == "EC PRIVATE KEY" {
			break
		}
	}
	if block == nil {
		return nil, fmt.Errorf("not PEM")
	}
	if block.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("PEM does not contain EC PRIVATE KEY")
	}

	var ecKey sec1ECPrivateKey
	if rest, err := asn1.Unmarshal(block.Bytes, &ecKey); err != nil {
		return nil, err
	} else if len(rest) != 0 {
		return nil, fmt.Errorf("trailing data after EC private key")
	}
	if len(ecKey.PrivateKey) == 0 {
		return nil, fmt.Errorf("missing EC private scalar")
	}
	if len(ecKey.Parameters.Bytes) == 0 {
		return nil, fmt.Errorf("missing EC parameters")
	}

	curve, err := parseExplicitCurve(ecKey.Parameters.Bytes)
	if err != nil {
		return nil, err
	}
	params := curve.Params()
	priv := new(big.Int).SetBytes(ecKey.PrivateKey)
	if priv.Sign() <= 0 || priv.Cmp(params.N) >= 0 {
		return nil, fmt.Errorf("invalid EC private scalar")
	}

	x, y := curve.ScalarBaseMult(ecKey.PrivateKey)
	if x == nil || y == nil {
		return nil, fmt.Errorf("failed to derive EC public key")
	}
	if len(ecKey.PublicKey.Bytes) != 0 {
		pubX, pubY := elliptic.Unmarshal(curve, ecKey.PublicKey.Bytes)
		if pubX == nil || pubY == nil {
			return nil, fmt.Errorf("invalid EC public key point")
		}
		if pubX.Cmp(x) != 0 || pubY.Cmp(y) != 0 {
			return nil, fmt.Errorf("EC public key does not match private scalar")
		}
	}

	return ssh.NewSignerFromKey(&ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         priv,
	})
}

func parseExplicitCurve(der []byte) (elliptic.Curve, error) {
	var domain explicitECDomain
	if rest, err := asn1.Unmarshal(der, &domain); err != nil {
		return nil, err
	} else if len(rest) != 0 {
		return nil, fmt.Errorf("trailing data after EC domain parameters")
	}

	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()} {
		if explicitDomainMatchesCurve(domain, curve) {
			return curve, nil
		}
	}
	return nil, fmt.Errorf("unsupported explicit EC domain")
}

func explicitDomainMatchesCurve(domain explicitECDomain, curve elliptic.Curve) bool {
	if domain.Version != 1 || !domain.FieldID.FieldType.Equal(oidPrimeField) || domain.Order == nil {
		return false
	}
	params := curve.Params()
	p := new(big.Int).SetBytes(domain.FieldID.Parameters.Bytes)
	if p.Cmp(params.P) != 0 || domain.Order.Cmp(params.N) != 0 {
		return false
	}
	if domain.Cofactor != 0 && domain.Cofactor != 1 {
		return false
	}

	wantA := new(big.Int).Sub(params.P, big.NewInt(3))
	if new(big.Int).SetBytes(domain.Curve.A).Cmp(wantA) != 0 {
		return false
	}
	if new(big.Int).SetBytes(domain.Curve.B).Cmp(params.B) != 0 {
		return false
	}

	pointLen := (params.BitSize + 7) / 8
	if len(domain.Base) != 1+2*pointLen || domain.Base[0] != 4 {
		return false
	}
	gx := new(big.Int).SetBytes(domain.Base[1 : 1+pointLen])
	gy := new(big.Int).SetBytes(domain.Base[1+pointLen:])
	return gx.Cmp(params.Gx) == 0 && gy.Cmp(params.Gy) == 0
}
