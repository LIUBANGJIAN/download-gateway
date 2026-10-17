package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Vector file helpers.
//
// The official RNCryptor v3 vector files are plain text of the form
// "# comment" and "key: value", with blank lines between records. Hex values
// carry whitespace grouping that must be stripped before decoding.
// ---------------------------------------------------------------------------

// loadVectors parses one of the testdata vector files into a slice of records.
func loadVectors(t *testing.T, name string) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read vector file %s: %v", name, err)
	}
	var (
		records []map[string]string
		cur     map[string]string
	)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if cur != nil {
				records = append(records, cur)
				cur = nil
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		if cur == nil {
			cur = make(map[string]string)
		}
		cur[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	if cur != nil {
		records = append(records, cur)
	}
	return records
}

// hexField decodes a hex field of a record, stripping all whitespace. An empty
// value decodes to an empty (non-nil) slice, which is legitimate for the
// zero-length plaintext vector.
func hexField(t *testing.T, rec map[string]string, key string) []byte {
	t.Helper()
	raw := rec[key]
	cleaned := strings.Join(strings.Fields(raw), "")
	if cleaned == "" {
		return []byte{}
	}
	b, err := hex.DecodeString(cleaned)
	if err != nil {
		t.Fatalf("field %q=%q is not valid hex: %v", key, raw, err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Judge (a): official RNCryptor v3 vectors (network-independent ground truth).
// ---------------------------------------------------------------------------

// TestOfficialPasswordVectors decrypts every official password-based vector and
// also re-checks the V3 header shape (version, options, salts, IV, alignment).
func TestOfficialPasswordVectors(t *testing.T) {
	recs := loadVectors(t, "v3_password.txt")
	if len(recs) != 6 {
		t.Fatalf("expected 6 password vectors, got %d", len(recs))
	}
	for _, rec := range recs {
		rec := rec
		t.Run(rec["title"], func(t *testing.T) {
			password := rec["password"]
			encSalt := hexField(t, rec, "enc_salt_hex")
			hmacSalt := hexField(t, rec, "hmac_salt_hex")
			iv := hexField(t, rec, "iv_hex")
			plaintext := hexField(t, rec, "plaintext_hex")
			blob := hexField(t, rec, "ciphertext_hex")

			if len(blob) < passwordHeaderSize+rncryptorMACSize+aesBlockSize {
				t.Fatalf("vector blob too short: %d bytes", len(blob))
			}
			if blob[0] != rncryptorVersion {
				t.Fatalf("version byte = %d, want %d", blob[0], rncryptorVersion)
			}
			if blob[1] != optionPassword {
				t.Fatalf("options byte = %#x, want %#x", blob[1], optionPassword)
			}
			if !bytes.Equal(blob[2:10], encSalt) {
				t.Fatalf("enc salt mismatch: got %x want %x", blob[2:10], encSalt)
			}
			if !bytes.Equal(blob[10:18], hmacSalt) {
				t.Fatalf("hmac salt mismatch: got %x want %x", blob[10:18], hmacSalt)
			}
			if !bytes.Equal(blob[18:34], iv) {
				t.Fatalf("iv mismatch: got %x want %x", blob[18:34], iv)
			}
			ctLen := len(blob) - passwordHeaderSize - rncryptorMACSize
			if ctLen%aesBlockSize != 0 {
				t.Fatalf("ciphertext length %d is not a multiple of %d", ctLen, aesBlockSize)
			}

			got, err := Decrypt(blob, password)
			if err != nil {
				t.Fatalf("Decrypt failed: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("plaintext mismatch:\n got %x\nwant %x", got, plaintext)
			}
		})
	}
}

// TestOfficialKDFVectors pins PBKDF2-HMAC-SHA1(password, salt, 10000, 32).
func TestOfficialKDFVectors(t *testing.T) {
	recs := loadVectors(t, "v3_kdf.txt")
	if len(recs) != 6 {
		t.Fatalf("expected 6 kdf vectors, got %d", len(recs))
	}
	for _, rec := range recs {
		rec := rec
		t.Run(rec["title"], func(t *testing.T) {
			password := rec["password"]
			salt := hexField(t, rec, "salt_hex")
			want := hexField(t, rec, "key_hex")

			got, err := deriveKey(password, salt, keySize)
			if err != nil {
				t.Fatalf("deriveKey failed: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("derived key mismatch:\n got %x\nwant %x", got, want)
			}
		})
	}
}

// TestOfficialKeyVectors exercises the key-based path (options == 0). This
// isolates the CBC + HMAC layer from PBKDF2 and is an extra, spec-native judge.
func TestOfficialKeyVectors(t *testing.T) {
	recs := loadVectors(t, "v3_key.txt")
	if len(recs) != 4 {
		t.Fatalf("expected 4 key vectors, got %d", len(recs))
	}
	for _, rec := range recs {
		rec := rec
		t.Run(rec["title"], func(t *testing.T) {
			encKey := hexField(t, rec, "enc_key_hex")
			macKey := hexField(t, rec, "hmac_key_hex")
			iv := hexField(t, rec, "iv_hex")
			plaintext := hexField(t, rec, "plaintext_hex")
			blob := hexField(t, rec, "ciphertext_hex")

			if blob[1] != 0x00 {
				t.Fatalf("options byte = %#x, want 0x00 for key-based", blob[1])
			}
			// Key-based header is 18 bytes: version(1)+options(1)+iv(16).
			if keyHeaderSize != 18 {
				t.Fatalf("keyHeaderSize = %d, want 18", keyHeaderSize)
			}
			if len(blob) < keyHeaderSize+rncryptorMACSize+aesBlockSize {
				t.Fatalf("key vector blob too short: %d bytes", len(blob))
			}
			if !bytes.Equal(blob[2:18], iv) {
				t.Fatalf("iv mismatch: got %x want %x", blob[2:18], iv)
			}
			ctLen := len(blob) - keyHeaderSize - rncryptorMACSize
			if ctLen%aesBlockSize != 0 {
				t.Fatalf("ciphertext length %d is not a multiple of %d", ctLen, aesBlockSize)
			}

			got, err := DecryptWithKey(blob, encKey, macKey)
			if err != nil {
				t.Fatalf("DecryptWithKey failed: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("plaintext mismatch:\n got %x\nwant %x", got, plaintext)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Judge (b): behavioural assertions that mirror the Rust reference test module.
// Each subtest names the Rust case it corresponds to.
// ---------------------------------------------------------------------------

// All fields empty or zero => empty plaintext must round-trip (Rust: v3 header
// shape / empty plaintext is legal and not a parse failure).
func TestEncryptDecryptEmptyPlaintext(t *testing.T) {
	blob, err := Encrypt([]byte{}, "client-id")
	if err != nil {
		t.Fatalf("Encrypt(empty) failed: %v", err)
	}
	got, err := Decrypt(blob, "client-id")
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %x", got)
	}
}

// V3 header shape (Rust: v3_header_shape). raw[0]==3, raw[1]==1, fixed 34-byte
// header, trailing 32-byte HMAC, ciphertext a multiple of 16.
func TestV3HeaderShape(t *testing.T) {
	blob, err := Encrypt([]byte("hello"), "pw")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if blob[0] != 3 {
		t.Fatalf("raw[0] = %d, want 3", blob[0])
	}
	if blob[1] != 1 {
		t.Fatalf("raw[1] = %d, want 1", blob[1])
	}
	if passwordHeaderSize != 34 {
		t.Fatalf("passwordHeaderSize = %d, want 34", passwordHeaderSize)
	}
	if len(blob) < passwordHeaderSize+rncryptorMACSize {
		t.Fatalf("blob too short: %d", len(blob))
	}
	if got := len(blob) - passwordHeaderSize - rncryptorMACSize; got%aesBlockSize != 0 {
		t.Fatalf("ciphertext length %d not a multiple of %d", got, aesBlockSize)
	}
	if rncryptorMACSize != 32 {
		t.Fatalf("mac size = %d, want 32", rncryptorMACSize)
	}
}

// 5-byte plaintext => exactly one 16-byte ciphertext block (5 + 11 pad bytes)
// (Rust: five_byte_plaintext_pads_to_single_block).
func TestFiveBytePlaintextIsOneBlock(t *testing.T) {
	blob, err := Encrypt([]byte("hello"), "pw")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	ctLen := len(blob) - passwordHeaderSize - rncryptorMACSize
	if ctLen != 16 {
		t.Fatalf("ciphertext length = %d, want 16", ctLen)
	}
}

// PKCS#7 boundary: a full block of plaintext must gain an extra block
// (Rust: pkcs7_pad_full_block_appends_extra_block).
func TestPKCS7PadFullBlock(t *testing.T) {
	in := bytes.Repeat([]byte{0xAB}, aesBlockSize)
	out := pkcs7Pad(in, aesBlockSize)
	if len(out) != 2*aesBlockSize {
		t.Fatalf("padded length = %d, want %d", len(out), 2*aesBlockSize)
	}
	tail := out[aesBlockSize:]
	want := bytes.Repeat([]byte{byte(aesBlockSize)}, aesBlockSize)
	if !bytes.Equal(tail, want) {
		t.Fatalf("pad block = %x, want %x", tail, want)
	}
}

// Legal single-byte padding must be accepted (Rust comment: rejecting it would
// break ~1/16 of otherwise valid ciphertexts).
func TestPKCS7UnpadPadOneAccepted(t *testing.T) {
	data := append([]byte("rncryptor-v3dat"), 0x01) // 15 data bytes + pad=1
	if len(data) != aesBlockSize {
		t.Fatalf("test setup: len = %d, want %d", len(data), aesBlockSize)
	}
	got, err := pkcs7Unpad(data, aesBlockSize)
	if err != nil {
		t.Fatalf("pad=1 was wrongly rejected: %v", err)
	}
	if string(got) != "rncryptor-v3dat" {
		t.Fatalf("unpadded = %q, want %q", got, "rncryptor-v3dat")
	}
}

// All six illegal-padding families must be rejected with ErrInvalidPadding
// (Rust: pkcs7_unpad_rejects_invalid_padding).
func TestPKCS7UnpadRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		// last byte = 4 but the byte four from the end is 9 (self-contradictory).
		{"self_contradictory", append(bytes.Repeat([]byte{0x00}, 12), 0x09, 0x03, 0x02, 0x04)},
		// pad = 0.
		{"pad_zero", bytes.Repeat([]byte{0x00}, aesBlockSize)},
		// pad = 17 > block size 16, with len(data) = 32 so the LENGTH guard
		// (pad > len(data)) does NOT also reject it. This case therefore
		// ISOLATES the "pad > blockSize" branch: if that guard is removed this
		// input would be wrongly accepted (returning 15 bytes). Distinct from
		// "pad_grossly_out_of_range" below, which other guards also reject.
		{"pad_exceeds_block_size", bytes.Repeat([]byte{0x11}, 32)},
		// pad = 0x20 (32): exceeds block size AND length, so it is rejected even
		// without the block-size guard. Kept as a representative absurd pad but
		// it does NOT isolate any single branch.
		{"pad_grossly_out_of_range", append(bytes.Repeat([]byte{0x00}, 15), 0x20)},
		// empty input.
		{"empty_input", []byte{}},
		// pad = 1 but length 3 is not a multiple of the block size.
		{"length_not_block_multiple", []byte{0x01, 0x02, 0x01}},
		// pad = 16 > available length 2. NOTE: the standalone "pad > len(data)"
		// guard in pkcs7Unpad is logically REDUNDANT under the current check
		// order (pad <= blockSize plus len%blockSize == 0 with len > 0 implies
		// len >= pad). This input is rejected here by that redundant guard; were
		// it removed, the length-alignment guard (2 % 16 != 0) would reject it
		// instead. The guard is kept on purpose -- see its comment in
		// rncryptor.go -- to keep the rejection robust to reordering.
		{"padding_out_of_range", []byte{0x10, 0x10}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pkcs7Unpad(tc.in, aesBlockSize); !errors.Is(err, ErrInvalidPadding) {
				t.Fatalf("err = %v, want ErrInvalidPadding", err)
			}
		})
	}
}

// An empty password must be rejected in both directions
// (Rust: empty_password_is_rejected).
func TestEmptyPasswordRejected(t *testing.T) {
	if _, err := Encrypt([]byte("x"), ""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("Encrypt err = %v, want ErrEmptyPassword", err)
	}
	blob, err := Encrypt([]byte("x"), "pw")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if _, err := Decrypt(blob, ""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("Decrypt err = %v, want ErrEmptyPassword", err)
	}
}

// Two encryptions of the same plaintext must differ (random salt/IV in effect)
// yet both decrypt back to the plaintext
// (Rust: encrypt_is_randomized).
func TestEncryptIsRandomized(t *testing.T) {
	a, err := Encrypt([]byte("same plaintext"), "pw")
	if err != nil {
		t.Fatalf("Encrypt a failed: %v", err)
	}
	b, err := Encrypt([]byte("same plaintext"), "pw")
	if err != nil {
		t.Fatalf("Encrypt b failed: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions produced identical output; salt/IV not random")
	}
	for i, blob := range [][]byte{a, b} {
		got, err := Decrypt(blob, "pw")
		if err != nil {
			t.Fatalf("Decrypt #%d failed: %v", i, err)
		}
		if string(got) != "same plaintext" {
			t.Fatalf("Decrypt #%d = %q", i, got)
		}
	}
}

// Flipping one bit of the ciphertext must be caught by the HMAC
// (Rust: tampered_ciphertext_fails_integrity).
func TestTamperOneBitFailsIntegrity(t *testing.T) {
	blob, err := Encrypt([]byte("payload"), "pw")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	ctStart, ctEnd := passwordHeaderSize, len(blob)-rncryptorMACSize
	if ctEnd <= ctStart {
		t.Fatal("no ciphertext region")
	}
	tampered := append([]byte(nil), blob...)
	tampered[ctStart] ^= 0x01
	if _, err := Decrypt(tampered, "pw"); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("err = %v, want ErrIntegrityFailed", err)
	}
}

// Wrong password must fail the integrity check
// (Rust: wrong_password_fails_integrity).
func TestWrongPasswordFailsIntegrity(t *testing.T) {
	blob, err := Encrypt([]byte("secret"), "right-password")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if _, err := Decrypt(blob, "wrong-password"); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("err = %v, want ErrIntegrityFailed", err)
	}
}

// Malformed inputs must fail gracefully, never panic
// (Rust: malformed_input_errors_without_panic).
func TestMalformedInputs(t *testing.T) {
	if _, err := DecryptFromBase64("!!! not base64 !!!", "pw"); err == nil {
		t.Fatal("expected error for non-base64 input")
	}
	if _, err := Decrypt(nil, "pw"); err == nil {
		t.Fatal("expected error for nil input")
	}
	if _, err := Decrypt([]byte{0x03}, "pw"); err == nil {
		t.Fatal("expected error for too-short input")
	}
	// correct length but wrong version byte.
	blob, err := Encrypt([]byte("x"), "pw")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	blob[0] = 2
	if _, err := Decrypt(blob, "pw"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
}

// TestCiphertextBlockAlignmentRejected covers the splitBlob alignment guard
// (`len(ciphertext) % aesBlockSize != 0`). Without that guard the ciphertext
// would reach cipher.NewCBCDecrypter(...).CryptBlocks, which PANICS on a
// non-block-aligned length. The input to Decrypt/DecryptFromBase64 is
// attacker-controlled (the BitComet handshake `authentication` field), so the
// missing guard is a denial-of-service surface. The guard must reject with
// ErrMalformed *before* the MAC check, so we also assert the error is not
// ErrIntegrityFailed.
func TestCiphertextBlockAlignmentRejected(t *testing.T) {
	const password = "alignment-pw"
	legit, err := Encrypt([]byte("alignment payload"), password)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	// Insert one byte immediately after the 34-byte header so that the
	// ciphertext region (previously 16k bytes) becomes 16k+1 bytes.
	bad := append(append([]byte{}, legit[:35]...), legit[34:]...)
	if len(bad) != len(legit)+1 {
		t.Fatalf("test setup: len(bad) = %d, want %d", len(bad), len(legit)+1)
	}
	ctLen := len(bad) - passwordHeaderSize - rncryptorMACSize
	if ctLen%aesBlockSize == 0 {
		t.Fatalf("test setup: ciphertext length %d is still block-aligned", ctLen)
	}

	t.Run("Decrypt", func(t *testing.T) {
		_, err := Decrypt(bad, password)
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("err = %v, want ErrMalformed", err)
		}
		if errors.Is(err, ErrIntegrityFailed) {
			t.Fatal("got ErrIntegrityFailed: the alignment guard must fire before the MAC check")
		}
	})
	t.Run("DecryptFromBase64", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString(bad)
		_, err := DecryptFromBase64(encoded, password)
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("err = %v, want ErrMalformed", err)
		}
		if errors.Is(err, ErrIntegrityFailed) {
			t.Fatal("got ErrIntegrityFailed: the alignment guard must fire before the MAC check")
		}
	})
}

// Round-trip across the PKCS#7 boundary lengths.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	password := "client-id-42"
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 255, 1024} {
		pt := bytes.Repeat([]byte{byte(n)}, n)
		blob, err := Encrypt(pt, password)
		if err != nil {
			t.Fatalf("n=%d: Encrypt failed: %v", n, err)
		}
		got, err := Decrypt(blob, password)
		if err != nil {
			t.Fatalf("n=%d: Decrypt failed: %v", n, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("n=%d: round-trip mismatch", n)
		}
	}
}

// Base64 helpers round-trip.
func TestBase64RoundTrip(t *testing.T) {
	enc, err := EncryptToBase64([]byte("hello world"), "pw")
	if err != nil {
		t.Fatalf("EncryptToBase64 failed: %v", err)
	}
	got, err := DecryptFromBase64(enc, "pw")
	if err != nil {
		t.Fatalf("DecryptFromBase64 failed: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
}

// Key-based round-trip isolates CBC+HMAC from PBKDF2.
func TestKeyBasedRoundTrip(t *testing.T) {
	encKey := bytes.Repeat([]byte{0x11}, keySize)
	macKey := bytes.Repeat([]byte{0x22}, keySize)
	blob, err := EncryptWithKey([]byte("key-based payload"), encKey, macKey)
	if err != nil {
		t.Fatalf("EncryptWithKey failed: %v", err)
	}
	if blob[1] != 0x00 {
		t.Fatalf("options byte = %#x, want 0x00", blob[1])
	}
	got, err := DecryptWithKey(blob, encKey, macKey)
	if err != nil {
		t.Fatalf("DecryptWithKey failed: %v", err)
	}
	if string(got) != "key-based payload" {
		t.Fatalf("got %q", got)
	}
	// wrong mac key must fail integrity.
	if _, err := DecryptWithKey(blob, encKey, bytes.Repeat([]byte{0x33}, keySize)); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("err = %v, want ErrIntegrityFailed", err)
	}
	// wrong key size must be rejected.
	if _, err := DecryptWithKey(blob, encKey[:16], macKey); !errors.Is(err, ErrBadKeySize) {
		t.Fatalf("err = %v, want ErrBadKeySize", err)
	}
}

// ---------------------------------------------------------------------------
// Falsification test: prove the tamper assertions are not vacuous.
//
// This test flips EVERY single bit of the ciphertext region and requires that
// every one of those blobs is rejected. It additionally asserts the number of
// positions examined, so that an accidentally empty loop cannot make it pass.
// ---------------------------------------------------------------------------
func TestFalsification_TamperEveryCiphertextBitFails(t *testing.T) {
	const password = "falsification-pw"
	blob, err := Encrypt([]byte("falsification payload"), password)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	ctStart, ctEnd := passwordHeaderSize, len(blob)-rncryptorMACSize
	if ctEnd <= ctStart {
		t.Fatalf("empty ciphertext region: [%d,%d)", ctStart, ctEnd)
	}

	examined := 0
	rejected := 0
	for i := ctStart; i < ctEnd; i++ {
		for bit := uint(0); bit < 8; bit++ {
			examined++
			tampered := append([]byte(nil), blob...)
			tampered[i] ^= 1 << bit
			_, err := Decrypt(tampered, password)
			if err != nil {
				rejected++
				continue
			}
			t.Fatalf("bit flip at byte %d bit %d was NOT detected", i, bit)
		}
	}

	wantExamined := (ctEnd - ctStart) * 8
	if examined != wantExamined {
		t.Fatalf("examined %d bit positions, want %d (loop may be vacuous)", examined, wantExamined)
	}
	if examined == 0 {
		t.Fatal("examined zero bit positions: test is vacuous")
	}
	if rejected != examined {
		t.Fatalf("rejected %d of %d tampered blobs", rejected, examined)
	}
	t.Logf("verified: all %d single-bit ciphertext tamper positions are rejected", examined)
}

// Extra falsification coverage: tamper the HMAC tag and the IV too.
func TestFalsification_TamperMACAndIVFails(t *testing.T) {
	const password = "falsification-pw"
	blob, err := Encrypt([]byte("payload"), password)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// flip a bit in the MAC region.
	macByte := len(blob) - 1
	tamperedMAC := append([]byte(nil), blob...)
	tamperedMAC[macByte] ^= 0x80
	if _, err := Decrypt(tamperedMAC, password); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("tampered MAC err = %v, want ErrIntegrityFailed", err)
	}

	// flip a bit in the IV region.
	tamperedIV := append([]byte(nil), blob...)
	tamperedIV[18] ^= 0x01
	if _, err := Decrypt(tamperedIV, password); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("tampered IV err = %v, want ErrIntegrityFailed", err)
	}
}

// ---------------------------------------------------------------------------
// Judge (a2): byte-exact re-encryption against the official vectors.
//
// For every password vector the salts and IV are fixed by the vector, so a
// conformant Encrypt must reproduce ciphertext_hex byte-for-byte. This tests
// the *encryption* direction directly against the reference implementation,
// independently of any round-trip.
// ---------------------------------------------------------------------------
func TestOfficialPasswordVectorsReEncryptByteExact(t *testing.T) {
	recs := loadVectors(t, "v3_password.txt")
	if len(recs) != 6 {
		t.Fatalf("expected 6 password vectors, got %d", len(recs))
	}
	for _, rec := range recs {
		rec := rec
		t.Run(rec["title"], func(t *testing.T) {
			password := rec["password"]
			encSalt := hexField(t, rec, "enc_salt_hex")
			hmacSalt := hexField(t, rec, "hmac_salt_hex")
			iv := hexField(t, rec, "iv_hex")
			plaintext := hexField(t, rec, "plaintext_hex")
			want := hexField(t, rec, "ciphertext_hex")

			encKey, err := deriveKey(password, encSalt, keySize)
			if err != nil {
				t.Fatalf("deriveKey enc: %v", err)
			}
			macKey, err := deriveKey(password, hmacSalt, keySize)
			if err != nil {
				t.Fatalf("deriveKey mac: %v", err)
			}
			got, err := encryptCore(plaintext, optionPassword, encSalt, hmacSalt, iv, encKey, macKey)
			if err != nil {
				t.Fatalf("encryptCore: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("re-encrypted blob not byte-identical to official vector:\n got %x\nwant %x", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Judge (c): cross-implementation interop.
//
// The user's existing Go encryptor (bitcomet-monitor/rncryptor.go) could not be
// read in this environment, so instead of inlining its output we interop
// against a minimal, independently-written RNCryptor v3 implementation defined
// below (raw stdlib calls, structured differently from rncryptor.go). This
// exercises the exact same contract: a blob produced elsewhere must Decrypt
// here, and our Encrypt output must be readable by a foreign implementation.
// ---------------------------------------------------------------------------
func TestCrossImplementation_IndependentV3(t *testing.T) {
	password := "cross-impl-client-id"
	for _, plaintext := range [][]byte{
		{},
		[]byte("x"),
		[]byte("independent encryptor plaintext 0123456789"),
		bytes.Repeat([]byte{0x42}, 40),
	} {
		// foreign -> ours
		foreignBlob, err := independentEncryptV3(plaintext, password)
		if err != nil {
			t.Fatalf("independentEncryptV3: %v", err)
		}
		got, err := Decrypt(foreignBlob, password)
		if err != nil {
			t.Fatalf("Decrypt could not open foreign blob (len(pt)=%d): %v", len(plaintext), err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("foreign->ours mismatch for len=%d:\n got %x\nwant %x", len(plaintext), got, plaintext)
		}

		// ours -> foreign
		ourBlob, err := Encrypt(plaintext, password)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got2, err := independentDecryptV3(ourBlob, password)
		if err != nil {
			t.Fatalf("independent decryptor could not open our blob (len(pt)=%d): %v", len(plaintext), err)
		}
		if !bytes.Equal(got2, plaintext) {
			t.Fatalf("ours->foreign mismatch for len=%d:\n got %x\nwant %x", len(plaintext), got2, plaintext)
		}
	}
}

// independentDerive is a separate PBKDF2-HMAC-SHA1 (10000 iters, 32 bytes).
func independentDerive(password string, salt []byte) []byte {
	k, err := pbkdf2.Key(sha1.New, password, salt, 10000, 32)
	if err != nil {
		panic(err)
	}
	return k
}

// independentEncryptV3 is a from-spec RNCryptor v3 password-based encryptor.
func independentEncryptV3(plaintext []byte, password string) ([]byte, error) {
	encSalt := make([]byte, 8)
	hmacSalt := make([]byte, 8)
	iv := make([]byte, 16)
	for _, b := range [][]byte{encSalt, hmacSalt, iv} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	encKey := independentDerive(password, encSalt)
	macKey := independentDerive(password, hmacSalt)

	pad := 16 - len(plaintext)%16
	buf := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(pad)}, pad)...)

	blk, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	ct := make([]byte, len(buf))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, buf)

	var out bytes.Buffer
	out.Write([]byte{3, 1})
	out.Write(encSalt)
	out.Write(hmacSalt)
	out.Write(iv)
	out.Write(ct)
	m := hmac.New(sha256.New, macKey)
	m.Write(out.Bytes())
	out.Write(m.Sum(nil))
	return out.Bytes(), nil
}

// independentDecryptV3 is a from-spec RNCryptor v3 password-based decryptor.
func independentDecryptV3(blob []byte, password string) ([]byte, error) {
	if len(blob) < 34+32 {
		return nil, errors.New("independent: short blob")
	}
	encSalt := blob[2:10]
	hmacSalt := blob[10:18]
	iv := blob[18:34]
	ct := blob[34 : len(blob)-32]
	gotMAC := blob[len(blob)-32:]

	encKey := independentDerive(password, encSalt)
	macKey := independentDerive(password, hmacSalt)
	m := hmac.New(sha256.New, macKey)
	m.Write(blob[:len(blob)-32])
	if !hmac.Equal(m.Sum(nil), gotMAC) {
		return nil, errors.New("independent: integrity")
	}

	blk, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(pt, ct)
	if len(pt) == 0 {
		return nil, errors.New("independent: empty")
	}
	p := int(pt[len(pt)-1])
	if p == 0 || p > 16 || p > len(pt) {
		return nil, errors.New("independent: pad")
	}
	for _, b := range pt[len(pt)-p:] {
		if int(b) != p {
			return nil, errors.New("independent: pad")
		}
	}
	return pt[:len(pt)-p], nil
}
