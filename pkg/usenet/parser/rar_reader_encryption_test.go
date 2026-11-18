package parser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/Trifocals3537/tessarr/pkg/usenet/types"
	"github.com/rs/zerolog"
)

var rar5HeaderTestKey = bytes.Repeat([]byte{0x42}, 32)
var rar5HeaderTestIV = bytes.Repeat([]byte{0x11}, 16)

func encryptRAR5HeaderFixture(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	size := (len(plain) + aes.BlockSize - 1) / aes.BlockSize * aes.BlockSize
	// Padding is outside the header and its CRC. Use non-zero bytes so a
	// reader cannot accidentally treat padding as missing header fields.
	encoded := bytes.Repeat([]byte{0xa5}, size)
	copy(encoded, plain)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(encoded, encoded)
	return encoded
}

func memoryRAR5HeaderStream(data []byte) *rarReader {
	return &rarReader{
		currentSegmentData: data,
		volumes:            []*types.Volume{{Size: int64(len(data))}},
	}
}

func TestEncryptedRAR5MainHeaderPreservesVolumeIndex(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	plain := integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, []byte{3, 1})
	control, _, _, err := p.readRAR5HeaderFromStream(memoryRAR5HeaderStream(plain))
	if err != nil || !bytes.Equal(control.Data, []byte{3, 1}) {
		t.Fatalf("plaintext control: %+v %v", control, err)
	}
	encoded := encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
	got, size, _, err := p.readAndDecryptRAR5Header(memoryRAR5HeaderStream(encoded), rar5HeaderTestKey, rar5HeaderTestIV)
	if err != nil || got.Type != control.Type || got.Flags != control.Flags || !bytes.Equal(got.Data, control.Data) || size != len(encoded) {
		t.Fatalf("encrypted main header: %+v size=%d, want payload=%v size=%d; %v", got, size, control.Data, len(encoded), err)
	}
}

func TestEncryptedRAR5ValidFileAtAESBoundary(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	plain := integrityRAR5Block(RAR5HeaderTypeFile, 0, 0, []byte{0, 0, 0, 0, 0, 4, 'd', 'e', 'm', 'o'})
	control, _, _, err := p.readRAR5HeaderFromStream(memoryRAR5HeaderStream(plain))
	if err != nil || p.parseRAR5FileHeader(control.Data, 0, "fixture.rar", 0, 0, "") == nil {
		t.Fatal("valid plaintext file rejected", err)
	}
	encoded := encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
	stream := memoryRAR5HeaderStream(encoded)
	got, size, dataSize, err := p.readAndDecryptRAR5Header(stream, rar5HeaderTestKey, rar5HeaderTestIV)
	if err != nil {
		t.Fatal(err)
	}
	file := p.parseRAR5FileHeader(got.Data, 0, "fixture.rar", stream.Position(), dataSize, "")
	if size != len(encoded) || stream.Position() != int64(len(encoded)) || !bytes.Equal(got.Data, control.Data) || file == nil || file.Name != "demo" {
		t.Fatalf("ciphertext=%d reported=%d consumed=%d payload=%d want=%d file=%+v", len(encoded), size, stream.Position(), len(got.Data), len(control.Data), file)
	}
}

func TestEncryptedRAR5RejectsBadCRC(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	plain := integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{0})
	plain[0] ^= 1
	if _, _, _, err := p.readRAR5HeaderFromStream(memoryRAR5HeaderStream(plain)); err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatal("plaintext control accepted a bad CRC", err)
	}
	encoded := encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
	if header, _, _, err := p.readAndDecryptRAR5Header(memoryRAR5HeaderStream(encoded), rar5HeaderTestKey, rar5HeaderTestIV); err == nil || header != nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("encrypted header with bad CRC accepted: %+v %v", header, err)
	}
}

func checkedRAR5HeaderFixture(body, encodedSize []byte) []byte {
	if encodedSize == nil {
		encodedSize = binary.AppendUvarint(nil, uint64(len(body)))
	}
	checked := append(append([]byte(nil), encodedSize...), body...)
	return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(checked)), checked...)
}

func assertRAR5HeaderParity(t *testing.T, plain []byte, kind, flags uint64, payload []byte, dataSize int64) {
	t.Helper()
	p := NewRARParser(nil, 1, zerolog.Nop())
	marker := []byte("next header remains unread")
	for _, encrypted := range []bool{false, true} {
		encoded := append([]byte(nil), plain...)
		if encrypted {
			encoded = encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
		}
		stream := memoryRAR5HeaderStream(append(append([]byte(nil), encoded...), marker...))
		var header *rar5HeaderData
		var size int
		var gotDataSize int64
		var err error
		if encrypted {
			header, size, gotDataSize, err = p.readAndDecryptRAR5Header(stream, rar5HeaderTestKey, rar5HeaderTestIV)
		} else {
			header, size, gotDataSize, err = p.readRAR5HeaderFromStream(stream)
		}
		if err != nil {
			t.Fatalf("encrypted=%v: %v", encrypted, err)
		}
		if header.Type != kind || header.Flags != flags || !bytes.Equal(header.Data, payload) || gotDataSize != dataSize {
			t.Fatalf("encrypted=%v: header=%+v dataSize=%d, want type=%d flags=%d payload=%d dataSize=%d", encrypted, header, gotDataSize, kind, flags, len(payload), dataSize)
		}
		if size != len(encoded) || stream.Position() != int64(len(encoded)) {
			t.Fatalf("encrypted=%v: reported=%d consumed=%d want=%d", encrypted, size, stream.Position(), len(encoded))
		}
		next := make([]byte, len(marker))
		if _, err := io.ReadFull(stream, next); err != nil || !bytes.Equal(next, marker) {
			t.Fatalf("encrypted=%v consumed following data: %q %v", encrypted, next, err)
		}
	}
}

func TestEncryptedRAR5HeaderLengthAndPaddingParity(t *testing.T) {
	for _, size := range []int{2, 3, 4, 10, 11, 12, 26, 27, 28, 122, 123, 127, 128, 138, 139, 16377, 16378, 16379, 16383, 16384, 65536} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := bytes.Repeat([]byte{0x37}, size)
			body[0], body[1] = RAR5HeaderTypeMain, 0
			assertRAR5HeaderParity(t, checkedRAR5HeaderFixture(body, nil), RAR5HeaderTypeMain, 0, body[2:], 0)
		})
	}
	// The encoded length must be counted as stored, including legal reserved
	// space in a size vint, rather than inferred by re-encoding its value.
	for _, sizeVint := range [][]byte{{0x84, 0}, {0x84, 0x80, 0}} {
		t.Run(fmt.Sprintf("reserved size bytes %d", len(sizeVint)), func(t *testing.T) {
			body := []byte{RAR5HeaderTypeMain, 0, 3, 1}
			assertRAR5HeaderParity(t, checkedRAR5HeaderFixture(body, sizeVint), RAR5HeaderTypeMain, 0, []byte{3, 1}, 0)
		})
	}
}

func TestEncryptedRAR5HeaderCommonFieldParity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind, flags uint64
		extra, data uint64
		payload     []byte
	}{
		{"extra and data", 1, 3, 4, 300, []byte{7, 8, 9, 10, 11, 12}},
		{"multibyte common fields", 128, 0x83, 1, 128, []byte{0x88, 0x99}},
		{"maximum signed data size", 1, 2, 0, math.MaxInt64, []byte{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := binary.AppendUvarint(nil, tc.kind)
			body = binary.AppendUvarint(body, tc.flags)
			if tc.flags&RAR5HeaderFlagExtraArea != 0 {
				body = binary.AppendUvarint(body, tc.extra)
			}
			if tc.flags&RAR5HeaderFlagDataArea != 0 {
				body = binary.AppendUvarint(body, tc.data)
			}
			body = append(body, tc.payload...)
			assertRAR5HeaderParity(t, checkedRAR5HeaderFixture(body, nil), tc.kind, tc.flags, tc.payload, int64(tc.data))
		})
	}
}

func TestEncryptedRAR5HeaderRejectsMalformedFields(t *testing.T) {
	overflow := append(bytes.Repeat([]byte{0xff}, 9), 2)
	for _, tc := range []struct {
		name       string
		body, size []byte
	}{
		{name: "empty body"},
		{name: "missing flags", body: []byte{1}},
		{name: "missing extra size", body: []byte{1, 1}},
		{name: "missing data size", body: []byte{1, 2}},
		{name: "truncated extra size", body: []byte{1, 1, 0x80}},
		{name: "extra area outside body", body: []byte{1, 1, 8, 0}},
		{name: "unsigned data size", body: binary.AppendUvarint([]byte{1, 2}, uint64(math.MaxInt64)+1)},
		{name: "overflowing type", body: append(append([]byte(nil), overflow...), 0)},
		{name: "overflowing flags", body: append([]byte{1}, overflow...)},
		{name: "oversized header", body: []byte{1, 0}, size: binary.AppendUvarint(nil, 65537)},
		{name: "size vint exceeds format limit", body: []byte{1, 0}, size: []byte{0x82, 0x80, 0x80, 0}},
		{name: "unterminated size", body: []byte{1, 0}, size: bytes.Repeat([]byte{0x80}, 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewRARParser(nil, 1, zerolog.Nop())
			plain := checkedRAR5HeaderFixture(tc.body, tc.size)
			if header, _, _, err := p.readRAR5HeaderFromStream(memoryRAR5HeaderStream(plain)); err == nil || header != nil {
				t.Fatalf("malformed plaintext accepted: %+v %v", header, err)
			}
			encoded := encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
			stream := memoryRAR5HeaderStream(encoded)
			if header, _, _, err := p.readAndDecryptRAR5Header(stream, rar5HeaderTestKey, rar5HeaderTestIV); err == nil || header != nil {
				t.Fatalf("malformed encrypted header accepted: %+v %v", header, err)
			}
			if tc.name == "oversized header" && stream.Position() != aes.BlockSize {
				t.Fatalf("untrusted size caused additional reads: %d", stream.Position())
			}
		})
	}
}

func TestEncryptedRAR5HeaderRejectsTruncationAndBadKeys(t *testing.T) {
	p := NewRARParser(nil, 1, zerolog.Nop())
	body := bytes.Repeat([]byte{0x55}, 139)
	body[0], body[1] = RAR5HeaderTypeMain, 0
	plain := checkedRAR5HeaderFixture(body, nil)
	encoded := encryptRAR5HeaderFixture(t, plain, rar5HeaderTestKey, rar5HeaderTestIV)
	for end := 0; end < len(encoded); end++ {
		stream := memoryRAR5HeaderStream(encoded[:end])
		if header, _, _, err := p.readAndDecryptRAR5Header(stream, rar5HeaderTestKey, rar5HeaderTestIV); err == nil || header != nil {
			t.Fatalf("truncated ciphertext accepted at %d: %+v %v", end, header, err)
		}
	}
	for _, tc := range []struct{ key, iv []byte }{
		{nil, rar5HeaderTestIV}, {make([]byte, 16), rar5HeaderTestIV},
		{make([]byte, 31), rar5HeaderTestIV}, {make([]byte, 33), rar5HeaderTestIV},
		{rar5HeaderTestKey, nil}, {rar5HeaderTestKey, make([]byte, 15)}, {rar5HeaderTestKey, make([]byte, 17)},
	} {
		stream := memoryRAR5HeaderStream(encoded)
		if header, _, _, err := p.readAndDecryptRAR5Header(stream, tc.key, tc.iv); err == nil || header != nil || stream.Position() != 0 {
			t.Fatalf("invalid key/IV reached parsing: %+v %v consumed=%d", header, err, stream.Position())
		}
	}
	for _, corruptAt := range []int{0, 6, len(plain) - 1} {
		bad := append([]byte(nil), plain...)
		bad[corruptAt] ^= 1
		ciphertext := encryptRAR5HeaderFixture(t, bad, rar5HeaderTestKey, rar5HeaderTestIV)
		if header, _, _, err := p.readAndDecryptRAR5Header(memoryRAR5HeaderStream(ciphertext), rar5HeaderTestKey, rar5HeaderTestIV); err == nil || header != nil || !strings.Contains(err.Error(), "CRC mismatch") {
			t.Fatalf("corruption at %d was not CRC-rejected: %+v %v", corruptAt, header, err)
		}
	}
}
