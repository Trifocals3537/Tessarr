package parser

import (
	"context"
	"fmt"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

type fileGroupResult struct {
	files []*storage.NZBFile
	err   error
}

// Only CRC-validated single volumes that reach a known incomplete archive
// boundary can request peers. Preserve their original error text and cause.
type rarIncompleteVolumeError struct {
	cause   error
	version RARVersion
}

func (e *rarIncompleteVolumeError) Error() string { return e.cause.Error() }
func (e *rarIncompleteVolumeError) Unwrap() error { return e.cause }

func incompleteRARVersion(err error) RARVersion {
	for depth := 0; err != nil && depth < 16; depth++ {
		if incomplete, ok := err.(*rarIncompleteVolumeError); ok {
			return incomplete.version
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = wrapper.Unwrap()
	}
	return RARVersionUnknown
}

// Keep every independently successful archive. A failed group is consumed only
// when one same-format assembly attempt passes the normal archive parser. This
// avoids guessing that every one-entry group is an obfuscated multipart set.
func (p *NZBParser) assembleIncompleteRARGroups(ctx context.Context, groups []FileGroup, results []fileGroupResult, password string) []error {
	var failures []error
	for _, version := range []RARVersion{RARVersion4, RARVersion5} {
		var candidates []int
		for index, result := range results {
			if groups[index].Type == storage.NZBFileTypeRar && len(groups[index].Files) == 1 && incompleteRARVersion(result.err) == version {
				candidates = append(candidates, index)
			}
		}
		if len(candidates) < 2 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return append(failures, err)
		}
		merged, err := incompleteRARGroup(groups, candidates, version)
		var files []*storage.NZBFile
		if err == nil {
			files, err = p.processFileGroup(ctx, merged, password)
			if err == nil && len(files) == 0 {
				err = fmt.Errorf("assembled RAR groups produced no files")
			}
		}
		if err != nil {
			err = fmt.Errorf("failed to assemble incomplete RAR%d volume groups: %w", version, err)
			p.logger.Warn().Err(err).Int("volume_groups", len(candidates)).Msg("RAR volume assembly failed")
			failures = append(failures, err)
			continue
		}
		for _, index := range candidates {
			results[index] = fileGroupResult{}
		}
		results[candidates[0]] = fileGroupResult{files: files}
		p.logger.Info().Int("rar_version", int(version)).Int("volume_groups", len(candidates)).Msg("Validated incomplete RAR volume groups as an archive")
	}
	return failures
}

func incompleteRARGroup(groups []FileGroup, candidates []int, version RARVersion) (*FileGroup, error) {
	base := &groups[candidates[0]]
	merged := &FileGroup{
		BaseName: base.BaseName, ActualFilename: base.ActualFilename, Type: storage.NZBFileTypeRar,
		Files: make([]nzbparser.NzbFile, 0, len(candidates)), Groups: make(map[string]struct{}),
	}
	seenOrder := make(map[[2]int]struct{})
	for _, index := range candidates {
		group := &groups[index]
		file := group.Files[0]
		if version == RARVersion4 {
			order := [2]int{getRARVolumeOrder(file.Filename), file.Number}
			if _, duplicate := seenOrder[order]; duplicate {
				return nil, fmt.Errorf("ambiguous RAR4 volume order across file groups")
			}
			seenOrder[order] = struct{}{}
		}
		// The retry owns its mutable file/segment slices; it cannot reorder the
		// independently processed groups or reuse their geometry metadata.
		file.Segments = append(nzbparser.NzbSegments(nil), file.Segments...)
		merged.Files = append(merged.Files, file)
		for name := range group.Groups {
			merged.Groups[name] = struct{}{}
		}
	}
	return merged, nil
}
