// Package protocol implements the on-the-wire primitives the download-gateway
// uses to talk to BitComet WebUI nodes.
//
// This file implements the RNCryptor version 3 binary format in both
// directions. It is used by:
//
//   - the BitComet three-stage handshake: Decrypt(authentication, client_id),
//   - node secret wrapping:   node.pass_enc = Base64(Encrypt(password, masterKey)).
//
// All cryptographic primitives come from the Go standard library only (no cgo,
// no third-party modules).
//
// # Wire format (RNCryptor v3)
//
//	password-based (options bit 0 set), header = 34 bytes:
//	+---------+---------+---------+----------+----+----------------+-----+
//	| version | options | encSalt | hmacSalt | iv |   ciphertext   | mac |
//	| 1 byte  | 1 byte  | 8 bytes | 8 bytes  | 16 | n bytes        | 32  |
//	+---------+---------+---------+----------+----+----------------+-----+
//
//	key-based (options bit 0 clear), header = 18 bytes (no salt fields):
//	+---------+---------+----+----------------+-----+
//	| version | options | iv |   ciphertext   | mac |
//	| 1 byte  | 1 byte  | 16 | n bytes        | 32  |
//	+---------+---------+----+----------------+-----+
//
//	version     = 3
//	options     = 0x01 for password-based, 0x00 for key-based
//	ciphertext  = AES-256-CBC over PKCS#7(plaintext); n is a multiple of 16
//	mac         = HMAC-SHA256(macKey, header || ciphertext)  (full 32 bytes,
//	              not truncated)
//
// Key derivation (password-based):
//
//	encKey  = PBKDF2-HMAC-SHA1(password, encSalt,  10000, 32)
//	macKey  = PBKDF2-HMAC-SHA1(password, hmacSalt, 10000, 32)
//
// Key-based (`options == 0`) uses the caller-supplied encKey/macKey directly;
// its header omits the salt fields (this is mandated by the official key
// vectors, whose blobs are 18 + n + 32 bytes long).
//
// The MAC is verified in constant time *before* any decryption or padding
// validation, so a single flipped bit anywhere in header/ciphertext is always
// reported as ErrIntegrityFailed and never leaks a padding oracle.
package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// RNCryptor v3 format constants.
const (
	rncryptorVersion = 3
	optionPassword   = 0x01

	saltSize = 8  // per-salt size (encryption salt and HMAC salt)
	ivSize   = 16 // AES-CBC IV
	keySize  = 32 // AES-256 key / HMAC-SHA256 key

	// passwordHeaderSize = version(1)+options(1)+encSalt(8)+hmacSalt(8)+iv(16).
	passwordHeaderSize = 1 + 1 + saltSize + saltSize + ivSize // 34

	// keyHeaderSize = version(1)+options(1)+iv(16); key-based blobs carry no
	// salt fields (see the official key vectors).
	keyHeaderSize = 1 + 1 + ivSize // 18

	rncryptorMACSize = sha256.Size // 32-byte HMAC-SHA256 tag

	pbkdf2Iterations = 10000
	aesBlockSize     = aes.BlockSize // 16
)

// Sentinel errors returned by this package. Callers should test them with
// errors.Is.
var (
	// ErrEmptyPassword is returned when a password-based operation is invoked
	// with an empty password (an empty client_id / master key is always invalid).
	ErrEmptyPassword = errors.New("protocol: empty password")

	// ErrBadKeySize is returned when a key-based operation receives a key whose
	// length is not exactly 32 bytes (AES-256 / HMAC-SHA256).
	ErrBadKeySize = errors.New("protocol: key must be 32 bytes")

	// ErrMalformed is returned for structurally invalid input: too short,
	// non-block-aligned ciphertext, or input that is not valid base64.
	ErrMalformed = errors.New("protocol: malformed RNCryptor data")

	// ErrUnsupportedVersion is returned when the version byte is not 3.
	ErrUnsupportedVersion = errors.New("protocol: unsupported RNCryptor version")

	// ErrUnsupportedOptions is returned when the options byte does not match the
	// operation (password-based Decrypt requires options bit 0 set).
	ErrUnsupportedOptions = errors.New("protocol: unsupported RNCryptor options")

	// ErrIntegrityFailed is returned when the HMAC check fails. A wrong password
	// and any tampering both surface as this error.
	ErrIntegrityFailed = errors.New("protocol: integrity check failed")

	// ErrInvalidPadding is returned when the decrypted PKCS#7 padding is invalid.
	ErrInvalidPadding = errors.New("protocol: invalid PKCS#7 padding")
)

// Encrypt encrypts plaintext with password using RNCryptor v3 and returns the
// raw binary blob (header || ciphertext || mac). A fresh random encryption salt,
// HMAC salt and IV are generated for every call, so two encryptions of the same
// plaintext with the same password always differ.
//
// An empty password is rejected with ErrEmptyPassword.
func Encrypt(plaintext []byte, password string) ([]byte, error) {
	if password == "" {
		return nil, ErrEmptyPassword
	}
	encSalt, err := randomBytes(saltSize)
	if err != nil {
		return nil, err
	}
	hmacSalt, err := randomBytes(saltSize)
	if err != nil {
		return nil, err
	}
	iv, err := randomBytes(ivSize)
	if err != nil {
		return nil, err
	}
	encKey, err := deriveKey(password, encSalt, keySize)
	if err != nil {
		return nil, err
	}
	macKey, err := deriveKey(password, hmacSalt, keySize)
	if err != nil {
		return nil, err
	}
	return encryptCore(plaintext, optionPassword, encSalt, hmacSalt, iv, encKey, macKey)
}

// EncryptToBase64 is Encrypt followed by standard base64 encoding (padded). This
// is the form used for stored secrets such as node.pass_enc.
func EncryptToBase64(plaintext []byte, password string) (string, error) {
	blob, err := Encrypt(plaintext, password)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// Decrypt decrypts an RNCryptor v3 password-based blob produced by Encrypt (or
// by any spec-conformant implementation, including the Rust reference) and
// returns the recovered plaintext.
//
// Errors:
//   - ErrEmptyPassword      password is empty;
//   - ErrMalformed          blob shorter than one block, or not block-aligned;
//   - ErrUnsupportedVersion version byte != 3;
//   - ErrUnsupportedOptions the blob is key-based (options bit 0 clear);
//   - ErrIntegrityFailed     HMAC mismatch (wrong password or tampering);
//   - ErrInvalidPadding      the recovered PKCS#7 padding is invalid.
func Decrypt(blob []byte, password string) ([]byte, error) {
	if password == "" {
		return nil, ErrEmptyPassword
	}
	if len(blob) < passwordHeaderSize {
		return nil, ErrMalformed
	}
	if blob[0] != rncryptorVersion {
		return nil, ErrUnsupportedVersion
	}
	if blob[1]&optionPassword == 0 {
		return nil, ErrUnsupportedOptions
	}
	encSalt := blob[2 : 2+saltSize]
	hmacSalt := blob[2+saltSize : 2+2*saltSize]
	encKey, err := deriveKey(password, encSalt, keySize)
	if err != nil {
		return nil, err
	}
	macKey, err := deriveKey(password, hmacSalt, keySize)
	if err != nil {
		return nil, err
	}
	return decryptCore(blob, passwordHeaderSize, encKey, macKey)
}

// DecryptFromBase64 decodes a standard (padded) or raw base64 string and then
// calls Decrypt. Invalid base64 is reported as ErrMalformed.
func DecryptFromBase64(encoded, password string) ([]byte, error) {
	blob, err := decodeBase64(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return Decrypt(blob, password)
}

// EncryptWithKey is the key-based variant (options == 0). Both encryptionKey and
// macKey must be exactly 32 bytes. Key-based blobs carry NO salt header fields:
// the header is 18 bytes (version + options + IV), matching the official key
// vectors. The IV is generated randomly per call.
func EncryptWithKey(plaintext, encryptionKey, macKey []byte) ([]byte, error) {
	if len(encryptionKey) != keySize || len(macKey) != keySize {
		return nil, ErrBadKeySize
	}
	iv, err := randomBytes(ivSize)
	if err != nil {
		return nil, err
	}
	// Key-based blobs carry no salt fields.
	return encryptCore(plaintext, 0x00, nil, nil, iv, encryptionKey, macKey)
}

// DecryptWithKey is the key-based variant (options == 0) of Decrypt. The
// encKey/macKey must be exactly 32 bytes; the options byte is not inspected
// because the keys are supplied explicitly.
func DecryptWithKey(blob, encryptionKey, macKey []byte) ([]byte, error) {
	if len(encryptionKey) != keySize || len(macKey) != keySize {
		return nil, ErrBadKeySize
	}
	if len(blob) < keyHeaderSize {
		return nil, ErrMalformed
	}
	if blob[0] != rncryptorVersion {
		return nil, ErrUnsupportedVersion
	}
	return decryptCore(blob, headerSizeFor(blob[1]), encryptionKey, macKey)
}

// encryptCore assembles an RNCryptor v3 blob from explicit key material. It is
// shared by the password-based and key-based paths.
func encryptCore(plaintext []byte, options byte, encSalt, hmacSalt, iv, encKey, macKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKeySize, err)
	}
	padded := pkcs7Pad(plaintext, aesBlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	hSize := headerSizeFor(options)
	header := make([]byte, 0, hSize)
	header = append(header, rncryptorVersion, options)
	if options&optionPassword != 0 {
		header = append(header, encSalt...)
		header = append(header, hmacSalt...)
	}
	header = append(header, iv...)
	if len(header) != hSize {
		return nil, ErrMalformed
	}

	mac := hmac.New(sha256.New, macKey)
	mac.Write(header)
	mac.Write(ciphertext)

	out := make([]byte, 0, len(header)+len(ciphertext)+rncryptorMACSize)
	out = append(out, header...)
	out = append(out, ciphertext...)
	out = append(out, mac.Sum(nil)...)
	return out, nil
}

// headerSizeFor returns the header length implied by an options byte.
func headerSizeFor(options byte) int {
	if options&optionPassword != 0 {
		return passwordHeaderSize
	}
	return keyHeaderSize
}

// decryptCore verifies the MAC and decrypts any RNCryptor v3 blob whose key
// material has already been resolved by the caller. hSize is the header length
// for the blob (34 for password-based, 18 for key-based).
func decryptCore(blob []byte, hSize int, encKey, macKey []byte) ([]byte, error) {
	header, ciphertext, mac, err := splitBlob(blob, hSize)
	if err != nil {
		return nil, err
	}

	// Verify integrity first (constant time) so that a tampered blob never
	// reaches the cipher or the padding check.
	expected := hmac.New(sha256.New, macKey)
	expected.Write(header)
	expected.Write(ciphertext)
	if !hmac.Equal(expected.Sum(nil), mac) {
		return nil, ErrIntegrityFailed
	}

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKeySize, err)
	}
	iv := header[hSize-ivSize:]
	padded := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(padded, ciphertext)

	return pkcs7Unpad(padded, aesBlockSize)
}

// splitBlob validates the overall length and splits a blob into its three
// regions: header, ciphertext (block-aligned, non-empty) and trailing MAC.
func splitBlob(blob []byte, hSize int) (header, ciphertext, mac []byte, err error) {
	if len(blob) < hSize+rncryptorMACSize+aesBlockSize {
		return nil, nil, nil, ErrMalformed
	}
	bodyEnd := len(blob) - rncryptorMACSize
	header = blob[:hSize]
	ciphertext = blob[hSize:bodyEnd]
	mac = blob[bodyEnd:]
	if len(ciphertext)%aesBlockSize != 0 {
		return nil, nil, nil, ErrMalformed
	}
	return header, ciphertext, mac, nil
}

// deriveKey runs PBKDF2-HMAC-SHA1 as required by RNCryptor v3.
func deriveKey(password string, salt []byte, keyLen int) ([]byte, error) {
	if keyLen <= 0 {
		return nil, ErrBadKeySize
	}
	return pbkdf2.Key(sha1.New, password, salt, pbkdf2Iterations, keyLen)
}

// pkcs7Pad appends PKCS#7 padding. When the input is already a multiple of
// blockSize an entire extra block (blockSize bytes of value blockSize) is
// appended, as the spec mandates.
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// pkcs7Unpad validates and strips PKCS#7 padding. It returns ErrInvalidPadding
// for every malformed case:
//
//   - empty input;
//   - a final byte of 0 (pad = 0);
//   - a pad length greater than the block size;
//   - a pad length greater than the available data (out of range);
//   - data whose length is not a multiple of blockSize;
//   - padding bytes that are not all equal to the pad length (self-contradictory).
//
// A single-byte padding (pad = 1) is valid and is accepted.
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if blockSize <= 0 || blockSize > 255 {
		return nil, ErrInvalidPadding
	}
	if len(data) == 0 {
		return nil, ErrInvalidPadding
	}
	pad := int(data[len(data)-1])
	if pad == 0 {
		return nil, ErrInvalidPadding
	}
	if pad > blockSize {
		// Reachable in isolation: a 32-byte input ending in 0x11 (pad = 17 > 16),
		// where len(data) >= pad so the length guard below does not also fire.
		return nil, ErrInvalidPadding
	}
	if pad > len(data) {
		// NOTE: under the current check order this guard is logically redundant.
		// The guard above guarantees pad <= blockSize, and the alignment guard
		// below guarantees len(data) % blockSize == 0 with len(data) > 0, hence
		// len(data) >= blockSize >= pad and "pad > len(data)" can never be true.
		// It is retained deliberately: it keeps the "reject illegal padding"
		// property independent of the order of these checks, so that a future
		// reordering cannot silently open a hole. Do not delete it.
		return nil, ErrInvalidPadding
	}
	if len(data)%blockSize != 0 {
		return nil, ErrInvalidPadding
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidPadding
		}
	}
	return data[:len(data)-pad], nil
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("protocol: entropy source failed: %w", err)
	}
	return b, nil
}

// decodeBase64 accepts padded standard base64 and, as a fallback, raw (unpadded)
// standard base64.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
