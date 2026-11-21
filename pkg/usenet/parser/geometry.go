package parser

import (
	"context"
	"fmt"
	"sort"

	"github.com/Tensai75/nzbparser"
	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/internal/utils"
	"github.com/Trifocals3537/tessarr/pkg/storage"
	"github.com/sourcegraph/conc/iter"
)

// analyzeFileGeometry verifies both ends of each independently posted file.
// Subject counters and encoded NZB byte counts are not decoded offsets. Names
// may be randomized per article, so they are not an integrity identifier either.
// This validates the fixed-part layout used by the offset map; it is not a full
// article-availability audit. Readers must also validate every fetched body.
func (p *NZBParser) analyzeFileGeometry(ctx context.Context, group *FileGroup) error {
	type result struct {
		meta filePartMeta
		name string
		err  error
	}
	mapper := iter.Mapper[nzbparser.NzbFile, result]{MaxGoroutines: max(1, p.maxConcurrent)}
	results := mapper.Map(group.Files, func(file *nzbparser.NzbFile) result {
		if err := sortAndValidateSegments(file.Segments); err != nil {
			return result{err: err}
		}
		fetch := func(id string) (*nntp.YencMetadata, error) {
			var meta *nntp.YencMetadata
			err := p.manager.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
				var err error
				meta, err = conn.GetHeaderPrefix(id, metadataOnly)
				return err
			})
			return meta, err
		}
		first, err := fetch(file.Segments[0].Id)
		if err != nil {
			return result{err: fmt.Errorf("first article: %w", err)}
		}
		if first == nil || first.Begin != 1 || first.Size <= 0 || first.End < 1 || first.End > first.Size || first.Part > 1 || first.Part < 0 {
			return result{err: fmt.Errorf("invalid yEnc geometry: first article is not the beginning of a complete file")}
		}
		count := int64(len(file.Segments))
		// Division avoids multiplying untrusted sizes until they are bounded.
		if (first.Size-1)/first.End+1 != count || (first.Total > 0 && first.Total != count) {
			return result{err: fmt.Errorf("invalid yEnc geometry: NZB segment count does not cover declared file size")}
		}
		last := first
		if count > 1 {
			last, err = fetch(file.Segments[len(file.Segments)-1].Id)
			if err != nil {
				return result{err: fmt.Errorf("last article: %w", err)}
			}
		}
		if last == nil || last.Size != first.Size || last.Begin != (count-1)*first.End+1 || last.End != first.Size ||
			(last.Part != 0 && last.Part != count) || (last.Total > 0 && last.Total != count) {
			return result{err: fmt.Errorf("invalid yEnc geometry: final article does not match the expected file boundary")}
		}
		return result{meta: filePartMeta{fileSize: first.Size, segmentSize: first.End, partNumber: first.Part, partBegin: first.Begin}, name: first.Name}
	})
	group.fileMeta = make(map[string]filePartMeta, len(results))
	for i, r := range results {
		if r.err != nil {
			return fmt.Errorf("file %q: %w", group.Files[i].Filename, r.err)
		}
		key := fileMetaKey(group.Files[i])
		if _, exists := group.fileMeta[key]; exists {
			return fmt.Errorf("duplicate NZB file articles")
		}
		group.fileMeta[key] = r.meta
		if len(group.Files) == 1 && group.Type == storage.NZBFileTypeMedia {
			if name := utils.RemoveInvalidChars(r.name); name != "" {
				group.ActualFilename = name
			}
		}
	}
	return nil
}

// Keep the layout validation strict, but report the failing condition without
// disclosing article message IDs (which may identify provider content).
func sortAndValidateSegments(segments nzbparser.NzbSegments) error {
	if len(segments) == 0 {
		return fmt.Errorf("invalid NZB segment numbering: no segments")
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Number < segments[j].Number })
	base := segments[0].Number
	if base != 0 && base != 1 {
		return fmt.Errorf("invalid NZB segment numbering: first segment is %d, expected 0 or 1", base)
	}
	ids := make(map[string]struct{}, len(segments))
	for i, s := range segments {
		if i > 0 && s.Number == segments[i-1].Number {
			return fmt.Errorf("invalid NZB segment numbering: duplicate segment number %d", s.Number)
		}
		if s.Number != base+i {
			return fmt.Errorf("invalid NZB segment numbering: expected segment %d, found %d", base+i, s.Number)
		}
		if s.Id == "" {
			return fmt.Errorf("invalid NZB segment numbering: empty message ID for segment %d", s.Number)
		}
		if _, exists := ids[s.Id]; exists {
			return fmt.Errorf("invalid NZB segment numbering: duplicate message ID for segment %d", s.Number)
		}
		ids[s.Id] = struct{}{}
	}
	return nil
}
