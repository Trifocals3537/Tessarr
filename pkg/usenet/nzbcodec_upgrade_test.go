package usenet

import (
	"encoding/hex"
	"reflect"
	"testing"
)

func TestNZBMetadataReadsPreSecurityUpgradeFrames(t *testing.T) {
	// Frozen with the pre-upgrade v1.18.7 zstd encoder, using the unchanged
	// sourceCodecFixture. Generating frames with the current encoder would not
	// exercise reading metadata already saved by deployed Tessarr versions.
	for _, tc := range []struct {
		name, data string
	}{
		{"geometry", "b13e28b52ffd04008d0100f4010766697874757265000000ffdb8ff9ce030109766964656f2e6d6b7600100206004aedefb9456e824c0d58ca1951b88ac3b01a3428b52ffd04003901000108616c742e74657374000208080008060e04000000b18ecd02020018000c020401180c0c0404ad4e783c28b52ffd0400e500004401116140666978747572652e696e76616c69641162015412040d2a7344517d"},
		{"legacy", "b13e28b52ffd04008d0100f4010766697874757265000000ffdb8ff9ce030109766964656f2e6d6b7600100206004aedefb9456e824c0d58ca1951b88ac3b01a2328b52ffd0400b100000108616c742e74657374000208080008060e040000000e49bddd28b52ffd0400e500004401116140666978747572652e696e76616c69641162015412040d2a7344517d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := hex.DecodeString(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			want := sourceCodecFixture()
			if tc.name == "legacy" {
				for i := range want.Files[0].Segments {
					want.Files[0].Segments[i].Source = nil
				}
			}
			got, err := decodeNZB(data)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != want.ID || got.Name != want.Name || len(got.Files) != 1 ||
				got.Files[0].Name != want.Files[0].Name || got.Files[0].Size != want.Files[0].Size ||
				!reflect.DeepEqual(got.Files[0].Segments, want.Files[0].Segments) {
				t.Fatalf("saved metadata changed after compression upgrade: %+v", got)
			}
			header, err := decodeNZBV2Header(data)
			if err != nil || header.ID != want.ID || header.Name != want.Name || len(header.Files) != 1 ||
				header.Files[0].Name != want.Files[0].Name || header.Files[0].Size != want.Files[0].Size || len(header.Files[0].Segments) != 0 {
				t.Fatalf("saved metadata header changed: %+v %v", header, err)
			}
			ids, count, err := decodeFileMessageIDsSampled(data, "video.mkv", 100)
			if err != nil || count != 2 || !reflect.DeepEqual(ids, []string{"a@fixture.invalid", "b@fixture.invalid"}) {
				t.Fatalf("saved message IDs changed: %v %d %v", ids, count, err)
			}
		})
	}
}
