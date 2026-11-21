package parser

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/safepath"
	"github.com/Trifocals3537/tessarr/internal/utils"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/Trifocals3537/tessarr/pkg/usenet/types"
)

func wrapNZBFile(f *storage.NZBFile) ([]*storage.NZBFile, error) {
	if f == nil {
		return nil, fmt.Errorf("nzb file is nil")
	}
	return []*storage.NZBFile{f}, nil
}

// fileMetaKey returns a stable key for associating per-file metadata.
func fileMetaKey(file nzbparser.NzbFile) string {
	if len(file.Segments) > 0 {
		return "m:" + file.Segments[0].Id
	}
	if file.Subject != "" {
		return "s:" + file.Subject
	}
	return ""
}

func getGroupsList(groups map[string]struct{}) []string {
	result := make([]string, 0, len(groups))
	for g := range groups {
		result = append(result, g)
	}
	return result
}

func determineNZBName(filename string, meta map[string]string) string {
	// A supplied release title can already be extensionless and contain dots.
	// Only an actual NZB suffix identifies a transport filename to normalize.
	if ext := filepath.Ext(filename); strings.EqualFold(ext, ".nzb") {
		filename = strings.TrimSuffix(filename, ext)
	}
	candidates := []string{
		filename,
		meta["Name"],
		meta["title"],
	}
	for _, candidate := range candidates {
		cleaned := utils.RemoveInvalidChars(candidate)
		if safepath.ValidateIdentifier(cleaned) == nil {
			return cleaned
		}
	}
	return ""
}

func determineExtension(group *FileGroup) string {
	// Try to determine extension from filenames
	for _, file := range group.Files {
		ext := filepath.Ext(file.Filename)
		if ext != "" {
			return ext
		}
	}
	return ""
}

func getNZBSegments(index int, file nzbparser.NzbFile, group *FileGroup) (int64, []storage.NZBSegment) {
	if err := sortAndValidateSegments(file.Segments); err != nil {
		return 0, nil
	}
	var fileSize, segmentSize int64
	if meta, ok := group.fileMeta[fileMetaKey(file)]; ok {
		fileSize, segmentSize = meta.fileSize, meta.segmentSize
	} else if group.metadata != nil {
		// Explicit metadata is used by archive/unit-test callers. Never estimate
		// decoded byte geometry from the encoded NZB byte counts.
		fileSize, segmentSize = group.metadata.fileSize, group.metadata.segmentSize
		if index == len(group.Files)-1 {
			fileSize = group.metadata.lastFileSize
		}
	}
	if fileSize <= 0 || segmentSize <= 0 || (fileSize-1)/segmentSize+1 != int64(len(file.Segments)) {
		return 0, nil
	}
	segments := make([]storage.NZBSegment, 0, len(file.Segments))
	// One backing allocation for immutable provenance, retained through slices.
	sources := make([]storage.NZBArticleGeometry, len(file.Segments))
	var offset int64
	for i, s := range file.Segments {
		size := min(segmentSize, fileSize-offset)
		sources[i] = storage.NZBArticleGeometry{Size: fileSize, Offset: offset, Bytes: size, Part: int64(i + 1), Total: int64(len(file.Segments))}
		segments = append(segments, storage.NZBSegment{Number: s.Number, MessageID: s.Id, Bytes: size, StartOffset: offset, EndOffset: offset + size - 1, Group: group.BaseName, Source: &sources[i]})
		offset += size
	}
	return offset, segments
}

func buildBaseSegments(group *FileGroup) ([]storage.NZBSegment, []storage.ArchiveVolumeInfo, int64) {
	if len(group.Files) == 0 {
		return nil, nil, 0
	}

	baseSegments := make([]storage.NZBSegment, 0)
	volumeInfos := make([]storage.ArchiveVolumeInfo, 0, len(group.Files))
	currentOffset := int64(0)

	for idx, nzbFile := range group.Files {
		totalSize, segments := getNZBSegments(idx, nzbFile, group)
		if totalSize == 0 || len(segments) == 0 {
			continue
		}
		start := len(baseSegments)
		baseSegments = append(baseSegments, segments...)
		volumeInfos = append(volumeInfos, storage.ArchiveVolumeInfo{
			Name:         nzbFile.Filename,
			Size:         totalSize,
			SegmentStart: start,
			SegmentEnd:   len(baseSegments),
		})
		currentOffset += totalSize
	}

	return baseSegments, volumeInfos, currentOffset
}

func buildArchiveVolumeDescriptors(group *FileGroup) []*types.Volume {
	var volumes []*types.Volume

	if len(group.Files) == 0 {
		return volumes
	}

	for idx, nzbFile := range group.Files {
		if len(nzbFile.Segments) == 0 {
			continue
		}

		totalSize, volumeSegments := getNZBSegments(idx, nzbFile, group)
		if totalSize == 0 || len(volumeSegments) == 0 {
			continue
		}

		volumeName := nzbFile.Filename
		if volumeName == "" {
			volumeName = fmt.Sprintf("%s.part%03d", group.BaseName, idx+1)
		}

		volumes = append(volumes, &types.Volume{
			Index:    idx,
			Name:     volumeName,
			Size:     totalSize,
			Segments: volumeSegments,
		})
	}

	return volumes
}

func buildExtractedArchiveFiles(
	group *FileGroup,
	password string,
	fileType storage.NZBFileType,
	baseSegments []storage.NZBSegment,
	volumeInfos []storage.ArchiveVolumeInfo,
	infos []*storage.ExtractedFileInfo,
) []*storage.NZBFile {
	if len(baseSegments) == 0 {
		return nil
	}
	files := make([]*storage.NZBFile, 0, len(infos))

	for _, info := range infos {
		if info == nil || info.FileSize <= 0 {
			continue
		}
		if info.InternalPath == "" {
			info.InternalPath = NormalizeArchivePath(info.FileName)
		}
		name := info.FileName
		if name == "" {
			name = group.BaseName
		}
		name = utils.RemoveInvalidChars(name)

		// Use pre-sliced segments if available, otherwise slice based on offset
		var segments []storage.NZBSegment
		if len(info.Segments) > 0 {
			// Use pre-computed segments (from RAR parser, etc.)
			segments = info.Segments
		} else if info.DataOffset > 0 || info.FileSize > 0 {
			// Slice segments for this file's byte range
			sliced, err := sliceSegmentsForRangeSimple(baseSegments, info.DataOffset, info.FileSize)
			if err != nil || len(sliced) == 0 {
				// Fallback to all segments if slicing fails
				segments = make([]storage.NZBSegment, len(baseSegments))
				copy(segments, baseSegments)
			} else {
				segments = sliced
			}
		} else {
			// No offset info, use all segments
			segments = make([]storage.NZBSegment, len(baseSegments))
			copy(segments, baseSegments)
		}

		files = append(files, &storage.NZBFile{
			Name:         name,
			InternalPath: info.InternalPath,
			Groups:       getGroupsList(group.Groups),
			Segments:     segments,
			Password:     password,
			FileType:     fileType,
			Size:         info.FileSize,
			IsStored:     info.IsStored,
		})
	}

	return files
}

func NormalizeArchivePath(name string) string {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.TrimLeft(trimmed, "./")
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	trimmed = path.Clean(trimmed)
	trimmed = strings.Trim(trimmed, "/")
	return trimmed
}
