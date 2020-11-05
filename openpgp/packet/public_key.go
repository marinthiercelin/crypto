// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package packet

import (
	"bytes"
	"crypto"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math/big"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/openpgp/ecdh"
	"golang.org/x/crypto/openpgp/elgamal"
	"golang.org/x/crypto/openpgp/errors"
	"golang.org/x/crypto/openpgp/internal/algorithm"
	"golang.org/x/crypto/openpgp/internal/ecc"
	"golang.org/x/crypto/openpgp/internal/encoding"
	"golang.org/x/crypto/rsa"
	sha1 "golang.org/x/crypto/sha1"
	sha256 "golang.org/x/crypto/sha256"
	sha512 "golang.org/x/crypto/sha512"
)

func init() {
	fmt.Println("init packet")
	crypto.RegisterHash(crypto.SHA1, sha1.New)
	crypto.RegisterHash(crypto.SHA256, sha256.New)
	crypto.RegisterHash(crypto.SHA512, sha512.New)
}

type kdfHashFunction byte
type kdfAlgorithm byte

// PublicKey represents an OpenPGP public key. See RFC 4880, section 5.5.2.
type PublicKey struct {
	Version      int
	CreationTime time.Time
	PubKeyAlgo   PublicKeyAlgorithm
	PublicKey    interface{} // *rsa.PublicKey, *dsa.PublicKey, *ecdsa.PublicKey or *eddsa.PublicKey
	Fingerprint  []byte
	KeyId        uint64
	IsSubkey     bool

	// RFC 4880 fields
	n, e, p, q, g, y encoding.Field

	// RFC 6637 fields
	// oid contains the OID byte sequence identifying the elliptic curve used
	oid encoding.Field

	// kdf stores key derivation function parameters
	// used for ECDH encryption. See RFC 6637, Section 9.
	kdf encoding.Field
}

// UpgradeToV5 updates the version of the key to v5, and updates all necessary
// fields.
func (pk *PublicKey) UpgradeToV5() {
	pk.Version = 5
	pk.setFingerprintAndKeyId()
}

// signingKey provides a convenient abstraction over signature verification
// for v3 and v4 public keys.
type signingKey interface {
	SerializeForHash(io.Writer) error
	SerializeSignaturePrefix(io.Writer)
	serializeWithoutHeaders(io.Writer) error
}

// NewRSAPublicKey returns a PublicKey that wraps the given rsa.PublicKey.
func NewRSAPublicKey(creationTime time.Time, pub *rsa.PublicKey) *PublicKey {
	pk := &PublicKey{
		Version:      4,
		CreationTime: creationTime,
		PubKeyAlgo:   PubKeyAlgoRSA,
		PublicKey:    pub,
		n:            new(encoding.MPI).SetBig(pub.N),
		e:            new(encoding.MPI).SetBig(big.NewInt(int64(pub.E))),
	}

	pk.setFingerprintAndKeyId()
	return pk
}

// NewDSAPublicKey returns a PublicKey that wraps the given dsa.PublicKey.
func NewDSAPublicKey(creationTime time.Time, pub *dsa.PublicKey) *PublicKey {
	pk := &PublicKey{
		Version:      4,
		CreationTime: creationTime,
		PubKeyAlgo:   PubKeyAlgoDSA,
		PublicKey:    pub,
		p:            new(encoding.MPI).SetBig(pub.P),
		q:            new(encoding.MPI).SetBig(pub.Q),
		g:            new(encoding.MPI).SetBig(pub.G),
		y:            new(encoding.MPI).SetBig(pub.Y),
	}

	pk.setFingerprintAndKeyId()
	return pk
}

// NewElGamalPublicKey returns a PublicKey that wraps the given elgamal.PublicKey.
func NewElGamalPublicKey(creationTime time.Time, pub *elgamal.PublicKey) *PublicKey {
	pk := &PublicKey{
		Version:      4,
		CreationTime: creationTime,
		PubKeyAlgo:   PubKeyAlgoElGamal,
		PublicKey:    pub,
		p:            new(encoding.MPI).SetBig(pub.P),
		g:            new(encoding.MPI).SetBig(pub.G),
		y:            new(encoding.MPI).SetBig(pub.Y),
	}

	pk.setFingerprintAndKeyId()
	return pk
}

func NewECDSAPublicKey(creationTime time.Time, pub *ecdsa.PublicKey) *PublicKey {
	pk := &PublicKey{
		Version:      4,
		CreationTime: creationTime,
		PubKeyAlgo:   PubKeyAlgoECDSA,
		PublicKey:    pub,
		p:            encoding.NewMPI(elliptic.Marshal(pub.Curve, pub.X, pub.Y)),
	}

	curveInfo := ecc.FindByCurve(pub.Curve)
	if curveInfo == nil {
		panic("unknown elliptic curve")
	}
	pk.oid = curveInfo.Oid
	pk.setFingerprintAndKeyId()
	return pk
}

func NewECDHPublicKey(creationTime time.Time, pub *ecdh.PublicKey) *PublicKey {
	var pk *PublicKey
	var curveInfo *ecc.CurveInfo
	var kdf = encoding.NewOID([]byte{0x1, pub.Hash.Id(), pub.Cipher.Id()})
	if pub.CurveType == ecc.Curve25519 {
		pk = &PublicKey{
			Version:      4,
			CreationTime: creationTime,
			PubKeyAlgo:   PubKeyAlgoECDH,
			PublicKey:    pub,
			p:            encoding.NewMPI(pub.X.Bytes()),
			kdf:          kdf,
		}
		curveInfo = ecc.FindByName("Curve25519")
	} else {
		pk = &PublicKey{
			Version:      4,
			CreationTime: creationTime,
			PubKeyAlgo:   PubKeyAlgoECDH,
			PublicKey:    pub,
			p:            encoding.NewMPI(elliptic.Marshal(pub.Curve, pub.X, pub.Y)),
			kdf:          kdf,
		}
		curveInfo = ecc.FindByCurve(pub.Curve)
	}
	if curveInfo == nil {
		panic("unknown elliptic curve")
	}
	pk.oid = curveInfo.Oid
	pk.setFingerprintAndKeyId()
	return pk
}

func NewEdDSAPublicKey(creationTime time.Time, pub *ed25519.PublicKey) *PublicKey {
	curveInfo := ecc.FindByName("Ed25519")
	pk := &PublicKey{
		Version:      4,
		CreationTime: creationTime,
		PubKeyAlgo:   PubKeyAlgoEdDSA,
		PublicKey:    pub,
		oid:          curveInfo.Oid,
		// Native point format, see draft-koch-eddsa-for-openpgp-04, Appendix B
		p: encoding.NewMPI(append([]byte{0x40}, *pub...)),
	}

	pk.setFingerprintAndKeyId()
	return pk
}

func (pk *PublicKey) parse(r io.Reader) (err error) {
	// RFC 4880, section 5.5.2
	var buf [6]byte
	_, err = readFull(r, buf[:])
	if err != nil {
		return
	}
	if buf[0] != 4 && buf[0] != 5 {
		return errors.UnsupportedError("public key version " + strconv.Itoa(int(buf[0])))
	}

	pk.Version = int(buf[0])
	if pk.Version == 5 {
		var n [4]byte
		_, err = readFull(r, n[:])
		if err != nil {
			return
		}
	}
	pk.CreationTime = time.Unix(int64(uint32(buf[1])<<24|uint32(buf[2])<<16|uint32(buf[3])<<8|uint32(buf[4])), 0)
	pk.PubKeyAlgo = PublicKeyAlgorithm(buf[5])
	switch pk.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoRSASignOnly:
		err = pk.parseRSA(r)
	case PubKeyAlgoDSA:
		err = pk.parseDSA(r)
	case PubKeyAlgoElGamal:
		err = pk.parseElGamal(r)
	case PubKeyAlgoECDSA:
		err = pk.parseECDSA(r)
	case PubKeyAlgoECDH:
		err = pk.parseECDH(r)
	case PubKeyAlgoEdDSA:
		err = pk.parseEdDSA(r)
	default:
		err = errors.UnsupportedError("public key type: " + strconv.Itoa(int(pk.PubKeyAlgo)))
	}
	if err != nil {
		return
	}

	pk.setFingerprintAndKeyId()
	return
}

func (pk *PublicKey) setFingerprintAndKeyId() {
	// RFC 4880, section 12.2
	if pk.Version == 5 {
		buffer := new(bytes.Buffer)
		pk.SerializeForHash(buffer)
		pk.Fingerprint = make([]byte, 32)
		h := sha256.Sum256(buffer.Bytes())
		copy(pk.Fingerprint, h[:])
		pk.KeyId = binary.BigEndian.Uint64(pk.Fingerprint[:8])
	} else {
		buffer := new(bytes.Buffer)
		pk.SerializeForHash(buffer)
		pk.Fingerprint = make([]byte, 20)
		h := sha1.Sum(buffer.Bytes())
		copy(pk.Fingerprint, h[:])
		pk.KeyId = binary.BigEndian.Uint64(pk.Fingerprint[12:20])
	}
	fmt.Printf("keyid: %x\n", pk.KeyId)
	fmt.Printf("fingerprint: %x\n", pk.Fingerprint)
}

// parseRSA parses RSA public key material from the given Reader. See RFC 4880,
// section 5.5.2.
func (pk *PublicKey) parseRSA(r io.Reader) (err error) {
	pk.n = new(encoding.MPI)
	if _, err = pk.n.ReadFrom(r); err != nil {
		return
	}
	pk.e = new(encoding.MPI)
	if _, err = pk.e.ReadFrom(r); err != nil {
		return
	}

	if len(pk.e.Bytes()) > 3 {
		err = errors.UnsupportedError("large public exponent")
		return
	}
	rsa := &rsa.PublicKey{
		N: new(big.Int).SetBytes(pk.n.Bytes()),
		E: 0,
	}
	for i := 0; i < len(pk.e.Bytes()); i++ {
		rsa.E <<= 8
		rsa.E |= int(pk.e.Bytes()[i])
	}
	pk.PublicKey = rsa
	return
}

// parseDSA parses DSA public key material from the given Reader. See RFC 4880,
// section 5.5.2.
func (pk *PublicKey) parseDSA(r io.Reader) (err error) {
	pk.p = new(encoding.MPI)
	if _, err = pk.p.ReadFrom(r); err != nil {
		return
	}
	pk.q = new(encoding.MPI)
	if _, err = pk.q.ReadFrom(r); err != nil {
		return
	}
	pk.g = new(encoding.MPI)
	if _, err = pk.g.ReadFrom(r); err != nil {
		return
	}
	pk.y = new(encoding.MPI)
	if _, err = pk.y.ReadFrom(r); err != nil {
		return
	}

	dsa := new(dsa.PublicKey)
	dsa.P = new(big.Int).SetBytes(pk.p.Bytes())
	dsa.Q = new(big.Int).SetBytes(pk.q.Bytes())
	dsa.G = new(big.Int).SetBytes(pk.g.Bytes())
	dsa.Y = new(big.Int).SetBytes(pk.y.Bytes())
	pk.PublicKey = dsa
	return
}

// parseElGamal parses ElGamal public key material from the given Reader. See
// RFC 4880, section 5.5.2.
func (pk *PublicKey) parseElGamal(r io.Reader) (err error) {
	pk.p = new(encoding.MPI)
	if _, err = pk.p.ReadFrom(r); err != nil {
		return
	}
	pk.g = new(encoding.MPI)
	if _, err = pk.g.ReadFrom(r); err != nil {
		return
	}
	pk.y = new(encoding.MPI)
	if _, err = pk.y.ReadFrom(r); err != nil {
		return
	}

	elgamal := new(elgamal.PublicKey)
	elgamal.P = new(big.Int).SetBytes(pk.p.Bytes())
	elgamal.G = new(big.Int).SetBytes(pk.g.Bytes())
	elgamal.Y = new(big.Int).SetBytes(pk.y.Bytes())
	pk.PublicKey = elgamal
	return
}

// parseECDSA parses ECDSA public key material from the given Reader. See
// RFC 6637, Section 9.
func (pk *PublicKey) parseECDSA(r io.Reader) (err error) {
	pk.oid = new(encoding.OID)
	if _, err = pk.oid.ReadFrom(r); err != nil {
		return
	}
	pk.p = new(encoding.MPI)
	if _, err = pk.p.ReadFrom(r); err != nil {
		return
	}

	var c elliptic.Curve
	curveInfo := ecc.FindByOid(pk.oid)
	if curveInfo == nil || curveInfo.SigAlgorithm != ecc.ECDSA {
		return errors.UnsupportedError(fmt.Sprintf("unsupported oid: %x", pk.oid))
	}
	c = curveInfo.Curve
	x, y := elliptic.Unmarshal(c, pk.p.Bytes())
	if x == nil {
		return errors.UnsupportedError("failed to parse EC point")
	}
	pk.PublicKey = &ecdsa.PublicKey{Curve: c, X: x, Y: y}
	return
}

// parseECDH parses ECDH public key material from the given Reader. See
// RFC 6637, Section 9.
func (pk *PublicKey) parseECDH(r io.Reader) (err error) {
	pk.oid = new(encoding.OID)
	if _, err = pk.oid.ReadFrom(r); err != nil {
		return
	}
	pk.p = new(encoding.MPI)
	if _, err = pk.p.ReadFrom(r); err != nil {
		return
	}
	pk.kdf = new(encoding.OID)
	if _, err = pk.kdf.ReadFrom(r); err != nil {
		return
	}

	curveInfo := ecc.FindByOid(pk.oid)
	if curveInfo == nil {
		return errors.UnsupportedError(fmt.Sprintf("unsupported oid: %x", pk.oid))
	}

	c := curveInfo.Curve
	cType := curveInfo.CurveType

	var x, y *big.Int
	if cType == ecc.Curve25519 {
		x = new(big.Int)
		x.SetBytes(pk.p.Bytes())
	} else {
		x, y = elliptic.Unmarshal(c, pk.p.Bytes())
	}
	if x == nil {
		return errors.UnsupportedError("failed to parse EC point")
	}

	if kdfLen := len(pk.kdf.Bytes()); kdfLen < 3 {
		return errors.UnsupportedError("unsupported ECDH KDF length: " + strconv.Itoa(kdfLen))
	}
	if reserved := pk.kdf.Bytes()[0]; reserved != 0x01 {
		return errors.UnsupportedError("unsupported KDF reserved field: " + strconv.Itoa(int(reserved)))
	}
	kdfHash, ok := algorithm.HashById[pk.kdf.Bytes()[1]]
	if !ok {
		return errors.UnsupportedError("unsupported ECDH KDF hash: " + strconv.Itoa(int(pk.kdf.Bytes()[1])))
	}
	kdfCipher, ok := algorithm.CipherById[pk.kdf.Bytes()[2]]
	if !ok {
		return errors.UnsupportedError("unsupported ECDH KDF cipher: " + strconv.Itoa(int(pk.kdf.Bytes()[2])))
	}

	pk.PublicKey = &ecdh.PublicKey{
		CurveType: cType,
		Curve:     c,
		X:         x,
		Y:         y,
		KDF: ecdh.KDF{
			Hash:   kdfHash,
			Cipher: kdfCipher,
		},
	}
	return
}

func (pk *PublicKey) parseEdDSA(r io.Reader) (err error) {
	pk.oid = new(encoding.OID)
	if _, err = pk.oid.ReadFrom(r); err != nil {
		return
	}
	curveInfo := ecc.FindByOid(pk.oid)
	if curveInfo == nil || curveInfo.SigAlgorithm != ecc.EdDSA {
		return errors.UnsupportedError(fmt.Sprintf("unsupported oid: %x", pk.oid))
	}
	pk.p = new(encoding.MPI)
	if _, err = pk.p.ReadFrom(r); err != nil {
		return
	}

	eddsa := make(ed25519.PublicKey, ed25519.PublicKeySize)
	switch flag := pk.p.Bytes()[0]; flag {
	case 0x04:
		// TODO: see _grcy_ecc_eddsa_ensure_compact in grcypt
		return errors.UnsupportedError("unsupported EdDSA compression: " + strconv.Itoa(int(flag)))
	case 0x40:
		copy(eddsa[:], pk.p.Bytes()[1:])
	default:
		return errors.UnsupportedError("unsupported EdDSA compression: " + strconv.Itoa(int(flag)))
	}

	pk.PublicKey = &eddsa
	return
}

// SerializeForHash serializes the PublicKey to w with the special packet
// header format needed for hashing.
func (pk *PublicKey) SerializeForHash(w io.Writer) error {
	fmt.Println("pk415")
	pk.SerializeSignaturePrefix(w)
	fmt.Println("pk417")
	return pk.serializeWithoutHeaders(w)
}

// SerializeSignaturePrefix writes the prefix for this public key to the given Writer.
// The prefix is used when calculating a signature over this public key. See
// RFC 4880, section 5.2.4.
func (pk *PublicKey) SerializeSignaturePrefix(w io.Writer) {
	fmt.Println("pk462")
	var pLength = pk.algorithmSpecificByteCount()
	if pk.Version == 5 {
		pLength += 10 // version, timestamp (4), algorithm, key octet count (4).
		println("writing to hash 1")
		w.Write([]byte{
			0x9A,
			byte(pLength >> 24),
			byte(pLength >> 16),
			byte(pLength >> 8),
			byte(pLength),
		})
		println("done writing")
		return
	}
	pLength += 6
	fmt.Println("pk475")
	println("writing to hash 2")
	w.Write([]byte{0x99, byte(pLength >> 8), byte(pLength)})
	println("done writing")
}

func (pk *PublicKey) Serialize(w io.Writer) (err error) {
	length := 6 // 6 byte header
	length += pk.algorithmSpecificByteCount()
	if pk.Version == 5 {
		length += 4 // octet key count
	}
	packetType := packetTypePublicKey
	if pk.IsSubkey {
		packetType = packetTypePublicSubkey
	}
	err = serializeHeader(w, packetType, length)
	if err != nil {
		return
	}
	return pk.serializeWithoutHeaders(w)
}

func (pk *PublicKey) algorithmSpecificByteCount() int {
	length := 0
	switch pk.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoRSASignOnly:
		length += int(pk.n.EncodedLength())
		length += int(pk.e.EncodedLength())
	case PubKeyAlgoDSA:
		length += int(pk.p.EncodedLength())
		length += int(pk.q.EncodedLength())
		length += int(pk.g.EncodedLength())
		length += int(pk.y.EncodedLength())
	case PubKeyAlgoElGamal:
		length += int(pk.p.EncodedLength())
		length += int(pk.g.EncodedLength())
		length += int(pk.y.EncodedLength())
	case PubKeyAlgoECDSA:
		length += int(pk.oid.EncodedLength())
		length += int(pk.p.EncodedLength())
	case PubKeyAlgoECDH:
		length += int(pk.oid.EncodedLength())
		length += int(pk.p.EncodedLength())
		length += int(pk.kdf.EncodedLength())
	case PubKeyAlgoEdDSA:
		length += int(pk.oid.EncodedLength())
		length += int(pk.p.EncodedLength())
	default:
		panic("unknown public key algorithm")
	}
	return length
}

// serializeWithoutHeaders marshals the PublicKey to w in the form of an
// OpenPGP public key packet, not including the packet header.
func (pk *PublicKey) serializeWithoutHeaders(w io.Writer) (err error) {
	// debug.PrintStack()
	println("pk531")
	t := uint32(pk.CreationTime.Unix())
	println("writing to hash 3")
	if _, err = w.Write([]byte{
		byte(pk.Version),
		byte(t >> 24), byte(t >> 16), byte(t >> 8), byte(t),
		byte(pk.PubKeyAlgo),
	}); err != nil {
		println("pk538")
		// println(err)
		return
	}
	println("done writing")
	println("pk540")
	if pk.Version == 5 {
		n := pk.algorithmSpecificByteCount()
		println("writing to hash 5")
		if _, err = w.Write([]byte{
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
		}); err != nil {
			return
		}
		println("done writing")
	}
	println("pk549")
	switch pk.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoRSASignOnly:
		println("pk520")
		println("writing to hash 6")
		if _, err = w.Write(pk.n.EncodedBytes()); err != nil {
			return
		}
		println("done writing")
		println("pk524")
		println("writing to hash 7")
		_, err = w.Write(pk.e.EncodedBytes())
		println("done writing")
		println("pk526")
		return
	case PubKeyAlgoDSA:
		println("pk529")
		if _, err = w.Write(pk.p.EncodedBytes()); err != nil {
			return
		}
		println("pk533")
		if _, err = w.Write(pk.q.EncodedBytes()); err != nil {
			return
		}
		println("pk536")
		if _, err = w.Write(pk.g.EncodedBytes()); err != nil {
			return
		}
		println("pk541")
		_, err = w.Write(pk.y.EncodedBytes())
		println("pk543")
		return
	case PubKeyAlgoElGamal:
		println("pk546")
		if _, err = w.Write(pk.p.EncodedBytes()); err != nil {
			return
		}
		println("pk550")
		if _, err = w.Write(pk.g.EncodedBytes()); err != nil {
			return
		}
		println("pk554")
		_, err = w.Write(pk.y.EncodedBytes())
		println("pk556")
		return
	case PubKeyAlgoECDSA:
		println("pk559")
		if _, err = w.Write(pk.oid.EncodedBytes()); err != nil {
			return
		}
		println("pk563")
		_, err = w.Write(pk.p.EncodedBytes())
		println("pk565")
		return
	case PubKeyAlgoECDH:
		println("pk568")
		if _, err = w.Write(pk.oid.EncodedBytes()); err != nil {
			return
		}
		println("pk571")
		if _, err = w.Write(pk.p.EncodedBytes()); err != nil {
			return
		}
		println("pk576")
		_, err = w.Write(pk.kdf.EncodedBytes())
		println("pk578")
		return
	case PubKeyAlgoEdDSA:
		println("pk581")
		if _, err = w.Write(pk.oid.EncodedBytes()); err != nil {
			return
		}
		println("pk585")
		_, err = w.Write(pk.p.EncodedBytes())
		println("pk587")
		return
	}
	return errors.InvalidArgumentError("bad public-key algorithm")
}

// CanSign returns true iff this public key can generate signatures
func (pk *PublicKey) CanSign() bool {
	return pk.PubKeyAlgo != PubKeyAlgoRSAEncryptOnly && pk.PubKeyAlgo != PubKeyAlgoElGamal && pk.PubKeyAlgo != PubKeyAlgoECDH
}

// VerifySignature returns nil iff sig is a valid signature, made by this
// public key, of the data hashed into signed. signed is mutated by this call.
func (pk *PublicKey) VerifySignature(signed hash.Hash, sig *Signature) (err error) {
	println("pk647")
	if !pk.CanSign() {
		return errors.InvalidArgumentError("public key cannot generate signatures")
	}
	println("pk651")
	if sig.Version == 5 && (sig.SigType == 0x00 || sig.SigType == 0x01) {
		sig.AddMetadataToHashSuffix()
	}
	println("pk655")
	println("writing to hash 8")
	signed.Write(sig.HashSuffix)
	println("done writing")
	hashBytes := signed.Sum(nil)
	fmt.Fprintf(os.Stderr, "h0 %x\n", hashBytes[0])
	fmt.Fprintf(os.Stderr, "s0 %x\n", sig.HashTag[0])
	fmt.Fprintf(os.Stderr, "h1 %x\n", hashBytes[1])
	fmt.Fprintf(os.Stderr, "s1 %x\n", sig.HashTag[1])
	if hashBytes[0] != sig.HashTag[0] || hashBytes[1] != sig.HashTag[1] {
		return errors.SignatureError("hash tag doesn't match")
	}
	println("pk661")
	if pk.PubKeyAlgo != sig.PubKeyAlgo {
		return errors.InvalidArgumentError("public key and signature use different algorithms")
	}
	println("pk665")
	switch pk.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSASignOnly:
		println("pk668")
		rsaPublicKey, _ := pk.PublicKey.(*rsa.PublicKey)
		err = rsa.VerifyPKCS1v15(rsaPublicKey, sig.Hash, hashBytes, padToKeySize(rsaPublicKey, sig.RSASignature.Bytes()))
		if err != nil {
			return errors.SignatureError("RSA verification failure")
		}
		println("pk674")
		return nil
	case PubKeyAlgoDSA:
		dsaPublicKey, _ := pk.PublicKey.(*dsa.PublicKey)
		// Need to truncate hashBytes to match FIPS 186-3 section 4.6.
		subgroupSize := (dsaPublicKey.Q.BitLen() + 7) / 8
		if len(hashBytes) > subgroupSize {
			hashBytes = hashBytes[:subgroupSize]
		}
		if !dsa.Verify(dsaPublicKey, hashBytes, new(big.Int).SetBytes(sig.DSASigR.Bytes()), new(big.Int).SetBytes(sig.DSASigS.Bytes())) {
			return errors.SignatureError("DSA verification failure")
		}
		return nil
	case PubKeyAlgoECDSA:
		ecdsaPublicKey := pk.PublicKey.(*ecdsa.PublicKey)
		if !ecdsa.Verify(ecdsaPublicKey, hashBytes, new(big.Int).SetBytes(sig.ECDSASigR.Bytes()), new(big.Int).SetBytes(sig.ECDSASigS.Bytes())) {
			return errors.SignatureError("ECDSA verification failure")
		}
		return nil
	case PubKeyAlgoEdDSA:
		eddsaPublicKey := pk.PublicKey.(*ed25519.PublicKey)

		sigR := sig.EdDSASigR.Bytes()
		sigS := sig.EdDSASigS.Bytes()

		eddsaSig := make([]byte, ed25519.SignatureSize)
		copy(eddsaSig[32-len(sigR):32], sigR)
		copy(eddsaSig[64-len(sigS):], sigS)

		if !ed25519.Verify(*eddsaPublicKey, hashBytes, eddsaSig) {
			return errors.SignatureError("EdDSA verification failure")
		}
		return nil
	default:
		return errors.SignatureError("Unsupported public key algorithm used in signature")
	}
}

// keySignatureHash returns a Hash of the message that needs to be signed for
// pk to assert a subkey relationship to signed.
func keySignatureHash(pk, signed signingKey, hashFunc crypto.Hash) (h hash.Hash, err error) {
	println("pk715")
	if !hashFunc.Available() {
		return nil, errors.UnsupportedError("hash function")
	}
	println("pk719")
	println("hash", hashFunc)
	if hashFunc == crypto.SHA1 {
		h = sha1.New()
	} else if hashFunc == crypto.SHA256 {
		h = sha256.New()
	} else if hashFunc == crypto.SHA512 {
		h = sha512.New()
	} else {
		h = hashFunc.New()
	}
	println("pk722")
	// RFC 4880, section 5.2.4
	err = pk.SerializeForHash(h)
	if err != nil {
		return nil, err
	}
	println("p728")
	err = signed.SerializeForHash(h)
	println("pk730")
	return
}

// VerifyKeySignature returns nil iff sig is a valid signature, made by this
// public key, of signed.
func (pk *PublicKey) VerifyKeySignature(signed *PublicKey, sig *Signature) error {
	println("pk737")
	h, err := keySignatureHash(pk, signed, sig.Hash)
	if err != nil {
		return err
	}
	println("pk741")
	if err = pk.VerifySignature(h, sig); err != nil {
		println("pk744")
		println(err.Error())
		return err
	}
	println("pk747")
	if sig.FlagSign {
		// Signing subkeys must be cross-signed. See
		// https://www.gnupg.org/faq/subkey-cross-certify.html.
		println("pk751")
		if sig.EmbeddedSignature == nil {
			return errors.StructuralError("signing subkey is missing cross-signature")
		}
		println("pk755")
		// Verify the cross-signature. This is calculated over the same
		// data as the main signature, so we cannot just recursively
		// call signed.VerifyKeySignature(...)
		println("pk759")
		if h, err = keySignatureHash(pk, signed, sig.EmbeddedSignature.Hash); err != nil {
			return errors.StructuralError("error while hashing for cross-signature: " + err.Error())
		}
		println("pk763")
		if err := signed.VerifySignature(h, sig.EmbeddedSignature); err != nil {
			return errors.StructuralError("error while verifying cross-signature: " + err.Error())
		}
		println("pk767")
	}

	return nil
}

func keyRevocationHash(pk signingKey, hashFunc crypto.Hash) (h hash.Hash, err error) {
	if !hashFunc.Available() {
		return nil, errors.UnsupportedError("hash function")
	}
	h = hashFunc.New()

	// RFC 4880, section 5.2.4
	err = pk.SerializeForHash(h)

	return
}

// VerifyRevocationSignature returns nil iff sig is a valid signature, made by this
// public key.
func (pk *PublicKey) VerifyRevocationSignature(sig *Signature) (err error) {
	h, err := keyRevocationHash(pk, sig.Hash)
	if err != nil {
		return err
	}
	return pk.VerifySignature(h, sig)
}

// VerifySubkeyRevocationSignature returns nil iff sig is a valid subkey revocation signature,
// made by the passed in signingKey.
func (pk *PublicKey) VerifySubkeyRevocationSignature(sig *Signature, signingKey *PublicKey) (err error) {
	h, err := keyRevocationHash(pk, sig.Hash)
	if err != nil {
		return err
	}
	return signingKey.VerifySignature(h, sig)
}

// userIdSignatureHash returns a Hash of the message that needs to be signed
// to assert that pk is a valid key for id.
func userIdSignatureHash(id string, pk *PublicKey, hashFunc crypto.Hash) (h hash.Hash, err error) {
	if !hashFunc.Available() {
		return nil, errors.UnsupportedError("hash function")
	}
	if hashFunc == crypto.SHA1 {
		h = sha1.New()
	} else if hashFunc == crypto.SHA256 {
		h = sha256.New()
	} else if hashFunc == crypto.SHA512 {
		h = sha512.New()
	} else {
		h = hashFunc.New()
	}

	// RFC 4880, section 5.2.4
	pk.SerializeSignaturePrefix(h)
	pk.serializeWithoutHeaders(h)

	var buf [5]byte
	buf[0] = 0xb4
	buf[1] = byte(len(id) >> 24)
	buf[2] = byte(len(id) >> 16)
	buf[3] = byte(len(id) >> 8)
	buf[4] = byte(len(id))

	println("writing to hash 9")
	h.Write(buf[:])
	println("done writing")
	println("writing to hash 4")
	h.Write([]byte(id))
	println("done writing")
	return
}

// VerifyUserIdSignature returns nil iff sig is a valid signature, made by this
// public key, that id is the identity of pub.
func (pk *PublicKey) VerifyUserIdSignature(id string, pub *PublicKey, sig *Signature) (err error) {
	h, err := userIdSignatureHash(id, pub, sig.Hash)
	if err != nil {
		return err
	}
	return pk.VerifySignature(h, sig)
}

// KeyIdString returns the public key's fingerprint in capital hex
// (e.g. "6C7EE1B8621CC013").
func (pk *PublicKey) KeyIdString() string {
	return fmt.Sprintf("%X", pk.Fingerprint[12:20])
}

// KeyIdShortString returns the short form of public key's fingerprint
// in capital hex, as shown by gpg --list-keys (e.g. "621CC013").
func (pk *PublicKey) KeyIdShortString() string {
	return fmt.Sprintf("%X", pk.Fingerprint[16:20])
}

// BitLength returns the bit length for the given public key.
func (pk *PublicKey) BitLength() (bitLength uint16, err error) {
	switch pk.PubKeyAlgo {
	case PubKeyAlgoRSA, PubKeyAlgoRSAEncryptOnly, PubKeyAlgoRSASignOnly:
		bitLength = pk.n.BitLength()
	case PubKeyAlgoDSA:
		bitLength = pk.p.BitLength()
	case PubKeyAlgoElGamal:
		bitLength = pk.p.BitLength()
	case PubKeyAlgoECDSA:
		bitLength = pk.p.BitLength()
	case PubKeyAlgoECDH:
		bitLength = pk.p.BitLength()
	case PubKeyAlgoEdDSA:
		bitLength = pk.p.BitLength()
	default:
		err = errors.InvalidArgumentError("bad public-key algorithm")
	}
	return
}

// KeyExpired returns whether sig is a self-signature of a key that has
// expired or is created in the future.
func (pk *PublicKey) KeyExpired(sig *Signature, currentTime time.Time) bool {
	if pk.CreationTime.After(currentTime) {
		return true
	}
	if sig.KeyLifetimeSecs == nil {
		return false
	}
	expiry := pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second)
	return currentTime.After(expiry)
}
