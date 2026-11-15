package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/crypto"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	streamreader "github.com/Trifocals3537/tessarr/pkg/usenet/fs/reader"
	"github.com/rs/zerolog"
)

const rar5HeaderFixturePassword = "synthetic-fixture-password"

func encryptedRAR5ArchiveFixture(t *testing.T, payload []byte, corruptEnd bool) ([]byte, int64) {
	t.Helper()
	salt := bytes.Repeat([]byte{0x33}, 16)
	keys := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), salt, 0)
	// Public synthetic password-check fixture; a low KDF count keeps tests fast.
	encData := append([]byte{0, 1, 0}, salt...)
	encData = append(encData, keys.PwCheck...)
	archive := append([]byte(RAR5Signature), integrityRAR5Block(RAR5HeaderTypeEncrypt, 0, 0, encData)...)
	appendHeader := func(plain []byte, ivByte byte) {
		iv := bytes.Repeat([]byte{ivByte}, 16)
		archive = append(archive, iv...)
		archive = append(archive, encryptRAR5HeaderFixture(t, plain, keys.Key, iv)...)
	}
	appendHeader(integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, []byte{0}), 0x11)
	fileData := binary.AppendUvarint(nil, RAR5FileFlagHasCRC32)
	fileData = binary.AppendUvarint(fileData, uint64(len(payload)))
	fileData = append(fileData, 0) // attributes
	fileData = binary.LittleEndian.AppendUint32(fileData, crc32.ChecksumIEEE(payload))
	fileData = append(fileData, 0, 0, 9) // stored, host OS, filename length
	fileData = append(fileData, "video.mkv"...)
	appendHeader(integrityRAR5Block(RAR5HeaderTypeFile, RAR5HeaderFlagDataArea, len(payload), fileData), 0x22)
	dataOffset := int64(len(archive))
	archive = append(archive, payload...)
	end := integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{0})
	if corruptEnd {
		end[0] ^= 1
	}
	appendHeader(end, 0x44)
	return archive, dataOffset
}

func TestEncryptedRAR5ArchiveStreamValidation(t *testing.T) {
	for _, scenario := range []string{"valid", "bad final CRC", "wrong password", "partial IV", "partial ciphertext"} {
		t.Run(scenario, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x55}, 64)
			archive, offset := encryptedRAR5ArchiveFixture(t, payload, scenario == "bad final CRC")
			password := rar5HeaderFixturePassword
			if scenario == "wrong password" {
				password = "incorrect-fixture-password"
			}
			if scenario == "partial IV" {
				archive = archive[:len(archive)-17]
			}
			if scenario == "partial ciphertext" {
				archive = archive[:len(archive)-1]
			}
			stream := memoryRAR5HeaderStream(archive)
			if err := stream.Skip(int64(len(RAR5Signature))); err != nil {
				t.Fatal(err)
			}
			result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", password)
			if scenario != "valid" {
				if err == nil || result != nil {
					t.Fatalf("invalid encrypted archive returned partial success: %+v %v", result, err)
				}
				if scenario == "bad final CRC" && !strings.Contains(err.Error(), "CRC mismatch") {
					t.Fatal("final header was not CRC-rejected", err)
				}
				if scenario == "wrong password" && !errors.Is(err, crypto.ErrBadPassword) {
					t.Fatal("password verification changed", err)
				}
				return
			}
			if err != nil || !result.IsHeaderEncrypted || len(result.Files) != 1 {
				t.Fatalf("valid encrypted archive rejected: %+v %v", result, err)
			}
			file := result.Files[0]
			if file.Name != "video.mkv" || file.DataOffset != offset || file.PackedSize != int64(len(payload)) || file.IsEncrypted || stream.Position() != int64(len(archive)) {
				t.Fatalf("file identity or stream alignment changed: %+v position=%d want=%d", file, stream.Position(), len(archive))
			}
		})
	}
}

func TestEncryptedRAR5ProcessArchiveThroughArticles(t *testing.T) {
	for _, chunkSize := range []int{13, 4096} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x55}, 64)
			archive, _ := encryptedRAR5ArchiveFixture(t, payload, false)
			articles := make(map[string][]byte)
			file := nzbparser.NzbFile{Filename: "encrypted.rar"}
			parts := (len(archive) + chunkSize - 1) / chunkSize
			for part, offset := 1, 0; offset < len(archive); part, offset = part+1, offset+chunkSize {
				id := fmt.Sprintf("header-%d@fixture.invalid", part)
				article := integrityYEncPart(archive[offset:min(offset+chunkSize, len(archive))], len(archive), offset, part, parts)
				articles["<"+id+">"] = article
				file.Segments = append(file.Segments, nzbparser.NzbSegment{Number: part, Id: id, Bytes: len(article)})
			}
			p := newContentTestParser(t, articles)
			group := &FileGroup{BaseName: "encrypted", Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{file}}
			if err := p.enrichGroupWithFileInfo(t.Context(), group); err != nil {
				t.Fatal(err)
			}
			files, err := NewRARParser(p.manager, 2, p.logger).Process(t.Context(), group, rar5HeaderFixturePassword)
			if err != nil || len(files) != 1 || files[0].Name != "video.mkv" || files[0].Size != int64(len(payload)) {
				t.Fatalf("encrypted archive article processing: %+v %v", files, err)
			}
			reader, err := streamreader.NewStreamingReader(t.Context(), p.manager, streamreader.NewSegmentMetaSlice(files[0].Segments), streamreader.WithDiskPath(t.TempDir()), streamreader.WithPrefetchAhead(0), streamreader.WithMaxConnections(2))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			got := make([]byte, len(payload))
			if n, err := reader.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got, payload) {
				t.Fatalf("member read after encrypted header: bytes=%d err=%v", n, err)
			}
			if _, err := reader.ReadAt(make([]byte, 1), int64(len(payload))); !errors.Is(err, io.EOF) {
				t.Fatalf("read past member EOF: %v", err)
			}
		})
	}
}
