package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/rs/zerolog"
)

const rarGroupDiagnosticMessage = "possible RAR chain split across filename groups"

// These are valid synthetic archives. Only the labels divide them into groups;
// the first volume is deliberately last in NZB document/file-number order.
func TestRARGroupDiagnosticThroughNZB(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, names := range [][2]string{{"Demo-Release", "Demo - Release"}, {"Archive.Name", "Archive Name"}} {
			for _, encrypted := range []bool{false, true} {
				if version == 4 && encrypted {
					continue
				}
				t.Run(fmt.Sprintf("RAR%d/%s/encrypted=%v", version, names[0], encrypted), func(t *testing.T) {
					volumes, want := rarGroupDiagnosticVolumes(t, version, encrypted, 15, 'A')
					p := rarGroupDiagnosticParser(t, volumes)
					password := ""
					if encrypted {
						password = rar5HeaderFixturePassword
					}
					controlLabels := make([]string, len(volumes))
					labels := make([]string, len(volumes))
					for index := range volumes {
						controlLabels[index] = fmt.Sprintf("Control.part%02d.rar", index+1)
						labels[index] = fmt.Sprintf("%s.r%02d", names[1], index-1)
					}
					labels[0] = names[0] + ".part01.rar"
					control, groups, err := p.Parse(t.Context(), "Control.nzb", rarGroupDiagnosticNZB(volumes, controlLabels))
					if err != nil || len(groups) != 1 {
						t.Fatalf("control grouping: groups=%d err=%v", len(groups), err)
					}
					control.Password = password
					parsed, err := p.Process(t.Context(), control, groups)
					if err != nil || parsed == nil || len(parsed.Files) != 1 {
						t.Fatalf("valid control rejected: parsed=%v err=%v", parsed, err)
					}
					assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)

					var logs bytes.Buffer
					p.logger = zerolog.New(zerolog.SyncWriter(&logs))
					entry, groups, err := p.Parse(t.Context(), "Variant.nzb", rarGroupDiagnosticNZB(volumes, labels))
					if err != nil || len(groups) != 2 {
						t.Fatalf("variant grouping: groups=%d err=%v", len(groups), err)
					}
					if len(groups[names[0]].Files) != 1 || len(groups[names[1]].Files) != 14 {
						t.Fatalf("unexpected group partition: %+v", groups)
					}
					entry.Password = password
					parsed, err = p.Process(t.Context(), entry, groups)
					if err == nil || parsed != nil {
						t.Fatalf("separate incomplete groups accepted: parsed=%v err=%v", parsed, err)
					}
					if !strings.Contains(err.Error(), rarGroupDiagnosticMessage) || !strings.Contains(logs.String(), rarGroupDiagnosticMessage) {
						t.Fatalf("missing cross-group diagnostic: err=%v logs=%s", err, logs.String())
					}
					for _, phrase := range []string{"1 volume", "14 volumes", ".partNN.rar", ".rNN", "unverified", "kept separate"} {
						if !strings.Contains(err.Error(), phrase) {
							t.Errorf("diagnostic missing %q: %v", phrase, err)
						}
					}
					for _, original := range map[int][]string{4: {"first part is a continuation", "missing final volume"}, 5: {"terminal marker"}}[version] {
						if !strings.Contains(err.Error(), original) {
							t.Errorf("original error lost: %q in %v", original, err)
						}
					}
					seen := 0
					for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
						var record map[string]any
						if err := json.Unmarshal([]byte(line), &record); err != nil {
							t.Fatal(err)
						}
						message, _ := record["message"].(string)
						if strings.HasPrefix(message, rarGroupDiagnosticMessage) {
							seen++
							for _, private := range []string{names[0], names[1], "video.mkv", "fixture.invalid", "password"} {
								if strings.Contains(line, private) {
									t.Errorf("new diagnostic exposed %q: %s", private, line)
								}
							}
						} else if message == "Failed to process file group" && record["group_index"] == nil {
							t.Error("original warning lacks diagnostic group index")
						}
					}
					if seen != 1 {
						t.Errorf("diagnostic logged %d times, want once", seen)
					}
				})
			}
		}
	}
}

func rarGroupDiagnosticVolumes(t *testing.T, version int, encrypted bool, count int, seed byte) ([][]byte, []byte) {
	t.Helper()
	var payloads [][]byte
	var want []byte
	for index := range count {
		payload := bytes.Repeat([]byte{seed + byte(index)}, 60+index)
		payloads = append(payloads, payload)
		want = append(want, payload...)
	}
	var volumes [][]byte
	for index, payload := range payloads {
		split, next := uint64(0), uint64(0)
		if index > 0 {
			split |= RAR5HeaderFlagSplitBefore
		}
		if index < count-1 {
			split |= RAR5HeaderFlagSplitAfter
			next = 1
		}
		var volume []byte
		if version == 4 {
			volume = integrityRAR4Volume(payload, uint32(len(want)), uint16(split>>3))
		} else {
			main := []byte{RAR5MainFlagVolume}
			if index > 0 {
				main = []byte{RAR5MainFlagVolume | RAR5MainFlagVolumeNumber, byte(index)}
			}
			checksum := crc32.ChecksumIEEE(payload)
			if index == count-1 {
				checksum = crc32.ChecksumIEEE(want)
			}
			volume = rar5OrderFixtureVolume(t, main, next, payload, len(want), split, encrypted, checksum)
		}
		volumes = append(volumes, volume)
	}
	return volumes, want
}

func rarGroupDiagnosticParser(t *testing.T, volumes [][]byte) *NZBParser {
	t.Helper()
	articles := make(map[string][]byte)
	for index, volume := range volumes {
		id := fmt.Sprintf("<diagnostic-volume-%d@fixture.invalid>", index)
		articles[id] = encodeContentTestArticle(volume, "fixture-volume.rar")
	}
	return newContentTestParser(t, articles)
}

func rarGroupDiagnosticNZB(volumes [][]byte, labels []string) []byte {
	indices := make([]int, len(volumes))
	for index := range indices {
		indices[index] = (index + 1) % len(volumes)
	}
	return rarGroupDiagnosticNZBIndices(volumes, labels, indices)
}

func rarGroupDiagnosticNZBIndices(volumes [][]byte, labels []string, indices []int) []byte {
	var xml strings.Builder
	xml.WriteString("<nzb>")
	for _, index := range indices {
		fmt.Fprintf(&xml, `<file poster="fixture" date="1" subject="&quot;%s&quot; yEnc (1/1)"><groups><group>alt.test</group></groups><segments><segment number="1" bytes="%d">diagnostic-volume-%d@fixture.invalid</segment></segments></file>`, labels[index], len(volumes[index]), index)
	}
	xml.WriteString("</nzb>")
	return []byte(xml.String())
}

func TestRARGroupDiagnosticKeepsUnrelatedArchivesSeparate(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			first, wantFirst := rarGroupDiagnosticVolumes(t, version, false, 2, 'A')
			second, wantSecond := rarGroupDiagnosticVolumes(t, version, false, 2, 'Q')
			// Valid independent sets may have identical internal member names and
			// sizes. Neither similar basenames nor volume indices prove identity.
			volumes := append(first, second...)
			p := rarGroupDiagnosticParser(t, volumes)
			var logs bytes.Buffer
			p.logger = zerolog.New(zerolog.SyncWriter(&logs))
			entry, groups, err := p.Parse(t.Context(), "Independent.nzb", rarGroupDiagnosticNZB(volumes, []string{"Separate.part01.rar", "Separate.part02.rar", "Sep arate.part01.rar", "Sep arate.part02.rar"}))
			if err != nil || len(groups) != 2 {
				t.Fatalf("independent groups combined: groups=%d err=%v", len(groups), err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 2 {
				t.Fatalf("independent archives rejected or combined: parsed=%v err=%v", parsed, err)
			}
			for index := range parsed.Files {
				file := &parsed.Files[index]
				want := wantSecond
				if strings.Contains(file.Segments[0].MessageID, "volume-0@") {
					want = wantFirst
				}
				assertRAR5OrderedReadback(t, p, file, want)
			}
			if strings.Contains(logs.String(), rarGroupDiagnosticMessage) {
				t.Fatal("complete archives reported as a split chain")
			}
		})
	}
}

func TestRARGroupDiagnosticInvalidSetsStillReject(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, problem := range []string{"missing first", "missing final", "missing interior", "duplicate volume", "corrupt header", "conflicting size"} {
			t.Run(fmt.Sprintf("RAR%d/%s", version, problem), func(t *testing.T) {
				volumes, _ := rarGroupDiagnosticVolumes(t, version, false, 15, 'A')
				indices := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0}
				switch problem {
				case "missing first":
					indices = indices[:14]
				case "missing final":
					indices = append(indices[:13], 0)
				case "missing interior":
					indices = append(indices[:5], indices[6:]...)
				case "duplicate volume":
					indices = append(indices, 7)
				case "corrupt header":
					volumes[0][len(RAR4Signature)+1] ^= 1
				case "conflicting size":
					if version == 4 {
						volumes[7] = integrityRAR4Volume(bytes.Repeat([]byte{'H'}, 67), 1006, 3)
					} else {
						volumes[7] = rar5OrderFixtureVolume(t, []byte{3, 7}, 1, bytes.Repeat([]byte{'H'}, 67), 1006, 24, false)
					}
				}
				labels := make([]string, len(volumes))
				for index := range labels {
					labels[index] = fmt.Sprintf("Demo - Release.r%02d", index-1)
				}
				labels[0] = "Demo-Release.part01.rar"
				p := rarGroupDiagnosticParser(t, volumes)
				entry, groups, err := p.Parse(t.Context(), "Invalid.nzb", rarGroupDiagnosticNZBIndices(volumes, labels, indices))
				if err == nil {
					var parsed *storage.NZB
					parsed, err = p.Process(t.Context(), entry, groups)
					if parsed != nil {
						t.Fatal("invalid archive produced an importable NZB")
					}
				}
				if err == nil {
					t.Fatal("incomplete/ambiguous archive accepted")
				}
			})
		}
	}
}

func TestRARGroupDiagnosticDoesNotBlockOtherValidGroup(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			incomplete, _ := rarGroupDiagnosticVolumes(t, version, false, 3, 'A')
			complete, want := rarGroupDiagnosticVolumes(t, version, false, 2, 'Q')
			volumes := append(incomplete, complete...)
			p := rarGroupDiagnosticParser(t, volumes)
			var logs bytes.Buffer
			p.logger = zerolog.New(zerolog.SyncWriter(&logs))
			labels := []string{"Demo-Release.part01.rar", "Demo - Release.r00", "Demo - Release.r01", "Unrelated.part01.rar", "Unrelated.part02.rar"}
			entry, groups, err := p.Parse(t.Context(), "Mixed.nzb", rarGroupDiagnosticNZB(volumes, labels))
			if err != nil || len(groups) != 3 {
				t.Fatalf("mixed NZB grouping: groups=%d err=%v", len(groups), err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("diagnostic changed partial-success policy: parsed=%v err=%v", parsed, err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
			if !strings.Contains(logs.String(), rarGroupDiagnosticMessage) {
				t.Fatal("partial success lost rejected-group diagnostic")
			}
		})
	}
}

func rarDiagnosticTestPair() []rarGroupFailure {
	var failures []rarGroupFailure
	for index, base := range []string{"Private-Demo", "Private - Demo"} {
		boundary := newRARBoundaryError(errors.New("original incomplete archive"), &RARFileEntry{Name: "private/member.mkv", UncompressedSize: 1000, IsStored: true}, index == 1)
		boundary.version = RARVersion4
		filename := base + ".part01.rar"
		if index == 1 {
			filename = base + ".r00"
		}
		group := &FileGroup{BaseName: base, Type: storage.NZBFileTypeRar, Files: []nzbparser.NzbFile{{Filename: filename}}}
		failures = append(failures, rarGroupFailure{index + 1, group, fmt.Errorf("wrapped: %w", boundary)})
	}
	return failures
}

func TestRARGroupDiagnosticRequiresComplementaryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]rarGroupFailure)
	}{
		{"same boundary", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).missingFirst = false }},
		{"different member", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.name = "other.mkv" }},
		{"different size", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.size++ }},
		{"different method", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.stored = false }},
		{"different encryption", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.encrypted = true }},
		{"different format", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).version = RARVersion5 }},
		{"unknown format", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).version = RARVersionUnknown }},
		{"different basename", func(f []rarGroupFailure) { f[1].group.BaseName = "Unrelated" }},
		{"identical basename", func(f []rarGroupFailure) { f[1].group.BaseName = f[0].group.BaseName }},
		{"empty basename", func(f []rarGroupFailure) { f[1].group.BaseName = " ._- " }},
		{"oversized basename", func(f []rarGroupFailure) { f[1].group.BaseName = strings.Repeat("x", maxRARDiagnosticName+1) }},
		{"oversized filename", func(f []rarGroupFailure) { f[1].group.Files[0].Filename = strings.Repeat("x", maxRARDiagnosticName+1) }},
		{"oversized member", func(f []rarGroupFailure) {
			rarBoundaryFailure(f[1].err).member.name = strings.Repeat("x", maxRARDiagnosticName+1)
		}},
		{"empty member", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.name = "" }},
		{"zero size", func(f []rarGroupFailure) { rarBoundaryFailure(f[1].err).member.size = 0 }},
		{"text only", func(f []rarGroupFailure) { f[1].err = errors.New(f[1].err.Error()) }},
		{"cyclic cause", func(f []rarGroupFailure) { f[1].err = &rarDiagnosticCycle{} }},
		{"success", func(f []rarGroupFailure) { f[1].err = nil }},
		{"nil group", func(f []rarGroupFailure) { f[1].group = nil }},
		{"other archive", func(f []rarGroupFailure) { f[1].group.Type = storage.NZBFileTypeSevenZip }},
		{"empty group", func(f []rarGroupFailure) { f[1].group.Files = nil }},
		{"oversized group", func(f []rarGroupFailure) { f[1].group.Files = make([]nzbparser.NzbFile, maxRARDiagnosticParts+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := rarDiagnosticTestPair()
			tc.change(failures)
			if hints := rarGroupDiagnostics(failures); len(hints) != 0 {
				t.Fatalf("unsupported diagnostic: %v", hints)
			}
		})
	}
}

type rarDiagnosticCycle struct{}

func (*rarDiagnosticCycle) Error() string   { return "cycle" }
func (e *rarDiagnosticCycle) Unwrap() error { return e }

func TestRARGroupDiagnosticBoundsAndErrorPreservation(t *testing.T) {
	failures := rarDiagnosticTestPair()
	boundary := rarBoundaryFailure(failures[0].err)
	if !errors.Is(failures[0].err, boundary.cause) || boundary.Error() != boundary.cause.Error() {
		t.Fatal("original error text/cause not preserved")
	}
	hints := rarGroupDiagnostics(failures)
	if len(hints) != 1 || !errors.Is(errors.Join(failures[0].err, failures[1].err, hints[0]), boundary.cause) {
		t.Fatalf("diagnostic lost original cause: %v", hints)
	}
	for _, private := range []string{"Private", "private/member.mkv", "original incomplete archive"} {
		if strings.Contains(hints[0].Error(), private) {
			t.Errorf("hint copied private evidence %q", private)
		}
	}
	var many []rarGroupFailure
	for index := range maxRARDiagnosticGroups + 1 {
		failure := rarDiagnosticTestPair()[0]
		failure.index = index + 1
		failure.group.BaseName = "Private" + strings.Repeat(" ", index) + "Demo"
		rarBoundaryFailure(failure.err).missingFirst = index%2 == 1
		many = append(many, failure)
	}
	if hints := rarGroupDiagnostics(many); len(hints) != maxRARGroupDiagnostics {
		t.Fatalf("diagnostic count not capped: %d", len(hints))
	} else {
		for _, hint := range hints {
			if len(hint.Error()) > 512 {
				t.Fatalf("unbounded diagnostic: %d bytes", len(hint.Error()))
			}
		}
	}
	for index := range many {
		rarBoundaryFailure(many[index].err).missingFirst = index == len(many)-1
	}
	if hints := rarGroupDiagnostics(many); len(hints) != 0 {
		t.Fatal("inspected candidates beyond bounded group limit")
	}
}

func TestRAR5GroupBoundaryEvidenceRejectsConflicts(t *testing.T) {
	for _, tc := range []string{"control", "prefix", "scrambled", "duplicate", "gap", "premature end", "missing both", "not multivolume", "nil metadata", "inconsistent size", "inconsistent method", "inconsistent encryption", "broken split", "duplicate member", "overflow", "oversized index set", "oversized member set"} {
		t.Run(tc, func(t *testing.T) {
			infos := []*rar5VolumeInfo{{number: 1, multi: true, next: true}, {number: 2, multi: true}}
			members := [][]*RARFileEntry{
				{{Name: "video.mkv", UncompressedSize: 300, PackedSize: 100, IsStored: true, SplitBefore: true, SplitAfter: true}},
				{{Name: "video.mkv", UncompressedSize: 300, PackedSize: 100, IsStored: true, SplitBefore: true}},
			}
			switch tc {
			case "prefix":
				infos[0].number, infos[1].number = 0, 1
				infos[1].next = true
				members[0][0].SplitBefore = false
				members[1][0].SplitAfter = true
			case "scrambled":
				infos[0], infos[1] = infos[1], infos[0]
				members[0], members[1] = members[1], members[0]
			case "duplicate":
				infos[1].number = 1
			case "gap":
				infos[1].number = 3
			case "premature end":
				infos[0].next = false
			case "missing both":
				infos[1].next = true
			case "not multivolume":
				infos[0].multi = false
			case "nil metadata":
				infos[0] = nil
			case "inconsistent size":
				members[1][0].UncompressedSize++
			case "inconsistent method":
				members[1][0].IsStored = false
			case "inconsistent encryption":
				members[1][0].IsEncrypted = true
			case "broken split":
				members[1][0].SplitBefore = false
			case "duplicate member":
				members[1] = append(members[1], members[1][0])
			case "overflow":
				members[0][0].PackedSize = math.MaxInt64
			case "oversized index set":
				infos = make([]*rar5VolumeInfo, maxRARDiagnosticParts+1)
			case "oversized member set":
				members[0] = make([]*RARFileEntry, maxRARDiagnosticParts+1)
			}
			beforeInfos := make([]*rar5VolumeInfo, len(infos))
			for index, info := range infos {
				if info != nil {
					copied := *info
					beforeInfos[index] = &copied
				}
			}
			beforeMembers, _ := json.Marshal(members)
			original := errors.New("original RAR5 sequence error")
			got := rar5GroupBoundaryFailure(original, infos, members)
			if got.Error() != original.Error() || !errors.Is(got, original) {
				t.Fatal("RAR5 boundary annotation changed original error")
			}
			if (rarBoundaryFailure(got) != nil) != (tc == "control" || tc == "prefix" || tc == "scrambled") {
				t.Fatalf("invalid evidence decision: %T %v", got, got)
			}
			afterMembers, _ := json.Marshal(members)
			if !reflect.DeepEqual(beforeInfos, infos) || !bytes.Equal(beforeMembers, afterMembers) {
				t.Fatal("diagnostic changed archive metadata")
			}
		})
	}
}
