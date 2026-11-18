package parser

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/types"
	"github.com/rs/zerolog"
)

func TestRAR5VolumeMetadataExtraAreaBoundary(t *testing.T) {
	extra := []byte{2, 99, 1} // a well-sized unknown extra record
	tests := []struct {
		name      string
		main      []byte
		extra     []byte
		extraSize uint64
		want      uint64
		wantError string
	}{
		{"implicit first", []byte{1}, nil, 0, 0, ""},
		{"multibyte continuation", binary.AppendUvarint([]byte{3}, 300), extra, uint64(len(extra)), 300, ""},
		{"largest number decoded without allocation", binary.AppendUvarint([]byte{3}, math.MaxUint64), nil, 0, math.MaxUint64, ""},
		{"extra cannot supply number", []byte{3}, extra, uint64(len(extra)), 0, "read RAR5 volume number"},
		{"extra cannot supply flags", nil, extra, uint64(len(extra)), 0, "read RAR5 archive flags"},
		{"extra exceeds body", []byte{1}, extra, 100, 0, "extra area exceeds header"},
		{"unflagged number", []byte{1, 2}, nil, 0, 0, "unexpected RAR5 main header fields"},
	}
	for _, encrypted := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("encrypted=%v/%s", encrypted, tc.name), func(t *testing.T) {
				flags := uint64(0)
				if tc.extraSize > 0 {
					flags = RAR5HeaderFlagExtraArea
				}
				body := binary.AppendUvarint(nil, RAR5HeaderTypeMain)
				body = binary.AppendUvarint(body, flags)
				if tc.extraSize > 0 {
					body = binary.AppendUvarint(body, tc.extraSize)
				}
				body = append(body, tc.main...)
				body = append(body, tc.extra...)
				archive := rar5OrderFixtureArchive(t, encrypted,
					rar5OrderFixtureBlock{header: checkedRAR5HeaderFixture(body, nil)},
					rar5OrderFixtureFile("video.mkv", []byte("data"), 4, 0, nil),
					rar5OrderFixtureBlock{header: integrityRAR5Block(RAR5HeaderTypeEndOfArc, 0, 0, []byte{0})},
				)
				stream := memoryRAR5HeaderStream(archive)
				if err := stream.Skip(int64(len(RAR5Signature))); err != nil {
					t.Fatal(err)
				}
				result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", rar5HeaderFixturePassword)
				if tc.wantError != "" {
					if err == nil || result != nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("metadata error=%v, want %q", err, tc.wantError)
					}
					return
				}
				if err != nil || result.volumeInfo == nil || result.volumeInfo.number != tc.want {
					t.Fatalf("decoded metadata=%+v, error=%v, want index=%d", result, err, tc.want)
				}
			})
		}
	}
}

func TestRAR5VolumeOrderUsesOnlyCompleteUniqueSequence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		infos []*rar5VolumeInfo
		want  []int
	}{
		{"empty", nil, nil},
		{"missing metadata", []*rar5VolumeInfo{nil}, nil},
		{"single archive", []*rar5VolumeInfo{{}}, []int{0}},
		{"single volume", []*rar5VolumeInfo{{multi: true}}, []int{0}},
		{"scrambled", []*rar5VolumeInfo{{number: 2, multi: true}, {number: 0, multi: true, next: true}, {number: 1, multi: true, next: true}}, []int{1, 2, 0}},
		{"missing prefix", []*rar5VolumeInfo{{number: 1, multi: true}}, nil},
		{"duplicate", []*rar5VolumeInfo{{multi: true, next: true}, {multi: true, next: true}}, nil},
		{"single archive claims continuation", []*rar5VolumeInfo{{next: true}}, nil},
		{"unbounded index", []*rar5VolumeInfo{{number: math.MaxUint64, multi: true}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rar5VolumeOrder(tc.infos)
			if tc.want == nil {
				if err == nil || got != nil {
					t.Fatalf("invalid volume sequence accepted: %v %v", got, err)
				}
				return
			}
			if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("order=%v error=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestRARBaseSegmentsRejectsInvalidMapping(t *testing.T) {
	valid := func() *types.Volume { return &types.Volume{Size: 4, Segments: []storage.NZBSegment{{Bytes: 4}}} }
	for _, tc := range []struct {
		name    string
		volumes []*types.Volume
		order   []int
	}{
		{"missing mapping", []*types.Volume{valid()}, nil},
		{"negative position", []*types.Volume{valid()}, []int{-1}},
		{"out of range", []*types.Volume{valid()}, []int{1}},
		{"duplicate position", []*types.Volume{valid(), valid()}, []int{0, 0}},
		{"nil descriptor", []*types.Volume{nil}, []int{0}},
		{"zero size", []*types.Volume{{}}, []int{0}},
		{"missing bytes", []*types.Volume{{Size: 4}}, []int{0}},
		{"short segment", []*types.Volume{{Size: 4, Segments: []storage.NZBSegment{{Bytes: 3}}}}, []int{0}},
		{"long segment", []*types.Volume{{Size: 4, Segments: []storage.NZBSegment{{Bytes: 5}}}}, []int{0}},
		{"negative segment", []*types.Volume{{Size: 4, Segments: []storage.NZBSegment{{Bytes: -1}}}}, []int{0}},
		{"zero segment", []*types.Volume{{Size: 4, Segments: []storage.NZBSegment{{Bytes: 0}}}}, []int{0}},
		{"total size overflow", []*types.Volume{{Size: math.MaxInt64, Segments: []storage.NZBSegment{{Bytes: math.MaxInt64}}}, {Size: 1, Segments: []storage.NZBSegment{{Bytes: 1}}}}, []int{0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, infos, err := buildRARBaseSegments(tc.volumes, tc.order)
			if err == nil || base != nil || infos != nil {
				t.Fatalf("invalid geometry accepted: %v", err)
			}
		})
	}
}

func TestRAR5VolumeMetadataRetainsHeaderCRCRequirement(t *testing.T) {
	main := checkedRAR5HeaderFixture([]byte{RAR5HeaderTypeMain, RAR5HeaderFlagExtraArea, 3, 3, 1, 2, 99, 1}, nil)
	// Alter the index without updating its CRC. Neither path may expose it as
	// trusted metadata even though all structural fields are still parseable.
	main[len(main)-4] = 2
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			archive := rar5OrderFixtureArchive(t, encrypted, rar5OrderFixtureBlock{header: main})
			stream := memoryRAR5HeaderStream(archive)
			if err := stream.Skip(int64(len(RAR5Signature))); err != nil {
				t.Fatal(err)
			}
			result, err := NewRARParser(nil, 1, zerolog.Nop()).parseRAR5Stream(stream, 0, "fixture.rar", rar5HeaderFixturePassword)
			if err == nil || result != nil || !strings.Contains(err.Error(), "CRC mismatch") {
				t.Fatalf("trusted modified index: %+v %v", result, err)
			}
		})
	}
}
