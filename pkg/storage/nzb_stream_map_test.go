package storage

import (
	"errors"
	"math"
	"testing"
)

func TestValidateNZBStreamMap(t *testing.T) {
	dense := []NZBSegment{
		{MessageID: "head", Bytes: 16, StartOffset: 0, EndOffset: 15},
		{MessageID: "tail", Bytes: 16, StartOffset: 16, EndOffset: 31},
	}
	implicit := []NZBSegment{{MessageID: "head", Bytes: 16}, {MessageID: "tail", Bytes: 16}}
	sliced := []NZBSegment{
		{MessageID: "archive", Bytes: 16, EndOffset: 15, SegmentDataStart: 10, Source: &NZBArticleGeometry{Size: 64, Offset: 32, Bytes: 32, Part: 2, Total: 2}},
	}
	copyWith := func(i int, change func(*NZBSegment)) []NZBSegment {
		result := append([]NZBSegment(nil), dense...)
		change(&result[i])
		return result
	}
	for _, tt := range []struct {
		name       string
		size       int64
		encrypted  bool
		requireSrc bool
		segments   []NZBSegment
		valid      bool
	}{
		{"dense legacy", 32, false, false, dense, true},
		{"offsetless complete legacy", 32, false, false, implicit, true},
		{"offsetless short legacy", 33, false, false, implicit, false},
		{"offsetless long legacy", 31, false, false, implicit, false},
		{"strict import requires source", 32, false, true, dense, false},
		{"valid sliced member", 16, false, true, sliced, true},
		{"encrypted padded member", 17, true, false, dense, true},
		{"encrypted short ciphertext", 17, true, false, dense[:1], false},
		{"size overflow", math.MaxInt64, true, false, dense, false},
		{"explicit missing middle segment", 48, false, false, copyWith(1, func(s *NZBSegment) { s.StartOffset, s.EndOffset = 32, 47 }), false},
		{"explicit gap despite matching byte sum", 32, false, false, copyWith(1, func(s *NZBSegment) { s.StartOffset, s.EndOffset = 17, 32 }), false},
		{"overlap despite matching byte sum", 32, false, false, copyWith(1, func(s *NZBSegment) { s.StartOffset, s.EndOffset = 15, 30 }), false},
		{"unspecified length", 32, false, false, copyWith(0, func(s *NZBSegment) { s.Bytes = 0 }), false},
		{"missing ID", 32, false, false, copyWith(0, func(s *NZBSegment) { s.MessageID = "" }), false},
		{"invalid source slice", 32, false, false, copyWith(0, func(s *NZBSegment) {
			s.Source = &NZBArticleGeometry{Size: 16, Bytes: 16, Part: 1, Total: 1}
			s.SegmentDataStart = 1
		}), false},
		{"slice overflow", 32, false, false, copyWith(0, func(s *NZBSegment) { s.SegmentDataStart = math.MaxInt64 }), false},
		{"empty map", 32, false, false, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := &NZBFile{Size: tt.size, IsEncrypted: tt.encrypted, Segments: tt.segments}
			err := ValidateNZBStreamMap(file, tt.requireSrc)
			if (tt.valid && err != nil) || (!tt.valid && !errors.Is(err, ErrInvalidNZBMap)) {
				t.Fatalf("validate = %v; valid=%v", err, tt.valid)
			}
		})
	}
}
