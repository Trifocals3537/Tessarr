package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Trifocals3537/tessarr/internal/crypto"
	"github.com/rs/zerolog"
)

func rar5FileEncryptionRecordFixture(version uint64, count byte) []byte {
	record := binary.AppendUvarint(nil, RAR5ExtraTypeEncryption)
	record = binary.AppendUvarint(record, version)
	record = append(record, 0, count)
	record = append(record, bytes.Repeat([]byte{0x33}, 16)...)
	return append(record, bytes.Repeat([]byte{0x11}, 16)...)
}

func rar5FileWithExtraRecordFixture(record []byte) []byte {
	data := []byte{0, 0, 0, 0, 0, 9}
	data = append(data, "video.mkv"...)
	data = binary.AppendUvarint(data, uint64(len(record)))
	return append(data, record...)
}

func TestRAR5FileEncryptionRejectsUnsafeParametersWithoutPassword(t *testing.T) {
	for _, tc := range []struct {
		version uint64
		count   byte
	}{{0, 25}, {0, 63}, {0, 255}, {1, 0}, {127, 0}} {
		t.Run(fmt.Sprintf("version=%d/count=%d", tc.version, tc.count), func(t *testing.T) {
			data := rar5FileWithExtraRecordFixture(rar5FileEncryptionRecordFixture(tc.version, tc.count))
			p := NewRARParser(nil, 1, zerolog.Nop())
			// Empty password deliberately avoids all key derivation on the old
			// implementation while reproducing unsafe metadata acceptance.
			if file := p.parseRAR5FileHeader(data, 0, "fixture.rar", 0, 0, ""); file != nil {
				t.Fatalf("unsafe file encryption parameters accepted: %+v", file)
			}
		})
	}
}

func TestRAR5FileEncryptionSupportedParameters(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	for count := 0; count <= 255; count++ {
		data := rar5FileWithExtraRecordFixture(rar5FileEncryptionRecordFixture(0, byte(count)))
		got := p.parseRAR5FileHeader(data, 0, "fixture.rar", 0, 0, "")
		if count <= crypto.MaxKdfCount {
			if got == nil || !got.IsEncrypted || len(got.EncryptionIV) != 16 || len(got.EncryptionKey) != 0 {
				t.Fatalf("supported metadata rejected at count %d: %+v", count, got)
			}
		} else if got != nil {
			t.Fatalf("unsafe count %d accepted", count)
		}
	}
	for _, flags := range []byte{0, 1, 3} {
		t.Run(fmt.Sprintf("flags=%d", flags), func(t *testing.T) {
			record := rar5FileEncryptionRecordFixture(0, 2)
			record[2] = flags
			keys, err := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), bytes.Repeat([]byte{0x33}, 16), 2)
			if err != nil {
				t.Fatal(err)
			}
			if flags&1 != 0 {
				record = append(record, keys.PwCheck...)
			}
			got := p.parseRAR5FileHeader(rar5FileWithExtraRecordFixture(record), 0, "fixture.rar", 0, 0, rar5HeaderFixturePassword)
			if got == nil || !bytes.Equal(got.EncryptionKey, keys.Key) || !bytes.Equal(got.EncryptionIV, bytes.Repeat([]byte{0x11}, 16)) {
				t.Fatalf("valid file encryption parameters changed: %+v", got)
			}
			if flags&1 != 0 && p.parseRAR5FileHeader(rar5FileWithExtraRecordFixture(record), 0, "fixture.rar", 0, 0, "incorrect-fixture-password") != nil {
				t.Fatal("incorrect file password check accepted")
			}
		})
	}
}

func TestRAR5FileEncryptionRecordBounds(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	record := rar5FileEncryptionRecordFixture(0, 0)
	for end := 1; end < len(record); end++ {
		if got := p.parseRAR5FileHeader(rar5FileWithExtraRecordFixture(record[:end]), 0, "fixture.rar", 0, 0, ""); got != nil {
			t.Fatalf("truncated encryption record accepted at %d", end)
		}
	}
	checked := append([]byte(nil), record...)
	checked[2] = 1
	for checkSize := 0; checkSize < 12; checkSize++ {
		partial := append(append([]byte(nil), checked...), make([]byte, checkSize)...)
		if got := p.parseRAR5FileHeader(rar5FileWithExtraRecordFixture(partial), 0, "fixture.rar", 0, 0, ""); got != nil {
			t.Fatalf("partial password check accepted at %d", checkSize)
		}
	}
	// Unknown records, including multi-byte record types and a type-only
	// record, must skip exactly their declared body before the encryption record.
	for _, unknown := range [][]byte{{0x7f}, {0x80, 1, 0x55, 0x66}} {
		withUnknown := rar5FileWithExtraRecordFixture(unknown)
		withUnknown = binary.AppendUvarint(withUnknown, uint64(len(record)))
		withUnknown = append(withUnknown, record...)
		if got := p.parseRAR5FileHeader(withUnknown, 0, "fixture.rar", 0, 0, ""); got == nil || !got.IsEncrypted {
			t.Fatalf("unknown record obscured valid encryption metadata: %+v", got)
		}
	}
	duplicate := rar5FileWithExtraRecordFixture(record)
	duplicate = binary.AppendUvarint(duplicate, uint64(len(record)))
	duplicate = append(duplicate, record...)
	if p.parseRAR5FileHeader(duplicate, 0, "fixture.rar", 0, 0, "") != nil {
		t.Fatal("duplicate encryption records accepted")
	}
	// A short declared record cannot consume the following record as its IV.
	spill := rar5FileWithExtraRecordFixture(record[:len(record)-16])
	spill = binary.AppendUvarint(spill, 17)
	spill = append(spill, bytes.Repeat([]byte{0x11}, 17)...)
	if p.parseRAR5FileHeader(spill, 0, "fixture.rar", 0, 0, "") != nil {
		t.Fatal("encryption fields crossed an extra-record boundary")
	}
}

func TestRAR5FileEncryptionRejectsBeforeDerivation(t *testing.T) {
	if os.Getenv("TESSARR_FILE_KDF_REJECTION_HELPER") == "1" {
		p := NewRARParser(nil, 1, zerolog.Nop())
		for _, count := range []byte{25, 63, 255} {
			data := rar5FileWithExtraRecordFixture(rar5FileEncryptionRecordFixture(0, count))
			if p.parseRAR5FileHeader(data, 0, "fixture.rar", 0, 0, rar5HeaderFixturePassword) != nil {
				t.Errorf("unsafe count %d reached usable metadata", count)
			}
		}
		// Even a supported maximum count must not run when its IV/check is
		// incomplete. The worker deadline bounds a future regression here.
		record := rar5FileEncryptionRecordFixture(0, crypto.MaxKdfCount)
		partialCheck := append([]byte(nil), record...)
		partialCheck[2] = 1
		partialCheck = append(partialCheck, make([]byte, 11)...)
		// Repetition makes entering the expensive loop observable through the
		// worker deadline, without relying on a tight per-call timing assertion.
		for repeat := 0; repeat < 32; repeat++ {
			for _, partial := range [][]byte{record[:len(record)-1], partialCheck} {
				if p.parseRAR5FileHeader(rar5FileWithExtraRecordFixture(partial), 0, "fixture.rar", 0, 0, rar5HeaderFixturePassword) != nil {
					t.Error("incomplete maximum-work record accepted")
				}
			}
		}
		// A complete maximum-work record must also wait for validation of
		// every following record, including duplicates and invalid lengths.
		complete := rar5FileWithExtraRecordFixture(record)
		duplicate := binary.AppendUvarint(nil, uint64(len(record)))
		duplicate = append(duplicate, record...)
		for repeat := 0; repeat < 32; repeat++ {
			for _, suffix := range [][]byte{duplicate, {0}, {0x80}, {2, 0x7f}, {1, 0x80}} {
				data := append(append([]byte(nil), complete...), suffix...)
				if p.parseRAR5FileHeader(data, 0, "fixture.rar", 0, 0, rar5HeaderFixturePassword) != nil {
					t.Error("invalid trailing record accepted")
				}
			}
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRAR5FileEncryptionRejectsBeforeDerivation$")
	cmd.Env = append(os.Environ(), "TESSARR_FILE_KDF_REJECTION_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("file-parameter rejection worker failed: %v context=%v\n%s", err, ctx.Err(), output)
	}
}

func TestRAR5ArchiveStreamRejectsUnsafeParameters(t *testing.T) {
	for _, password := range []string{"", rar5HeaderFixturePassword} {
		for _, tc := range []struct {
			version byte
			count   byte
			want    error
		}{{0, 25, crypto.ErrInvalidKDFCount}, {0, 63, crypto.ErrInvalidKDFCount}, {0, 255, crypto.ErrInvalidKDFCount}, {1, 0, crypto.ErrUnsupportedEncryptionVersion}} {
			data := append([]byte{tc.version, 0, tc.count}, bytes.Repeat([]byte{0x33}, 16)...)
			stream := memoryRAR5HeaderStream(integrityRAR5Block(RAR5HeaderTypeEncrypt, 0, 0, data))
			result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", password)
			if result != nil || !errors.Is(err, tc.want) {
				t.Fatalf("unsafe archive parameters were not rejected before an encrypted read: %+v %v", result, err)
			}
		}
	}
}
