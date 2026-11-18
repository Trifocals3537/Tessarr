package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/crypto"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	streamreader "github.com/Trifocals3537/tessarr/pkg/usenet/fs/reader"
	"github.com/rs/zerolog"
)

type rar5AlignmentMember struct {
	name      string
	payload   []byte
	encrypted bool
	service   bool
}

// Each header and file uses an independent IV. File packed sizes include
// actual ciphertext padding; plain data areas have their exact byte length.
func rar5AlignmentArchive(t *testing.T, members []rar5AlignmentMember) ([]byte, []int64, [][]byte) {
	t.Helper()
	salt := bytes.Repeat([]byte{0x33}, 16)
	keys, err := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), salt, 0)
	if err != nil {
		t.Fatal(err)
	}
	encryption := append([]byte{0, 1, 0}, salt...)
	encryption = append(encryption, keys.PwCheck...)
	archive := append([]byte(RAR5Signature), integrityRAR5Block(RAR5HeaderTypeEncrypt, 0, 0, encryption)...)
	appendHeader := func(plain []byte, ivByte byte) {
		iv := bytes.Repeat([]byte{ivByte}, crypto.BlockSize)
		archive = append(archive, iv...)
		archive = append(archive, encryptRAR5HeaderFixture(t, plain, keys.Key, iv)...)
	}
	appendHeader(integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, []byte{0}), 0x11)
	var offsets []int64
	var packed [][]byte
	for index, member := range members {
		stored := member.payload
		var extra []byte
		if member.encrypted {
			fileSalt := bytes.Repeat([]byte{byte(0x50 + index)}, 16)
			fileIV := bytes.Repeat([]byte{byte(0x60 + index)}, 16)
			fileKeys, err := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), fileSalt, 0)
			if err != nil {
				t.Fatal(err)
			}
			record := append([]byte{RAR5ExtraTypeEncryption, 0, 1, 0}, fileSalt...)
			record = append(record, fileIV...)
			record = append(record, fileKeys.PwCheck...)
			extra = binary.AppendUvarint(nil, uint64(len(record)))
			extra = append(extra, record...)
			stored = encryptRAR5HeaderFixture(t, member.payload, fileKeys.Key, fileIV)
		}
		fileData := binary.AppendUvarint(nil, RAR5FileFlagHasCRC32)
		fileData = binary.AppendUvarint(fileData, uint64(len(member.payload)))
		fileData = append(fileData, 0) // attributes
		fileData = binary.LittleEndian.AppendUint32(fileData, crc32.ChecksumIEEE(member.payload))
		fileData = append(fileData, 0, 0) // stored, host OS
		fileData = binary.AppendUvarint(fileData, uint64(len(member.name)))
		fileData = append(fileData, member.name...)
		kind := uint64(RAR5HeaderTypeFile)
		if member.service {
			kind = RAR5HeaderTypeService
		}
		flags := uint64(RAR5HeaderFlagDataArea)
		if len(extra) > 0 {
			flags |= RAR5HeaderFlagExtraArea
		}
		body := binary.AppendUvarint(nil, kind)
		body = binary.AppendUvarint(body, flags)
		if len(extra) > 0 {
			body = binary.AppendUvarint(body, uint64(len(extra)))
		}
		body = binary.AppendUvarint(body, uint64(len(stored)))
		body = append(body, fileData...)
		body = append(body, extra...)
		checked := binary.AppendUvarint(nil, uint64(len(body)))
		checked = append(checked, body...)
		plain := append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(checked)), checked...)
		appendHeader(plain, byte(0x20+index))
		offsets = append(offsets, int64(len(archive)))
		packed = append(packed, stored)
		archive = append(archive, stored...)
	}
	appendHeader(integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{0}), 0x77)
	return archive, offsets, packed
}

func rar5AlignmentPayload(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index*31 + 7)
	}
	return data
}

func TestEncryptedRAR5DataAreaBoundaries(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, size := range []int{0, 1, 15, 16, 17, 31, 32, 33, 64, 65, 127, 128, 129} {
			t.Run(fmt.Sprintf("encrypted=%v/size=%d", encrypted, size), func(t *testing.T) {
				archive, offsets, packed := rar5AlignmentArchive(t, []rar5AlignmentMember{{name: "video.mkv", payload: rar5AlignmentPayload(size), encrypted: encrypted}})
				marker := []byte("unconsumed archive trailer")
				stream := memoryRAR5HeaderStream(append(append([]byte(nil), archive...), marker...))
				if err := stream.Skip(int64(len(RAR5Signature))); err != nil {
					t.Fatal(err)
				}
				result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", rar5HeaderFixturePassword)
				if err != nil || result == nil || len(result.Files) != 1 {
					t.Fatalf("valid data area rejected: %+v %v", result, err)
				}
				file := result.Files[0]
				if !result.IsHeaderEncrypted || file.Name != "video.mkv" || file.IsEncrypted != encrypted || file.UncompressedSize != int64(size) || file.PackedSize != int64(len(packed[0])) || file.DataOffset != offsets[0] || stream.Position() != int64(len(archive)) {
					t.Fatalf("incorrect file geometry: %+v position=%d want=%d", file, stream.Position(), len(archive))
				}
				got := make([]byte, len(marker))
				if _, err := io.ReadFull(stream, got); err != nil || !bytes.Equal(got, marker) {
					t.Fatal("parser consumed the following trailer", err)
				}
			})
		}
	}
}

func rar5MixedAlignmentMembers() []rar5AlignmentMember {
	return []rar5AlignmentMember{
		{name: "empty.mkv"},
		{name: "plain-one.mkv", payload: rar5AlignmentPayload(1)},
		{name: "encrypted-small.mkv", payload: rar5AlignmentPayload(17), encrypted: true},
		{name: "service", payload: rar5AlignmentPayload(13), service: true},
		{name: "plain-large.mkv", payload: rar5AlignmentPayload(65)},
		{name: "encrypted-aligned.mkv", payload: rar5AlignmentPayload(64), encrypted: true},
	}
}

func TestEncryptedRAR5MixedDataThroughFragmentedArticles(t *testing.T) {
	for _, chunkSize := range []int{13, 17, 4096} {
		t.Run(fmt.Sprint(chunkSize), func(t *testing.T) {
			members := rar5MixedAlignmentMembers()
			archive, _, packed := rar5AlignmentArchive(t, members)
			articles := make(map[string][]byte)
			nzbFile := nzbparser.NzbFile{Filename: "alignment.rar"}
			parts := (len(archive) + chunkSize - 1) / chunkSize
			for part, offset := 1, 0; offset < len(archive); part, offset = part+1, offset+chunkSize {
				id := fmt.Sprintf("alignment-%d@fixture.invalid", part)
				article := integrityYEncPart(archive[offset:min(offset+chunkSize, len(archive))], len(archive), offset, part, parts)
				articles["<"+id+">"] = article
				nzbFile.Segments = append(nzbFile.Segments, nzbparser.NzbSegment{Number: part, Id: id, Bytes: len(article)})
			}
			p := newContentTestParser(t, articles)
			group := &FileGroup{BaseName: "alignment", Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{nzbFile}}
			if err := p.enrichGroupWithFileInfo(t.Context(), group); err != nil {
				t.Fatal(err)
			}
			files, err := NewRARParser(p.manager, 2, p.logger).Process(t.Context(), group, rar5HeaderFixturePassword)
			if err != nil || len(files) != 4 {
				t.Fatalf("mixed archive failed: files=%+v err=%v", files, err)
			}
			for _, file := range files {
				index := -1
				for candidate, member := range members {
					if member.name == file.Name {
						index = candidate
						break
					}
				}
				if index < 0 {
					t.Fatalf("unexpected member %q", file.Name)
				}
				member := members[index]
				if file.Size != int64(len(member.payload)) || file.IsEncrypted != member.encrypted {
					t.Fatalf("incorrect member metadata: %+v", file)
				}
				config := streamreader.EncryptionConfig{Enabled: file.IsEncrypted, Key: file.EncryptionKey, IV: file.EncryptionIV}
				reader, err := streamreader.NewStreamingReaderWithEncryption(t.Context(), p.manager, streamreader.NewSegmentMetaSlice(file.Segments), config, streamreader.WithDiskPath(t.TempDir()), streamreader.WithPrefetchAhead(0), streamreader.WithMaxConnections(2))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				})
				got := make([]byte, len(packed[index]))
				if n, err := reader.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got[:len(member.payload)], member.payload) {
					t.Fatalf("member readback %q: bytes=%d err=%v", file.Name, n, err)
				}
				if member.encrypted && !bytes.Equal(got[len(member.payload):], bytes.Repeat([]byte{0xa5}, len(got)-len(member.payload))) {
					t.Fatal("encrypted padding/decryption changed")
				}
				if n, err := reader.ReadAt(make([]byte, 1), int64(len(got))); n != 0 || !errors.Is(err, io.EOF) {
					t.Fatalf("past physical member EOF: bytes=%d err=%v", n, err)
				}
				// Cross the physical end and check that no following header bytes leak.
				crossing := make([]byte, 2)
				if n, err := reader.ReadAt(crossing, int64(len(got)-1)); n != 1 || !errors.Is(err, io.EOF) || crossing[0] != got[len(got)-1] {
					t.Fatalf("cross-member read %q (encrypted=%v): bytes=%d err=%v", file.Name, file.IsEncrypted, n, err)
				}
			}
		})
	}
}

func TestEncryptedRAR5MixedDataRejectsEveryTruncation(t *testing.T) {
	archive, _, _ := rar5AlignmentArchive(t, rar5MixedAlignmentMembers())
	for end := len(RAR5Signature); end < len(archive); end++ {
		stream := memoryRAR5HeaderStream(archive[:end])
		if err := stream.Skip(int64(len(RAR5Signature))); err != nil {
			t.Fatal(err)
		}
		result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", rar5HeaderFixturePassword)
		if err == nil || result != nil {
			t.Fatalf("truncated archive returned partial success at %d/%d: %+v %v", end, len(archive), result, err)
		}
	}
}

func TestStreamingReaderEOFMissingArticle(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			p := newContentTestParser(t, map[string][]byte{})
			segments := []streamreader.SegmentMeta{{MessageID: "missing@fixture.invalid", Number: 1, Bytes: 64, StartOffset: 0, EndOffset: 63}}
			reader, err := streamreader.NewStreamingReaderWithEncryption(t.Context(), p.manager, segments,
				streamreader.EncryptionConfig{Enabled: encrypted, Key: rar5HeaderTestKey, IV: rar5HeaderTestIV},
				streamreader.WithDiskPath(t.TempDir()), streamreader.WithPrefetchAhead(0), streamreader.WithMaxConnections(1),
				func(c *streamreader.Config) { c.MaxRetries = 1 })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			got := bytes.Repeat([]byte{0xcc}, 2)
			if n, err := reader.ReadAt(got, 63); n != 0 || !nntp.IsArticleNotFoundError(err) || errors.Is(err, io.EOF) || !bytes.Equal(got, []byte{0xcc, 0xcc}) {
				t.Fatalf("NNTP failure became EOF/data: n=%d err=%v data=%x", n, err, got)
			}
		})
	}
}
