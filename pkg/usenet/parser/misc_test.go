package parser

import "testing"

func TestDetermineNZBNameUsesFirstPortableCandidate(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		meta     map[string]string
		want     string
	}{
		{name: "filename", filename: "release.nzb", meta: map[string]string{"Name": "meta"}, want: "release"},
		{name: "dotted NZB filename", filename: "Demo.S01E01.WEB.H264.nzb", want: "Demo.S01E01.WEB.H264"},
		{name: "already normalized release", filename: "Demo.S01E01.WEB.H264", want: "Demo.S01E01.WEB.H264"},
		{name: "uppercase extension", filename: "Demo.S01E01.H264.NZB", want: "Demo.S01E01.H264"},
		{name: "only final NZB suffix", filename: "Demo.nzb.Release.nzb", want: "Demo.nzb.Release"},
		{name: "canonical name can end in NZB", filename: "Demo.nzb.nzb", want: "Demo.nzb"},
		{name: "media suffix is part of supplied title", filename: "Demo.S01E01.mkv", want: "Demo.S01E01.mkv"},
		{name: "invalid filename falls back", filename: "???.nzb", meta: map[string]string{"Name": "meta-release"}, want: "meta-release"},
		{name: "reserved filename falls back", filename: "CON.nzb", meta: map[string]string{"title": "safe-title"}, want: "safe-title"},
		{name: "all invalid", filename: ".nzb", meta: map[string]string{"Name": "..", "title": "***"}, want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := determineNZBName(test.filename, test.meta); got != test.want {
				t.Fatalf("determineNZBName() = %q, want %q", got, test.want)
			}
		})
	}
}
