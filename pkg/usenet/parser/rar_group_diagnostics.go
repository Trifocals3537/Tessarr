package parser

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/Trifocals3537/tessarr/pkg/storage"
)

const (
	maxRARGroupDiagnostics = 3
	maxRARDiagnosticGroups = 64
	maxRARDiagnosticParts  = 256
	maxRARDiagnosticName   = 512
)

// This evidence describes a rejected group, not a verified archive identity.
// Keep the original error text and cause; never derive evidence from that text.
type rarBoundaryError struct {
	cause        error
	version      RARVersion
	missingFirst bool
	member       rarBoundaryMember
}

type rarBoundaryMember struct {
	name      string
	size      int64
	stored    bool
	encrypted bool
}

func (e *rarBoundaryError) Error() string { return e.cause.Error() }
func (e *rarBoundaryError) Unwrap() error { return e.cause }

func newRARBoundaryError(cause error, file *RARFileEntry, missingFirst bool) *rarBoundaryError {
	return &rarBoundaryError{cause: cause, missingFirst: missingFirst, member: rarBoundaryMember{
		name: file.Name, size: file.UncompressedSize, stored: file.IsStored, encrypted: file.IsEncrypted,
	}}
}

func rarBoundaryFailure(err error) *rarBoundaryError {
	for depth := 0; err != nil && depth < 16; depth++ {
		if boundary, ok := err.(*rarBoundaryError); ok {
			return boundary
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = wrapper.Unwrap()
	}
	return nil
}

// The legacy member aggregator stops at an orphan continuation. Inspect the
// rest of that rejected group before treating the early error as evidence.
func rar4GroupBoundaryFailure(boundary *rarBoundaryError, members [][]*RARFileEntry) error {
	if len(members) == 0 || len(members) > maxRARDiagnosticParts {
		return boundary.cause
	}
	order := make([]int, len(members))
	entries := 0
	for index, files := range members {
		if len(files) > maxRARDiagnosticParts-entries {
			return boundary.cause
		}
		entries += len(files)
		order[index] = index
	}
	boundaryIndex := len(members) - 1
	if boundary.missingFirst {
		boundaryIndex = 0
	}
	target := rarDiagnosticBoundaryMember(members[boundaryIndex], boundary.missingFirst)
	if target == nil || target.Name != boundary.member.name || !rarDiagnosticMemberChain(order, members, target, boundary.missingFirst) {
		return boundary.cause
	}
	boundary.version = RARVersion4
	return boundary
}

// RAR5 rejects incomplete embedded index sets before member aggregation. Only
// attach boundary evidence for a contiguous prefix or suffix with consistent
// end markers and member metadata. Duplicate/gapped/conflicting sets keep their
// original error without supplying a cross-group hint.
func rar5GroupBoundaryFailure(cause error, infos []*rar5VolumeInfo, members [][]*RARFileEntry) error {
	if len(infos) == 0 || len(infos) > maxRARDiagnosticParts || len(members) != len(infos) {
		return cause
	}
	order := make([]int, len(infos))
	entries := 0
	for index, info := range infos {
		if info == nil || !info.multi {
			return cause
		}
		if len(members[index]) > maxRARDiagnosticParts-entries {
			return cause
		}
		entries += len(members[index])
		order[index] = index
	}
	sort.Slice(order, func(i, j int) bool { return infos[order[i]].number < infos[order[j]].number })
	for index := 1; index < len(order); index++ {
		previous, current := infos[order[index-1]], infos[order[index]]
		if current.number <= previous.number || current.number-previous.number != 1 || !previous.next {
			return cause
		}
	}
	first, last := infos[order[0]], infos[order[len(order)-1]]
	missingFirst := first.number > 0
	if missingFirst == last.next { // Either both boundaries are absent, or neither is.
		return cause
	}
	boundaryIndex := order[len(order)-1]
	if missingFirst {
		boundaryIndex = order[0]
	}
	target := rarDiagnosticBoundaryMember(members[boundaryIndex], missingFirst)
	if target == nil || !rarDiagnosticMemberChain(order, members, target, missingFirst) {
		return cause
	}
	boundary := newRARBoundaryError(cause, target, missingFirst)
	boundary.version = RARVersion5
	return boundary
}

func rarDiagnosticBoundaryMember(files []*RARFileEntry, missingFirst bool) *RARFileEntry {
	var target *RARFileEntry
	for _, file := range files {
		if file == nil || file.IsDirectory || (missingFirst && !file.SplitBefore) || (!missingFirst && !file.SplitAfter) {
			continue
		}
		if target != nil { // Multiple boundary members are ambiguous.
			return nil
		}
		target = file
	}
	return target
}

func rarDiagnosticMemberChain(order []int, members [][]*RARFileEntry, target *RARFileEntry, missingFirst bool) bool {
	// IsStored=false does not retain the actual compressed method. Only stored
	// members have a known method that can be compared within/between groups.
	if !target.IsStored || target.Name == "" || len(target.Name) > maxRARDiagnosticName || target.UncompressedSize <= 0 {
		return false
	}
	previous := -1
	var after bool
	var packed int64
	for index, original := range order {
		for _, file := range members[original] {
			if file == nil || file.Name != target.Name {
				continue
			}
			if file.IsDirectory || file.UncompressedSize != target.UncompressedSize || file.IsStored != target.IsStored ||
				file.IsEncrypted != target.IsEncrypted || file.PackedSize < 0 || file.PackedSize > math.MaxInt64-packed {
				return false
			}
			if previous < 0 {
				if file.SplitBefore != missingFirst || (missingFirst && index != 0) {
					return false
				}
			} else if index != previous+1 || !after || !file.SplitBefore {
				return false
			}
			previous, after = index, file.SplitAfter
			packed += file.PackedSize
		}
	}
	return previous >= 0 && after != missingFirst && (missingFirst || previous == len(order)-1) &&
		(!target.IsStored || target.IsEncrypted || packed < target.UncompressedSize)
}

type rarGroupFailure struct {
	index int
	group *FileGroup
	err   error
}

// Call only after every group has been processed. No additional provider reads,
// grouping mutations, or admission decisions are made by this diagnostic.
func rarGroupDiagnostics(failures []rarGroupFailure) []error {
	type candidate struct {
		rarGroupFailure
		boundary *rarBoundaryError
		key      string
		pattern  string
	}
	var candidates []candidate
	for _, failure := range failures[:min(len(failures), maxRARDiagnosticGroups)] {
		group := failure.group
		if group == nil || group.Type != storage.NZBFileTypeRar || len(group.Files) == 0 || len(group.Files) > maxRARDiagnosticParts {
			continue
		}
		boundary := rarBoundaryFailure(failure.err)
		if boundary == nil || (boundary.version != RARVersion4 && boundary.version != RARVersion5) ||
			!boundary.member.stored || boundary.member.name == "" || len(boundary.member.name) > maxRARDiagnosticName || boundary.member.size <= 0 {
			continue
		}
		key := rarDiagnosticBase(group.BaseName, true)
		pattern := rarDiagnosticPattern(group)
		if key != "" && pattern != "" {
			candidates = append(candidates, candidate{failure, boundary, key, pattern})
		}
	}
	var hints []error
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			if a.key != b.key || a.group.BaseName == b.group.BaseName || a.boundary.version != b.boundary.version ||
				a.boundary.member != b.boundary.member || a.boundary.missingFirst == b.boundary.missingFirst {
				continue
			}
			variation := "whitespace/separators"
			if rarDiagnosticBase(a.group.BaseName, false) == rarDiagnosticBase(b.group.BaseName, false) {
				variation = "whitespace"
			}
			hints = append(hints, fmt.Errorf("possible RAR chain split across filename groups: group %d (%d %s, %s) and group %d (%d %s, %s); basenames differ by %s and member boundaries are complementary; archive identity and completeness unverified; groups kept separate",
				a.index, len(a.group.Files), rarVolumeNoun(len(a.group.Files)), a.pattern,
				b.index, len(b.group.Files), rarVolumeNoun(len(b.group.Files)), b.pattern, variation))
			if len(hints) == maxRARGroupDiagnostics {
				return hints
			}
		}
	}
	return hints
}

func rarDiagnosticBase(name string, separators bool) string {
	if len(name) > maxRARDiagnosticName {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || (separators && (r == '.' || r == '-' || r == '_')) {
			return -1
		}
		return r
	}, name)
}

func rarDiagnosticPattern(group *FileGroup) string {
	pattern := ""
	for _, file := range group.Files {
		if len(file.Filename) > maxRARDiagnosticName {
			return ""
		}
		name := strings.ToLower(file.Filename)
		current := "other"
		switch {
		case rarVolumePattern.MatchString(name):
			current = ".partNN.rar"
		case rarPartPattern.MatchString(name):
			current = ".rNN"
		case rarMainPattern.MatchString(name):
			current = ".rar"
		}
		if pattern == "" {
			pattern = current
		} else if pattern != current {
			pattern = "mixed"
		}
	}
	return pattern
}

func rarVolumeNoun(count int) string {
	if count == 1 {
		return "volume"
	}
	return "volumes"
}
