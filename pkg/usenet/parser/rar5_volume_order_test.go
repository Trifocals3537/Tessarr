package parser

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/crypto"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	streamreader "github.com/Trifocals3537/tessarr/pkg/usenet/fs/reader"
)

type rar5OrderFixtureBlock struct {
	header []byte
	data   []byte
}

func rar5OrderFixtureArchive(t *testing.T, encryptedHeaders bool, blocks ...rar5OrderFixtureBlock) []byte {
	t.Helper()
	archive := []byte(RAR5Signature)
	var key []byte
	if encryptedHeaders {
		salt := bytes.Repeat([]byte{0x33}, 16)
		keys, err := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), salt, 0)
		if err != nil {
			t.Fatal(err)
		}
		key = keys.Key
		body := append([]byte{0, 1, 0}, salt...)
		body = append(body, keys.PwCheck...)
		archive = append(archive, integrityRAR5Block(RAR5HeaderTypeEncrypt, 0, 0, body)...)
	}
	for index, block := range blocks {
		if encryptedHeaders {
			iv := bytes.Repeat([]byte{byte(0x11 + index)}, 16)
			archive = append(archive, iv...)
			archive = append(archive, encryptRAR5HeaderFixture(t, block.header, key, iv)...)
		} else {
			archive = append(archive, block.header...)
		}
		archive = append(archive, block.data...)
	}
	return archive
}

func rar5OrderFixtureFile(name string, payload []byte, logicalSize int, splitFlags uint64, extra []byte, dataCRC ...uint32) rar5OrderFixtureBlock {
	body := binary.AppendUvarint(nil, RAR5FileFlagHasCRC32)
	body = binary.AppendUvarint(body, uint64(logicalSize))
	body = append(body, 0) // attributes
	checksum := crc32.ChecksumIEEE(payload)
	if len(dataCRC) > 0 {
		checksum = dataCRC[0]
	}
	body = binary.LittleEndian.AppendUint32(body, checksum)
	body = append(body, 0, 0) // stored, host OS
	body = binary.AppendUvarint(body, uint64(len(name)))
	body = append(body, name...)
	flags := uint64(RAR5HeaderFlagDataArea) | splitFlags
	if len(extra) > 0 {
		flags |= RAR5HeaderFlagExtraArea
	}
	header := binary.AppendUvarint(nil, RAR5HeaderTypeFile)
	header = binary.AppendUvarint(header, flags)
	if len(extra) > 0 {
		header = binary.AppendUvarint(header, uint64(len(extra)))
	}
	header = binary.AppendUvarint(header, uint64(len(payload)))
	header = append(header, body...)
	header = append(header, extra...)
	return rar5OrderFixtureBlock{header: checkedRAR5HeaderFixture(header, nil), data: payload}
}

func rar5OrderFixtureVolume(t *testing.T, main []byte, endFlags uint64, payload []byte, logicalSize int, splitFlags uint64, encryptedHeaders bool, dataCRC ...uint32) []byte {
	t.Helper()
	return rar5OrderFixtureArchive(t, encryptedHeaders,
		rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, main)},
		rar5OrderFixtureFile("video.mkv", payload, logicalSize, splitFlags, nil, dataCRC...),
		rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, binary.AppendUvarint(nil, endFlags))},
	)
}

func rar5OrderFixtureGroup(t *testing.T, volumes [][]byte, labels []int, chunk int) (*NZBParser, *FileGroup) {
	t.Helper()
	articles := make(map[string][]byte)
	group := &FileGroup{BaseName: "Demo", Type: storage.NZBFileTypeRar}
	for label, volumeIndex := range labels {
		volume := volumes[volumeIndex]
		file := nzbparser.NzbFile{Number: 7, Filename: fmt.Sprintf("Demo.part%02d.rar", label+1)}
		partSize := chunk + volumeIndex%3
		parts := (len(volume) + partSize - 1) / partSize
		for part, offset := 1, 0; offset < len(volume); part, offset = part+1, offset+partSize {
			id := fmt.Sprintf("volume-%d-article-%d@fixture.invalid", volumeIndex, part)
			article := integrityYEncPart(volume[offset:min(offset+partSize, len(volume))], len(volume), offset, part, parts)
			articles["<"+id+">"] = article
			file.Segments = append(nzbparser.NzbSegments{{Number: part, Id: id, Bytes: len(article)}}, file.Segments...)
		}
		group.Files = append([]nzbparser.NzbFile{file}, group.Files...)
	}
	p := newContentTestParser(t, articles)
	if err := p.enrichGroupWithFileInfo(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	return p, group
}

func TestRAR5EmbeddedVolumeOrderReadback(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted-headers=%v", encrypted), func(t *testing.T) {
			// Exact filename-to-header permutation retained in #20.
			labels := []int{7, 8, 10, 9, 11, 12, 0, 1, 2, 3, 4, 5, 6}
			var volumes [][]byte
			var want []byte
			for index := range labels {
				payload := make([]byte, 137+index*23)
				for offset := range payload {
					payload[offset] = byte(index*37 + offset*19 + offset/11)
				}
				want = append(want, payload...)
			}
			at := 0
			for index := range labels {
				main := []byte{RAR5MainFlagVolume}
				if index > 0 {
					main[0] |= RAR5MainFlagVolumeNumber
					main = binary.AppendUvarint(main, uint64(index))
				}
				split, next := uint64(0), uint64(0)
				if index > 0 {
					split |= RAR5HeaderFlagSplitBefore
				}
				if index < len(labels)-1 {
					split |= RAR5HeaderFlagSplitAfter
					next = 1
				}
				end := at + 137 + index*23
				checksum := crc32.ChecksumIEEE(want[at:end])
				if index == len(labels)-1 {
					checksum = crc32.ChecksumIEEE(want)
				}
				volumes = append(volumes, rar5OrderFixtureVolume(t, main, next, want[at:end], len(want), split, encrypted, checksum))
				at = end
			}
			p, group := rar5OrderFixtureGroup(t, volumes, labels, 17)
			password := ""
			if encrypted {
				password = rar5HeaderFixturePassword
			}
			files, err := NewRARParser(p.manager, 3, p.logger).Process(t.Context(), group, password)
			if err != nil || len(files) != 1 {
				t.Fatalf("complete archive rejected: files=%d err=%v", len(files), err)
			}
			if files[0].Size != int64(len(want)) {
				t.Fatalf("wrong logical size: %d", files[0].Size)
			}
			assertRAR5OrderedReadback(t, p, files[0], want)
			// Exercise NZB grouping, geometry, import probes, and persisted file
			// metadata as well as the direct archive parser above.
			var xml strings.Builder
			xml.WriteString("<nzb>")
			for _, file := range group.Files {
				fmt.Fprintf(&xml, `<file poster="fixture" date="1" subject="&quot;%s&quot; yEnc (1/%d)"><groups><group>alt.test</group></groups><segments>`, file.Filename, len(file.Segments))
				for _, segment := range file.Segments {
					fmt.Fprintf(&xml, `<segment number="%d" bytes="%d">%s</segment>`, segment.Number, segment.Bytes, segment.Id)
				}
				xml.WriteString("</segments></file>")
			}
			xml.WriteString("</nzb>")
			entry, groups, err := p.Parse(t.Context(), "Synthetic.Release.nzb", []byte(xml.String()))
			if err != nil {
				t.Fatal(err)
			}
			entry.Password = password
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("full NZB admission failed: %+v %v", parsed, err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
			reader, err := streamreader.NewStreamingReader(t.Context(), p.manager, streamreader.NewSegmentMetaSlice(files[0].Segments), streamreader.WithDiskPath(t.TempDir()), streamreader.WithPrefetchAhead(0), streamreader.WithMaxConnections(2))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			got := make([]byte, len(want))
			if n, err := reader.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got, want) {
				t.Fatalf("reconstructed bytes mismatch: n=%d err=%v", n, err)
			}
			for _, offset := range []int{0, 125, 1300, len(want) - 40} {
				got := make([]byte, 31)
				if n, err := reader.ReadAt(got, int64(offset)); err != nil || n != len(got) || !bytes.Equal(got, want[offset:offset+len(got)]) {
					t.Fatalf("random read at %d: n=%d err=%v", offset, n, err)
				}
			}
			if n, err := reader.ReadAt(make([]byte, 1), int64(len(want))); n != 0 || err != io.EOF {
				t.Fatalf("member EOF changed: n=%d err=%v", n, err)
			}
		})
	}
}

func assertRAR5OrderedReadback(t *testing.T, p *NZBParser, file *storage.NZBFile, want []byte) {
	t.Helper()
	encoded, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	var restored storage.NZBFile
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	var offset int64
	for _, segment := range restored.Segments {
		if segment.Source == nil || segment.StartOffset != offset || segment.EndOffset != offset+segment.Bytes-1 {
			t.Fatalf("lost provenance or contiguous member geometry: %+v", segment)
		}
		offset += segment.Bytes
	}
	reader, err := streamreader.NewStreamingReaderWithEncryption(t.Context(), p.manager, streamreader.NewSegmentMetaSlice(restored.Segments), streamreader.EncryptionConfig{Enabled: restored.IsEncrypted, Key: restored.EncryptionKey, IV: restored.EncryptionIV}, streamreader.WithDiskPath(t.TempDir()), streamreader.WithPrefetchAhead(0), streamreader.WithMaxConnections(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	got := make([]byte, len(want))
	if n, err := reader.ReadAt(got, 0); err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("restored member mismatch: n=%d err=%v", n, err)
	}
	for offset := len(want) - 1; offset >= 0; offset -= 29 {
		got := make([]byte, min(31, len(want)-offset))
		if n, err := reader.ReadAt(got, int64(offset)); err != nil || n != len(got) || !bytes.Equal(got, want[offset:offset+len(got)]) {
			t.Fatalf("restored random read at %d: n=%d err=%v", offset, n, err)
		}
	}
}

func TestRAR5EmbeddedOrderRejectsInvalidArchiveSets(t *testing.T) {
	tests := []struct{ name, wantError string }{
		{"control", ""}, {"duplicate first", "duplicate RAR5 volume"},
		{"duplicate continuation", "duplicate RAR5 volume"}, {"missing first", "volume sequence"},
		{"index gap", "volume sequence"}, {"nonvolume member", "volume sequence"},
		{"zero explicit index", "omit its number"}, {"missing number", "read RAR5 volume number"},
		{"number without volume flag", "without volume flag"}, {"overflowed number", "vint too large"},
		{"maximum number", "volume sequence"}, {"premature terminal", "terminal marker"},
		{"missing terminal", "terminal marker"}, {"broken split chain", "broken volume continuation"},
		{"size conflict", "inconsistent RAR member"}, {"missing member end", "missing final volume"},
		{"main CRC", "CRC mismatch"}, {"end CRC", "CRC mismatch"},
		{"missing main", "missing RAR5 main"}, {"duplicate main", "duplicate RAR5 main"},
		{"mixed header encryption", "inconsistent RAR5 header encryption"}, {"mixed format", "signature mismatch"},
	}
	for _, encrypted := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("encrypted=%v/%s", encrypted, tc.name), func(t *testing.T) {
				mains := [][]byte{{1}, {3, 1}, {3, 2}}
				tails := []uint64{1, 1, 0}
				splits := []uint64{16, 24, 8}
				totals := []int{195, 195, 195}
				switch tc.name {
				case "duplicate first":
					mains[1] = []byte{1}
				case "duplicate continuation":
					mains[2] = []byte{3, 1}
				case "missing first":
					mains = [][]byte{{3, 1}, {3, 2}, {3, 3}}
					tails = []uint64{1, 0, 0}
				case "index gap":
					mains[1] = []byte{3, 3}
				case "nonvolume member":
					mains[1] = []byte{0}
				case "zero explicit index":
					mains[0] = []byte{3, 0}
				case "missing number":
					mains[1] = []byte{3}
				case "number without volume flag":
					mains[1] = []byte{2, 1}
				case "overflowed number":
					mains[1] = append([]byte{3}, bytes.Repeat([]byte{0xff}, 10)...)
				case "maximum number":
					mains[1] = binary.AppendUvarint([]byte{3}, math.MaxUint64)
				case "premature terminal":
					tails[0] = 0
				case "missing terminal":
					tails[2] = 1
				case "broken split chain":
					splits[1] = 0
				case "size conflict":
					totals[1]++
				case "missing member end":
					splits[2] |= RAR5HeaderFlagSplitAfter
				}
				var volumes [][]byte
				var want []byte
				for index := range 3 {
					payload := bytes.Repeat([]byte{byte('A' + index)}, 64+index)
					want = append(want, payload...)
					checksum := crc32.ChecksumIEEE(payload)
					if index == 2 {
						checksum = crc32.ChecksumIEEE(want)
					}
					blocks := []rar5OrderFixtureBlock{
						{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, mains[index])},
						rar5OrderFixtureFile("video.mkv", payload, totals[index], splits[index], nil, checksum),
						{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, binary.AppendUvarint(nil, tails[index]))},
					}
					if index == 1 {
						switch tc.name {
						case "main CRC":
							blocks[0].header[0] ^= 1
						case "end CRC":
							blocks[2].header[0] ^= 1
						case "missing main":
							blocks = blocks[1:]
						case "duplicate main":
							blocks = append([]rar5OrderFixtureBlock{blocks[0]}, blocks...)
						}
					}
					mode := encrypted
					if index == 1 && tc.name == "mixed header encryption" {
						mode = !mode
					}
					volume := rar5OrderFixtureArchive(t, mode, blocks...)
					if index == 1 && tc.name == "mixed format" {
						copy(volume, []byte(RAR4Signature))
					}
					volumes = append(volumes, volume)
				}
				p, group := rar5OrderFixtureGroup(t, volumes, []int{0, 1, 2}, 13)
				files, err := NewRARParser(p.manager, 3, p.logger).Process(t.Context(), group, rar5HeaderFixturePassword)
				if tc.wantError != "" {
					if err == nil || len(files) != 0 || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("invalid archive admission: files=%d error=%v, want %q", len(files), err, tc.wantError)
					}
					return
				}
				if err != nil || len(files) != 1 {
					t.Fatalf("valid control: files=%d error=%v", len(files), err)
				}
				assertRAR5OrderedReadback(t, p, files[0], want)
			})
		}
	}
}

func TestRAR5EmbeddedOrderPreservesEncryptedData(t *testing.T) {
	for _, encryptedHeaders := range []bool{false, true} {
		t.Run(fmt.Sprint(encryptedHeaders), func(t *testing.T) {
			plain := rar5AlignmentPayload(521)
			salt, iv := bytes.Repeat([]byte{0x55}, 16), bytes.Repeat([]byte{0x66}, 16)
			keys, err := crypto.DeriveKeys([]byte(rar5HeaderFixturePassword), salt, 0)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext := encryptRAR5HeaderFixture(t, plain, keys.Key, iv)
			record := append([]byte{RAR5ExtraTypeEncryption, 0, 1, 0}, salt...)
			record = append(record, iv...)
			record = append(record, keys.PwCheck...)
			extra := binary.AppendUvarint(nil, uint64(len(record)))
			extra = append(extra, record...)
			bounds := []int{0, 137, 356, len(ciphertext)}
			var volumes [][]byte
			for index := range 3 {
				main := []byte{1}
				if index > 0 {
					main = []byte{3, byte(index)}
				}
				split, next := uint64(0), uint64(0)
				if index > 0 {
					split |= RAR5HeaderFlagSplitBefore
				}
				if index < 2 {
					split |= RAR5HeaderFlagSplitAfter
					next = 1
				}
				checksum := crc32.ChecksumIEEE(ciphertext[bounds[index]:bounds[index+1]])
				if index == 2 {
					checksum = crc32.ChecksumIEEE(plain)
				}
				volumes = append(volumes, rar5OrderFixtureArchive(t, encryptedHeaders,
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, main)},
					rar5OrderFixtureFile("video.mkv", ciphertext[bounds[index]:bounds[index+1]], len(plain), split, extra, checksum),
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, binary.AppendUvarint(nil, next))},
				))
			}
			p, group := rar5OrderFixtureGroup(t, volumes, []int{2, 0, 1}, 17)
			files, err := NewRARParser(p.manager, 2, p.logger).Process(t.Context(), group, rar5HeaderFixturePassword)
			if err != nil || len(files) != 1 || !files[0].IsEncrypted || files[0].Size != int64(len(plain)) {
				t.Fatalf("encrypted data member: %+v %v", files, err)
			}
			want := append(append([]byte(nil), plain...), bytes.Repeat([]byte{0xa5}, len(ciphertext)-len(plain))...)
			assertRAR5OrderedReadback(t, p, files[0], want)
		})
	}
}

func TestRAR5EmbeddedOrderRetainsLegacyPerFileGeometry(t *testing.T) {
	want := append(bytes.Repeat([]byte{'A'}, 100), bytes.Repeat([]byte{'B'}, 67)...)
	volumes := [][]byte{
		rar5OrderFixtureVolume(t, []byte{1}, 1, want[:100], len(want), 16, false),
		rar5OrderFixtureVolume(t, []byte{3, 1}, 0, want[100:], len(want), 8, false, crc32.ChecksumIEEE(want)),
	}
	p, group := rar5OrderFixtureGroup(t, volumes, []int{1, 0}, 10000)
	group.fileMeta = nil
	group.metadata = &fileAnalysisResult{fileSize: int64(len(volumes[1])), lastFileSize: int64(len(volumes[0])), segmentSize: 10000}
	files, err := NewRARParser(p.manager, 2, p.logger).Process(t.Context(), group, "")
	if err != nil || len(files) != 1 {
		t.Fatalf("legacy geometry: files=%d error=%v", len(files), err)
	}
	assertRAR5OrderedReadback(t, p, files[0], want)
}

func TestRAR5EmbeddedOrderKeepsPasswordErrors(t *testing.T) {
	volume := rar5OrderFixtureVolume(t, []byte{0}, 0, []byte("fixture"), 7, 0, true)
	for _, password := range []string{"", "incorrect-password"} {
		t.Run(password, func(t *testing.T) {
			p, group := rar5OrderFixtureGroup(t, [][]byte{volume}, []int{0}, 13)
			files, err := NewRARParser(p.manager, 2, p.logger).Process(t.Context(), group, password)
			if err == nil || len(files) != 0 {
				t.Fatal("accepted encrypted headers without their password")
			}
			if password != "" && !errors.Is(err, crypto.ErrBadPassword) {
				t.Fatalf("password error identity changed: %v", err)
			}
		})
	}
}
