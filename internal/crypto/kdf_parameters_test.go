package crypto

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestArchiveEncryptionParametersRejectUnsafeValues(t *testing.T) {
	for _, tc := range []struct{ version, count byte }{
		{0, 25}, {0, 63}, {0, 255}, {1, 0}, {127, 0},
	} {
		t.Run(fmt.Sprintf("version=%d/count=%d", tc.version, tc.count), func(t *testing.T) {
			data := append([]byte{tc.version, 0, tc.count}, bytes.Repeat([]byte{0x33}, 16)...)
			if header, err := ParseEncryptionHeader(data); err == nil || header != nil {
				t.Fatalf("unsafe parameters accepted: %+v %v", header, err)
			}
		})
	}
}

func TestArchiveEncryptionParametersAcceptSupportedBounds(t *testing.T) {
	for _, count := range []byte{0, 1, 23, 24} {
		data := append([]byte{0, 0, count}, bytes.Repeat([]byte{0x33}, 16)...)
		header, err := ParseEncryptionHeader(data)
		if err != nil || header.KdfCount != int(count) || header.Version != 0 || len(header.Salt) != 16 {
			t.Fatalf("supported count %d rejected: %+v %v", count, header, err)
		}
	}
}

func TestKDFIterationScheduleBounds(t *testing.T) {
	for _, count := range []int{0, 1, 8, MaxKdfCount} {
		got, err := kdfIterations(count)
		if err != nil || got != uint32(1)<<uint(count) || got == 0 {
			t.Fatalf("count=%d iterations=%d err=%v", count, got, err)
		}
	}
	for _, count := range []int{-1, MaxKdfCount + 1, 31, 32, 63, 64, 255, int(^uint(0) >> 1)} {
		if got, err := kdfIterations(count); !errors.Is(err, ErrInvalidKDFCount) || got != 0 {
			t.Fatalf("invalid count=%d iterations=%d err=%v", count, got, err)
		}
	}
}

func TestDeriveKeysRejectsInvalidCounts(t *testing.T) {
	if os.Getenv("TESSARR_KDF_REJECTION_HELPER") == "1" {
		invalid := []int{-1, int(^uint(0) >> 1)}
		for count := MaxKdfCount + 1; count <= 255; count++ {
			invalid = append(invalid, count)
		}
		for _, count := range invalid {
			keys, err := DeriveKeys([]byte("synthetic-password"), bytes.Repeat([]byte{0x33}, 16), count)
			if keys != nil || !errors.Is(err, ErrInvalidKDFCount) {
				t.Errorf("unsafe count %d returned keys or wrong error: %v", count, err)
			}
		}
		return
	}
	// A regression must fail within a bounded worker, rather than letting a
	// previously unsafe count turn the test runner into a long derivation job.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDeriveKeysRejectsInvalidCounts$")
	cmd.Env = append(os.Environ(), "TESSARR_KDF_REJECTION_HELPER=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("invalid-count rejection worker failed: %v context=%v\n%s", err, ctx.Err(), output)
	}
}

func TestDeriveKeysMatchesStandardPBKDF2(t *testing.T) {
	for _, password := range []string{"synthetic-password", "fixture-\u00e9"} {
		for _, count := range []int{0, 1, 4, 8} {
			t.Run(fmt.Sprintf("%s/%d", password, count), func(t *testing.T) {
				salt := bytes.Repeat([]byte{0x33}, 16)
				got, err := DeriveKeys([]byte(password), salt, count)
				if err != nil {
					t.Fatal(err)
				}
				reference := func(extra int) []byte {
					key, err := pbkdf2.Key(sha256.New, password, salt, (1<<uint(count))+extra, AESKeySize)
					if err != nil {
						t.Fatal(err)
					}
					return key
				}
				if !bytes.Equal(got.Key, reference(0)) || !bytes.Equal(got.CheckKey, reference(16)) {
					t.Fatal("RAR key derivation changed")
				}
				checkMaterial := reference(32)
				check := make([]byte, PwCheckSize)
				for i, value := range checkMaterial {
					check[i%PwCheckSize] ^= value
				}
				sum := sha256.Sum256(check)
				check = append(check, sum[:4]...)
				if !bytes.Equal(got.PwCheck, check) || !VerifyPassword(got, check) {
					t.Fatal("password check changed")
				}
				check[0] ^= 1
				if VerifyPassword(got, check) {
					t.Fatal("invalid password check accepted")
				}
			})
		}
	}
}

func TestArchiveEncryptionParameterRecordBounds(t *testing.T) {
	for count := 0; count <= 255; count++ {
		data := append([]byte{0, 1, byte(count)}, bytes.Repeat([]byte{0x33}, 16)...)
		data = append(data, bytes.Repeat([]byte{0x44}, 12)...)
		got, err := ParseEncryptionHeader(data)
		if count <= MaxKdfCount {
			if err != nil || got.KdfCount != count || !got.HasPwCheck || len(got.PwCheck) != 12 {
				t.Fatalf("supported count=%d rejected: %+v %v", count, got, err)
			}
		} else if got != nil || !errors.Is(err, ErrInvalidKDFCount) {
			t.Fatalf("unsafe byte count=%d accepted: %+v %v", count, got, err)
		}
	}
	for version := 1; version < 128; version++ {
		data := append([]byte{byte(version), 0, 0}, bytes.Repeat([]byte{0x33}, 16)...)
		if got, err := ParseEncryptionHeader(data); got != nil || !errors.Is(err, ErrUnsupportedEncryptionVersion) {
			t.Fatalf("unsupported version=%d accepted: %+v %v", version, got, err)
		}
	}
	for _, hasCheck := range []byte{0, 1} {
		data := append([]byte{0, hasCheck, 0}, bytes.Repeat([]byte{0x33}, 16)...)
		if hasCheck != 0 {
			data = append(data, bytes.Repeat([]byte{0x44}, 12)...)
		}
		for end := 0; end < len(data); end++ {
			if got, err := ParseEncryptionHeader(data[:end]); got != nil || !errors.Is(err, ErrInvalidData) {
				t.Fatalf("truncated record accepted at %d/check=%d: %+v %v", end, hasCheck, got, err)
			}
		}
	}
}
