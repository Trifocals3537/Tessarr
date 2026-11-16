package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/config"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/rs/zerolog"
)

// Audit reproducer: both standalone controls must work independently and when
// posted together. Nothing about these descriptive filenames is obfuscated.
func TestRARIndependentStandaloneArchives(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			payloads := [][]byte{bytes.Repeat([]byte{'A'}, 80), bytes.Repeat([]byte{'B'}, 90)}
			var volumes [][]byte
			for _, payload := range payloads {
				var volume []byte
				if version == 5 {
					volume = rar5OrderFixtureVolume(t, []byte{0}, 0, payload, len(payload), 0, false)
				} else {
					volume = integrityRAR4Volume(payload, uint32(len(payload)), 0)
					main := volume[len(RAR4Signature) : len(RAR4Signature)+13]
					binary.LittleEndian.PutUint16(main[3:], 0)
					binary.LittleEndian.PutUint16(main, uint16(crc32.ChecksumIEEE(main[2:])))
				}
				volumes = append(volumes, volume)
			}
			p := rarGroupDiagnosticParser(t, volumes)
			labels := []string{"First.rar", "Second.rar"}
			for index := range volumes {
				entry, groups, err := p.Parse(t.Context(), "Control.nzb", rarGroupDiagnosticNZBIndices(volumes, labels, []int{index}))
				if err != nil || len(groups) != 1 {
					t.Fatalf("standalone control grouping: groups=%d err=%v", len(groups), err)
				}
				parsed, err := p.Process(t.Context(), entry, groups)
				if err != nil || parsed == nil || len(parsed.Files) != 1 {
					t.Fatalf("standalone control failed: result=%v err=%v", parsed, err)
				}
				assertRAR5OrderedReadback(t, p, &parsed.Files[0], payloads[index])
				t.Logf("independent control %s admitted and passed full/random readback", labels[index])
			}
			entry, groups, err := p.Parse(t.Context(), "Combined.nzb", rarGroupDiagnosticNZBIndices(volumes, labels, []int{0, 1}))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if len(groups) != 2 || err != nil || parsed == nil || len(parsed.Files) != 2 {
				t.Fatalf("independent complete RAR archives combined/rejected: groups=%d err=%v", len(groups), err)
			}
			for index := range parsed.Files {
				file := &parsed.Files[index]
				volumeIndex := 0
				if strings.Contains(file.Segments[0].MessageID, "volume-1@") {
					volumeIndex = 1
				}
				assertRAR5OrderedReadback(t, p, file, payloads[volumeIndex])
			}
		})
	}
}

func rarAssemblyVolume(t *testing.T, version int, encrypted bool, name string, payload []byte, total, index, count int, split uint64, checksum uint32) []byte {
	t.Helper()
	if version == 5 {
		main := []byte{0}
		if count > 1 {
			main[0] = RAR5MainFlagVolume
			if index > 0 {
				main[0] |= RAR5MainFlagVolumeNumber
				main = binary.AppendUvarint(main, uint64(index))
			}
		}
		next := byte(0)
		if index < count-1 {
			next = 1
		}
		return rar5OrderFixtureArchive(t, encrypted,
			rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, main)},
			rar5OrderFixtureFile(name, payload, total, split, nil, checksum),
			rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{next})},
		)
	}
	body := make([]byte, 25+len(name))
	binary.LittleEndian.PutUint32(body, uint32(len(payload)))
	binary.LittleEndian.PutUint32(body[4:], uint32(total))
	binary.LittleEndian.PutUint32(body[9:], checksum)
	body[17], body[18] = 20, RAR4CompressionMethodStore
	binary.LittleEndian.PutUint16(body[19:], uint16(len(name)))
	binary.LittleEndian.PutUint32(body[21:], 32)
	copy(body[25:], name)
	mainFlags := uint16(0)
	if count > 1 {
		mainFlags = 1
	}
	archive := append([]byte(RAR4Signature), integrityRAR4Block(RAR4HeaderTypeArchive, mainFlags, make([]byte, 6))...)
	archive = append(archive, integrityRAR4Block(RAR4HeaderTypeFile, RAR4HeaderFlagLongBlock|uint16(split>>3), body)...)
	archive = append(archive, payload...)
	return append(archive, integrityRAR4Block(RAR4HeaderTypeEnd, 0, nil)...)
}

func rarAssemblyVolumes(t *testing.T, version int, encrypted bool, name string, count int, seed byte) ([][]byte, []byte) {
	t.Helper()
	var payloads [][]byte
	var want []byte
	for index := range count {
		payload := make([]byte, 69+index*3)
		for offset := range payload {
			payload[offset] = seed + byte(index*13+offset*7)
		}
		payloads = append(payloads, payload)
		want = append(want, payload...)
	}
	var volumes [][]byte
	for index, payload := range payloads {
		split := uint64(0)
		if index > 0 {
			split |= RAR5HeaderFlagSplitBefore
		}
		if index < count-1 {
			split |= RAR5HeaderFlagSplitAfter
		}
		checksum := crc32.ChecksumIEEE(payload)
		if index == count-1 {
			checksum = crc32.ChecksumIEEE(want)
		}
		volumes = append(volumes, rarAssemblyVolume(t, version, encrypted, name, payload, len(want), index, count, split, checksum))
	}
	return volumes, want
}

func rarAssemblyFixture(t *testing.T, volumes [][]byte, labels []string, order []int, omit ...func(int, int) bool) (*NZBParser, []byte) {
	t.Helper()
	articles := make(map[string][]byte)
	files := make([]string, len(volumes))
	for index, volume := range volumes {
		chunk := 31 + index%3
		count := (len(volume) + chunk - 1) / chunk
		var xml strings.Builder
		fmt.Fprintf(&xml, `<file poster="fixture" date="1" subject="&quot;%s&quot; yEnc (1/%d)"><groups><group>alt.test</group></groups><segments>`, labels[index], count)
		for part, offset := 1, 0; offset < len(volume); part, offset = part+1, offset+chunk {
			id := fmt.Sprintf("assembly-volume-%d-part-%d@fixture.invalid", index, part)
			article := integrityYEncPart(volume[offset:min(offset+chunk, len(volume))], len(volume), offset, part, count)
			article = bytes.Replace(article, []byte(fmt.Sprintf("name=random-name-%d", part)), []byte("name="+labels[index]), 1)
			if len(omit) == 0 || !omit[0](index, part) {
				articles["<"+id+">"] = article
			}
			fmt.Fprintf(&xml, `<segment number="%d" bytes="%d">%s</segment>`, part, len(article), id)
		}
		xml.WriteString("</segments></file>")
		files[index] = xml.String()
	}
	if order == nil {
		for index := range volumes {
			order = append(order, index)
		}
	}
	var xml strings.Builder
	xml.WriteString("<nzb>")
	for _, index := range order {
		xml.WriteString(files[index])
	}
	xml.WriteString("</nzb>")
	return newContentTestParser(t, articles), []byte(xml.String())
}

func TestRARIndependentNamedArchives(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, encrypted := range []bool{false, true} {
			if version == 4 && encrypted {
				continue
			}
			t.Run(fmt.Sprintf("RAR%d/encrypted=%v", version, encrypted), func(t *testing.T) {
				first, wantFirst := rarAssemblyVolumes(t, version, encrypted, "first.mkv", 1, 'A')
				second, wantSecond := rarAssemblyVolumes(t, version, encrypted, "second.mkv", 1, 'Q')
				p, xml := rarAssemblyFixture(t, append(first, second...), []string{"First.rar", "Second.rar"}, []int{1, 0})
				var logs bytes.Buffer
				p.logger = zerolog.New(zerolog.SyncWriter(&logs))
				entry, groups, err := p.Parse(t.Context(), "Independent.nzb", xml)
				if err != nil || len(groups) != 2 {
					t.Fatalf("standalone grouping: groups=%d err=%v", len(groups), err)
				}
				if encrypted {
					entry.Password = rar5HeaderFixturePassword
				}
				parsed, err := p.Process(t.Context(), entry, groups)
				if err != nil || parsed == nil || len(parsed.Files) != 2 {
					t.Fatalf("standalone admission: result=%v err=%v", parsed, err)
				}
				for index := range parsed.Files {
					file := &parsed.Files[index]
					want := map[string][]byte{"first.mkv": wantFirst, "second.mkv": wantSecond}[file.Name]
					if want == nil || file.InternalPath != file.Name {
						t.Fatalf("unexpected standalone member: %q / %q", file.Name, file.InternalPath)
					}
					assertRAR5OrderedReadback(t, p, file, want)
				}
				if strings.Contains(logs.String(), "volume groups as an archive") {
					t.Fatal("independent successful groups entered assembly fallback")
				}
			})
		}
	}
}

func TestRARObfuscatedMultipartAssembly(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, encrypted := range []bool{false, true} {
			if version == 4 && encrypted {
				continue
			}
			t.Run(fmt.Sprintf("RAR%d/encrypted=%v", version, encrypted), func(t *testing.T) {
				volumes, want := rarAssemblyVolumes(t, version, encrypted, "video.mkv", 4, 'A')
				order := []int{0, 1, 2, 3}
				if version == 5 {
					order = []int{2, 0, 3, 1}
				}
				p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2", "opaque3"}, order)
				var logs bytes.Buffer
				p.logger = zerolog.New(zerolog.SyncWriter(&logs))
				entry, groups, err := p.Parse(t.Context(), "Multipart.nzb", xml)
				if err != nil || len(groups) != 4 {
					t.Fatalf("opaque grouping: groups=%d err=%v", len(groups), err)
				}
				if encrypted {
					entry.Password = rar5HeaderFixturePassword
				}
				parsed, err := p.Process(t.Context(), entry, groups)
				if err != nil || parsed == nil || len(parsed.Files) != 1 {
					t.Fatalf("valid opaque multipart archive failed: result=%v err=%v", parsed, err)
				}
				assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
				for _, group := range groups {
					if len(group.Files) != 1 {
						t.Fatal("fallback mutated original group membership")
					}
				}
				if strings.Count(logs.String(), "Validated incomplete RAR volume groups as an archive") != 1 || strings.Contains(logs.String(), "Failed to process file group") {
					t.Fatalf("unexpected successful fallback diagnostics: %s", logs.String())
				}
			})
		}
	}
}

func TestRARStandaloneArchivesBesideMultipart(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			first, wantFirst := rarAssemblyVolumes(t, version, false, "first.mkv", 1, 'A')
			second, wantSecond := rarAssemblyVolumes(t, version, false, "second.mkv", 1, 'Q')
			multipart, wantMultipart := rarAssemblyVolumes(t, version, false, "multipart.mkv", 4, 'a')
			volumes := append(append(first, second...), multipart...)
			p, xml := rarAssemblyFixture(t, volumes, []string{"First.rar", "Second.rar", "opaque0", "opaque1", "opaque2", "opaque3"}, nil)
			entry, groups, err := p.Parse(t.Context(), "Mixture.nzb", xml)
			if err != nil || len(groups) != 6 {
				t.Fatalf("mixture grouping: groups=%d err=%v", len(groups), err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 3 {
				t.Fatalf("mixture processing: result=%v err=%v", parsed, err)
			}
			for index := range parsed.Files {
				file := &parsed.Files[index]
				want := map[string][]byte{"first.mkv": wantFirst, "second.mkv": wantSecond, "multipart.mkv": wantMultipart}[file.Name]
				if want == nil {
					t.Fatalf("unknown mixture member: %q", file.Name)
				}
				assertRAR5OrderedReadback(t, p, file, want)
			}
		})
	}
}

func TestRARAssemblySeparatesFormats(t *testing.T) {
	four, wantFour := rarAssemblyVolumes(t, 4, false, "four.mkv", 3, 'A')
	five, wantFive := rarAssemblyVolumes(t, 5, true, "five.mkv", 3, 'Q')
	p, xml := rarAssemblyFixture(t, append(four, five...), []string{"four0", "four1", "four2", "five0", "five1", "five2"}, nil)
	entry, groups, err := p.Parse(t.Context(), "Formats.nzb", xml)
	if err != nil || len(groups) != 6 {
		t.Fatalf("format grouping: groups=%d err=%v", len(groups), err)
	}
	entry.Password = rar5HeaderFixturePassword
	parsed, err := p.Process(t.Context(), entry, groups)
	if err != nil || parsed == nil || len(parsed.Files) != 2 {
		t.Fatalf("format separation failed: result=%v err=%v", parsed, err)
	}
	for index := range parsed.Files {
		file := &parsed.Files[index]
		want := map[string][]byte{"four.mkv": wantFour, "five.mkv": wantFive}[file.Name]
		if want == nil {
			t.Fatalf("unexpected format member: %q", file.Name)
		}
		assertRAR5OrderedReadback(t, p, file, want)
	}
}

func TestRARAssemblyRejectsIncompleteOrConflictingSets(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, problem := range []string{"missing first", "missing final", "missing interior", "duplicate first", "duplicate continuation", "conflicting size", "broken split", "corrupt header"} {
			t.Run(fmt.Sprintf("RAR%d/%s", version, problem), func(t *testing.T) {
				volumes, want := rarAssemblyVolumes(t, version, false, "video.mkv", 4, 'A')
				order := []int{0, 1, 2, 3}
				labels := []string{"opaque0", "opaque1", "opaque2", "opaque3"}
				switch problem {
				case "missing first":
					order = []int{1, 2, 3}
				case "missing final":
					order = []int{0, 1, 2}
				case "missing interior":
					order = []int{0, 2, 3}
				case "duplicate first", "duplicate continuation":
					original := 0
					if problem == "duplicate continuation" {
						original = 1
					}
					volumes = append(volumes, bytes.Clone(volumes[original]))
					labels = append(labels, "opaque4")
					order = append(order, 4)
				case "conflicting size", "broken split":
					total, split := len(want), uint64(24)
					if problem == "conflicting size" {
						total++
					} else {
						split = 16
					}
					payload := want[69+72 : 69+72+75]
					volumes[2] = rarAssemblyVolume(t, version, false, "video.mkv", payload, total, 2, 4, split, crc32.ChecksumIEEE(payload))
				case "corrupt header":
					start := len(RAR4Signature)
					if version == 5 {
						start = len(RAR5Signature)
					}
					volumes[2][start] ^= 1
				}
				p, xml := rarAssemblyFixture(t, volumes, labels, order)
				entry, groups, err := p.Parse(t.Context(), "Invalid.nzb", xml)
				if err == nil {
					var parsed *storage.NZB
					parsed, err = p.Process(t.Context(), entry, groups)
					if parsed != nil {
						t.Fatal("invalid archive produced an importable NZB")
					}
				}
				if err == nil {
					t.Fatal("invalid multipart set accepted")
				}
			})
		}
	}
}

func TestRARAssemblyPreservesImportAvailabilityGate(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			volumes, _ := rarAssemblyVolumes(t, version, false, "video.mkv", 4, 'A')
			p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2", "opaque3"}, nil, func(volume, part int) bool { return volume == 2 && part == 3 })
			var logs bytes.Buffer
			p.logger = zerolog.New(zerolog.SyncWriter(&logs))
			config.Get().Usenet.ImportAvailabilitySamplePercent = 100
			entry, groups, err := p.Parse(t.Context(), "Unavailable.nzb", xml)
			if err != nil {
				t.Fatal(err)
			}
			if parsed, err := p.Process(t.Context(), entry, groups); err == nil || parsed != nil || !strings.Contains(err.Error(), "import body sample") {
				t.Fatalf("missing payload article did not fail the import gate: result=%v err=%v", parsed, err)
			}
			if !strings.Contains(logs.String(), "Validated incomplete RAR volume groups as an archive") {
				t.Fatal("availability test never reached successful archive assembly")
			}
		})
	}
}

func TestRARAssemblyPreservesAdditionalCompleteMembers(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			volumes, want := rarAssemblyVolumes(t, version, false, "video.mkv", 4, 'A')
			sidecar := []byte("sidecar fixture")
			if version == 4 {
				standalone := rarAssemblyVolume(t, version, false, "sidecar.mkv", sidecar, len(sidecar), 0, 1, 0, crc32.ChecksumIEEE(sidecar))
				member := standalone[20 : len(standalone)-7]
				first := append(bytes.Clone(volumes[0][:20]), member...)
				volumes[0] = append(first, volumes[0][20:]...)
			} else {
				payload := want[:69]
				volumes[0] = rar5OrderFixtureArchive(t, false,
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, []byte{1})},
					rar5OrderFixtureFile("sidecar.mkv", sidecar, len(sidecar), 0, nil),
					rar5OrderFixtureFile("video.mkv", payload, len(want), 16, nil),
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{1})},
				)
			}
			p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2", "opaque3"}, nil)
			entry, groups, err := p.Parse(t.Context(), "Members.nzb", xml)
			if err != nil || len(groups) != 4 {
				t.Fatalf("member grouping: groups=%d err=%v", len(groups), err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 2 {
				t.Fatalf("multipart member lost: result=%v err=%v", parsed, err)
			}
			for index := range parsed.Files {
				file := &parsed.Files[index]
				expected := map[string][]byte{"video.mkv": want, "sidecar.mkv": sidecar}[file.Name]
				if expected == nil {
					t.Fatalf("unexpected member: %q", file.Name)
				}
				assertRAR5OrderedReadback(t, p, file, expected)
			}
		})
	}
}

func TestRARAssemblyDoesNotRetryPasswordFailures(t *testing.T) {
	for _, password := range []string{"", "incorrect fixture password"} {
		t.Run(fmt.Sprintf("missing=%v", password == ""), func(t *testing.T) {
			volumes, _ := rarAssemblyVolumes(t, 5, true, "video.mkv", 3, 'A')
			p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2"}, nil)
			var logs bytes.Buffer
			p.logger = zerolog.New(zerolog.SyncWriter(&logs))
			entry, groups, err := p.Parse(t.Context(), "Password.nzb", xml)
			if err != nil {
				t.Fatal(err)
			}
			entry.Password = password
			if parsed, err := p.Process(t.Context(), entry, groups); err == nil || parsed != nil {
				t.Fatal("encrypted headers admitted without the correct password")
			}
			if strings.Contains(logs.String(), "RAR volume assembly") || strings.Contains(logs.String(), "volume groups as an archive") {
				t.Fatal("password errors entered the multipart fallback")
			}
		})
	}
}

func TestRARAssemblyPreservesMetadataOnlyVolumes(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			_, want := rarAssemblyVolumes(t, 5, encrypted, "video.mkv", 3, 'A')
			empty := func(main []byte, next byte) []byte {
				return rar5OrderFixtureArchive(t, encrypted,
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeMain, 0, 0, main)},
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{next})},
				)
			}
			volumes := [][]byte{empty([]byte{1}, 1)}
			at := 0
			for index, length := range []int{69, 72, 75} {
				payload := want[at : at+length]
				flags := []uint64{16, 24, 8}[index]
				checksum := crc32.ChecksumIEEE(payload)
				if index == 2 {
					checksum = crc32.ChecksumIEEE(want)
				}
				volumes = append(volumes, rarAssemblyVolume(t, 5, encrypted, "video.mkv", payload, len(want), index+1, 5, flags, checksum))
				at += length
			}
			volumes = append(volumes, empty([]byte{3, 4}, 0))
			p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2", "opaque3", "opaque4"}, []int{4, 2, 0, 3, 1})
			entry, groups, err := p.Parse(t.Context(), "Metadata.nzb", xml)
			if err != nil || len(groups) != 5 {
				t.Fatalf("metadata grouping: groups=%d err=%v", len(groups), err)
			}
			if encrypted {
				entry.Password = rar5HeaderFixturePassword
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("metadata-only volume lost: result=%v err=%v", parsed, err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
		})
	}
}

func TestRARAssemblyRejectsAmbiguousLegacyOrder(t *testing.T) {
	volumes, _ := rarAssemblyVolumes(t, 4, false, "video.mkv", 4, 'A')
	p, xml := rarAssemblyFixture(t, volumes, []string{"opaque0", "opaque1", "opaque2", "opaque3"}, nil)
	entry, groups, err := p.Parse(t.Context(), "Ambiguous.nzb", xml)
	if err != nil {
		t.Fatal(err)
	}
	groups["opaque1"].Files[0].Number = 2
	groups["opaque2"].Files[0].Number = 2
	parsed, err := p.Process(t.Context(), entry, groups)
	if err == nil || parsed != nil || !strings.Contains(err.Error(), "ambiguous RAR4 volume order") {
		t.Fatalf("ambiguous legacy order was guessed: result=%v err=%v", parsed, err)
	}
}

func TestRARAssemblyExcludesUnrelatedErrors(t *testing.T) {
	for _, reason := range []string{"header CRC mismatch", "password incorrect", "first article: provider unavailable", "invalid yEnc geometry", "incomplete RAR5 volume sequence at index 0"} {
		t.Run(reason, func(t *testing.T) {
			groups := []FileGroup{{Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{{Filename: "one"}}}, {Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{{Filename: "two"}}}}
			first := errors.New(reason)
			second := fmt.Errorf("wrapped: %w", first)
			results := []fileGroupResult{{err: first}, {err: second}}
			// A nil provider client ensures these untyped failures cannot read
			// articles or enter the fallback even when their text resembles it.
			p := NewParser(nil, 2, zerolog.Nop())
			if errs := p.assembleIncompleteRARGroups(t.Context(), groups, results, ""); len(errs) != 0 || results[0].err != first || results[1].err != second {
				t.Fatal("unrelated failures were retried or changed")
			}
		})
	}
	if got := incompleteRARVersion(&rarDiagnosticCycle{}); got != RARVersionUnknown {
		t.Fatal("cyclic error supplied incomplete-volume evidence")
	}
}

func TestRARAssemblyPreservesCancellationAndOriginalCauses(t *testing.T) {
	groups := []FileGroup{{Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{{Filename: "one"}}}, {Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{{Filename: "two"}}}}
	first, second := errors.New("first incomplete"), errors.New("second incomplete")
	results := []fileGroupResult{{err: &rarIncompleteVolumeError{cause: first, version: RARVersion5}}, {err: &rarIncompleteVolumeError{cause: second, version: RARVersion5}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	errs := NewParser(nil, 2, zerolog.Nop()).assembleIncompleteRARGroups(ctx, groups, results, "")
	joined := errors.Join(append(errs, results[0].err, results[1].err)...)
	if !errors.Is(joined, context.Canceled) || !errors.Is(joined, first) || !errors.Is(joined, second) || results[0].err.Error() != first.Error() || results[1].err.Error() != second.Error() {
		t.Fatal("cancellation or original incomplete-volume error was lost")
	}
}

func TestRARAssemblyOwnsRetryGeometry(t *testing.T) {
	groups := []FileGroup{
		{BaseName: "one", Type: storage.NZBFileTypeRar, Groups: map[string]struct{}{"alt.one": {}}, Files: []nzbparser.NzbFile{{Filename: "opaque0", Number: 1, Segments: nzbparser.NzbSegments{{Number: 1, Id: "first"}}}}},
		{BaseName: "two", Type: storage.NZBFileTypeRar, Groups: map[string]struct{}{"alt.two": {}}, Files: []nzbparser.NzbFile{{Filename: "opaque1", Number: 2, Segments: nzbparser.NzbSegments{{Number: 1, Id: "second"}}}}},
	}
	before, _ := json.Marshal(groups)
	merged, err := incompleteRARGroup(groups, []int{0, 1}, RARVersion4)
	if err != nil || len(merged.Groups) != 2 || merged.fileMeta != nil || merged.metadata != nil {
		t.Fatalf("retry inherited incomplete geometry: %v", err)
	}
	merged.Files[0].Segments[0].Id = "changed"
	merged.Files[0].Filename = "changed"
	delete(merged.Groups, "alt.one")
	after, _ := json.Marshal(groups)
	if !bytes.Equal(before, after) {
		t.Fatal("retry modified independent group state")
	}
}
