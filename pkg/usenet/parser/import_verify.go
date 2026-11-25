package parser

import (
	"context"
	"fmt"

	"github.com/Trifocals3537/tessarr/internal/nntp"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

// verifyImportFile validates the logical map and actual sampled article bodies.
// STAT can succeed when BODY and ARTICLE both fail. Sampling is deliberately
// not a complete-availability guarantee; 100 percent opts into reading all
// articles. Each file is checked sequentially so probes do not fan out beyond
// the parser's existing processing connection budget.
func (p *NZBParser) verifyImportFile(ctx context.Context, file *storage.NZBFile, percent int) error {
	if err := storage.ValidateNZBStreamMap(file, true); err != nil {
		return err
	}
	count := len(file.Segments)
	samples := min(count, max(3, count*min(100, max(1, percent))/100))
	for i := 0; i < samples; i++ {
		index := 0
		if samples > 1 {
			index = (count - 1) * i / (samples - 1)
		}
		seg := file.Segments[index]
		if err := ctx.Err(); err != nil {
			return err
		}
		err := p.manager.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
			meta, err := conn.GetHeaderPrefix(seg.MessageID, 0)
			if err != nil {
				return err
			}
			if meta == nil || meta.PartSize != meta.DecodedSize || !seg.Source.Matches(meta.Size, meta.Begin, meta.End, meta.Part, meta.Total, meta.DecodedSize) {
				return &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, ArticleReason: nntp.ArticleInvalidGeometry, Message: "invalid yEnc geometry in import body sample"}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("import body sample %d of %q: %w", index+1, file.Name, err)
		}
	}
	return nil
}
