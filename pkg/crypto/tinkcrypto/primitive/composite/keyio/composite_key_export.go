/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package keyio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/google/tink/go/aead"
	hybrid "github.com/google/tink/go/hybrid/subtle"
	"github.com/google/tink/go/insecurecleartextkeyset"
	"github.com/google/tink/go/keyset"
	commonpb "github.com/google/tink/go/proto/common_go_proto"
	tinkpb "github.com/google/tink/go/proto/tink_go_proto"

	cryptoapi "github.com/hyperledger/aries-framework-go/pkg/crypto"
	ecdhpb "github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto/primitive/proto/ecdh_aead_go_proto"
)

// Package keyio supports exporting of Composite keys (aka Write) and converting the public key part of the a composite
// key (aka PublicKeyToHandle to be used as a valid Tink key)

const (
	ecdhAESPublicKeyTypeURL = "type.hyperledger.org/hyperledger.aries.crypto.tink.EcdhAesAeadPublicKey"
)

// PubKeyWriter will write the raw bytes of a Tink KeySet's primary public key. The raw bytes are a marshaled
// composite.VerificationMethod type.
// The keyset must have a keyURL value equal to `ecdhAESPublicKeyTypeURL` constant of ecdh package.
// Note: This writer should be used only for ECDH public key exports. Other export of public keys should be
//       called via localkms package.
type PubKeyWriter struct {
	w io.Writer
}

// NewWriter creates a new PubKeyWriter instance.
func NewWriter(w io.Writer) *PubKeyWriter {
	return &PubKeyWriter{
		w: w,
	}
}

// Write writes the public keyset to the underlying w.Writer.
func (p *PubKeyWriter) Write(ks *tinkpb.Keyset) error {
	return write(p.w, ks)
}

// WriteEncrypted writes the encrypted keyset to the underlying w.Writer.
func (p *PubKeyWriter) WriteEncrypted(_ *tinkpb.EncryptedKeyset) error {
	return fmt.Errorf("write encrypted function not supported")
}

func write(w io.Writer, msg *tinkpb.Keyset) error {
	ks := msg.Key
	primaryKID := msg.PrimaryKeyId
	created := false

	var err error

	for _, key := range ks {
		if key.KeyId == primaryKID && key.Status == tinkpb.KeyStatusType_ENABLED {
			created, err = writePubKey(w, key)
			if err != nil {
				return err
			}

			break
		}
	}

	if !created {
		return fmt.Errorf("key not written")
	}

	return nil
}

func writePubKey(w io.Writer, key *tinkpb.Keyset_Key) (bool, error) {
	pubKey, err := protoToCompositeKey(key.KeyData)
	if err != nil {
		return false, err
	}

	mPubKey, err := json.Marshal(pubKey)
	if err != nil {
		return false, err
	}

	n, err := w.Write(mPubKey)
	if err != nil {
		return false, err
	}

	return n > 0, nil
}

func protoToCompositeKey(keyData *tinkpb.KeyData) (*cryptoapi.PublicKey, error) {
	var (
		cKey compositeKeyGetter
		err  error
	)

	switch keyData.TypeUrl {
	case ecdhAESPublicKeyTypeURL:
		cKey, err = newECDHKey(keyData.Value)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("can't export key with keyURL:%s", keyData.TypeUrl)
	}

	return buildKey(cKey)
}

func buildKey(c compositeKeyGetter) (*cryptoapi.PublicKey, error) {
	curveName := c.curveName()
	keyTypeName := c.keyType()

	return buildCompositeKey(c.kid(), keyTypeName, curveName, c.x(), c.y())
}

func buildCompositeKey(kid, keyType, curve string, x, y []byte) (*cryptoapi.PublicKey, error) {
	// validate curve
	_, err := hybrid.GetCurve(curve)
	if err != nil {
		return nil, fmt.Errorf("undefined curve: %w", err)
	}

	return &cryptoapi.PublicKey{
		KID:   kid,
		Type:  keyType,
		Curve: curve,
		X:     x,
		Y:     y,
	}, nil
}

type compositeKeyGetter interface {
	kid() string
	curveName() string
	keyType() string
	x() []byte
	y() []byte
}

type ecdhKey struct {
	protoKey *ecdhpb.EcdhAeadPublicKey
}

func newECDHKey(mKey []byte) (compositeKeyGetter, error) {
	pubKeyProto := new(ecdhpb.EcdhAeadPublicKey)

	err := proto.Unmarshal(mKey, pubKeyProto)
	if err != nil {
		return nil, err
	}

	// validate key type
	if pubKeyProto.Params.KwParams.KeyType != ecdhpb.KeyType_EC {
		return nil, fmt.Errorf("undefined key type: '%s'", pubKeyProto.Params.KwParams.KeyType)
	}

	return &ecdhKey{protoKey: pubKeyProto}, nil
}

func (e *ecdhKey) kid() string {
	return e.protoKey.KID
}

func (e *ecdhKey) curveName() string {
	return e.protoKey.Params.KwParams.CurveType.String()
}

func (e *ecdhKey) keyType() string {
	return e.protoKey.Params.KwParams.KeyType.String()
}

func (e *ecdhKey) x() []byte {
	return e.protoKey.X
}

func (e *ecdhKey) y() []byte {
	return e.protoKey.Y
}

// ExtractPrimaryPublicKey is a utility function that will extract the main public key from *keyset.Handle kh.
func ExtractPrimaryPublicKey(kh *keyset.Handle) (*cryptoapi.PublicKey, error) {
	keyBytes, err := writePubKeyFromKeyHandle(kh)
	if err != nil {
		return nil, fmt.Errorf("extractPrimaryPublicKey: failed to get public key content: %w", err)
	}

	ecPubKey := new(cryptoapi.PublicKey)

	err = json.Unmarshal(keyBytes, ecPubKey)
	if err != nil {
		return nil, fmt.Errorf("extractPrimaryPublicKey: unmarshal key failed: %w", err)
	}

	return ecPubKey, nil
}

func writePubKeyFromKeyHandle(handle *keyset.Handle) ([]byte, error) {
	pubKH, err := handle.Public()
	if err != nil {
		if strings.HasSuffix(err.Error(), "keyset contains a non-private key") {
			pubKH = handle
		} else {
			return nil, err
		}
	}

	buf := new(bytes.Buffer)
	pubKeyWriter := NewWriter(buf)

	err = pubKH.WriteWithNoSecrets(pubKeyWriter)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// PublicKeyToKeysetHandle converts pubKey into a *keyset.Handle where pubKey could be either a sender or a recipient
// key. The resulting handle cannot be directly used for primitive execution as the cek is not set. This function serves
// as a helper to get a senderKH to be used as an option for ECDH execution (for ECDH-1PU/authcrypt).
func PublicKeyToKeysetHandle(pubKey *cryptoapi.PublicKey) (*keyset.Handle, error) {
	// validate curve
	cp, err := getCurveProto(pubKey.Curve)
	if err != nil {
		return nil, fmt.Errorf("publicKeyToKeysetHandle: failed to convert curve string to proto: %w", err)
	}

	protoKey := &ecdhpb.EcdhAeadPublicKey{
		Version: 0,
		Params: &ecdhpb.EcdhAeadParams{
			KwParams: &ecdhpb.EcdhKwParams{
				CurveType: cp,
				KeyType:   ecdhpb.KeyType_EC, // for now, TODO create getTypeProto(pubKey.Type) function
			},
			EncParams: &ecdhpb.EcdhAeadEncParams{
				AeadEnc: aead.AES256GCMKeyTemplate(),
			},
			EcPointFormat: commonpb.EcPointFormat_UNCOMPRESSED,
		},
		KID: pubKey.KID,
		X:   pubKey.X,
		Y:   pubKey.Y,
	}

	marshalledKey, err := proto.Marshal(protoKey)
	if err != nil {
		return nil, fmt.Errorf("publicKeyToKeysetHandle: failed to marshal proto: %w", err)
	}

	ks := newKeySet(ecdhAESPublicKeyTypeURL, marshalledKey, tinkpb.KeyData_ASYMMETRIC_PUBLIC)

	memReader := &keyset.MemReaderWriter{Keyset: ks}

	parsedHandle, err := insecurecleartextkeyset.Read(memReader)
	if err != nil {
		return nil, fmt.Errorf("publicKeyToKeysetHandle: failed to create key handle: %w", err)
	}

	return parsedHandle, nil
}

func getCurveProto(c string) (commonpb.EllipticCurveType, error) {
	switch c {
	case "secp256r1", "NIST_P256", "P-256", "EllipticCurveType_NIST_P256":
		return commonpb.EllipticCurveType_NIST_P256, nil
	case "secp384r1", "NIST_P384", "P-384", "EllipticCurveType_NIST_P384":
		return commonpb.EllipticCurveType_NIST_P384, nil
	case "secp521r1", "NIST_P521", "P-521", "EllipticCurveType_NIST_P521":
		return commonpb.EllipticCurveType_NIST_P521, nil
	default:
		return 0, errors.New("unsupported curve")
	}
}

func newKeySet(tURL string, marshalledKey []byte, keyMaterialType tinkpb.KeyData_KeyMaterialType) *tinkpb.Keyset {
	keyData := &tinkpb.KeyData{
		TypeUrl:         tURL,
		Value:           marshalledKey,
		KeyMaterialType: keyMaterialType,
	}

	return &tinkpb.Keyset{
		Key: []*tinkpb.Keyset_Key{
			{
				KeyData: keyData,
				Status:  tinkpb.KeyStatusType_ENABLED,
				KeyId:   1,
				// since we're building the key from raw key bytes, then must use raw key prefix type
				OutputPrefixType: tinkpb.OutputPrefixType_RAW,
			},
		},
		PrimaryKeyId: 1,
	}
}
